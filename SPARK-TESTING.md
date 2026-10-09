# Spark test pass - container lifecycle and related PRs

Written 2026-10-09 to run offline on the Spark workstations. It collects every
check that could not be done on the development workstation (no Docker, no GPU,
no real engine there; Podman was used and is noted where it covered a piece).
Nothing here needs the internet except pulling an image if you use `busybox`.

Covers: #150 (container names), #153 (model id = profile name, name rules),
#154 (docker group), #156 (Dead state), #157 (health resumes after an agent
restart), #161 (log archive on Unload), #162 (failed launches and
replace-on-launch), #163 (live logs and retention). Package version under test:
**0.2.11**.

Work top to bottom. Part A is the setup everything else needs. Each test has an
ID so you can fill in the results table at the end and tell me which ones failed.

---

## Part A - Install the new build (do this first)

### Why the order matters

Agent messages are decoded strictly, so a new field breaks an older receiver.
**Upgrade every agent first, then the server.**

- An older agent rejects `load_instance` (it gained `profile_id`) and the
  reconnect sweep (`check_instance` gained `port` and `engine_type`). A new server
  with an old agent leaves a load stuck in `starting`.
- An older agent ignores `fetch_logs`, so the live log page times out for it.
- A new agent with an older server is fine (it just never gets confirmations, so
  it keeps containers it would otherwise remove).

### A1. Build and install

On the dev workstation, from the repo at 0.2.11 (master after #163 merges):

```bash
cat VERSION                       # 0.2.11
scripts/build_packages.sh         # builds dist/ for amd64 and arm64
# note: it runs `go install ...migrate@latest`, which replaces your local migrate binary
```

Copy the arm64 agent package to each Spark and the server package to the server
host, then:

```bash
# on each Spark, one at a time
sudo apt install ./sparky-agent_0.2.11_arm64.deb
dpkg -l sparky-agent | tail -1                     # expect version 0.2.11
```

### A2. Docker group (PR #154) - on each docker-backend Spark

`/etc/sparky-agent/secrets.env` must have `SPARKY_RUNTIME_BACKEND=docker`.

```bash
sudo sparky-agent setup                            # re-run once; upgrades re-run it automatically afterwards
id serviceloop                                     # expect docker in the group list
sudo -u serviceloop docker ps                      # expect no permission error
sudo systemctl restart sparky-agent                # a group change only reaches new processes
journalctl -u sparky-agent -n 20 --no-pager        # connected, no socket permission errors
```

**Expect:** `serviceloop` is in the `docker` group and the agent reaches the
Docker socket. A bare-metal or podman node must NOT be added to the group (if you
have one, check `id serviceloop` there does not list docker).

Result: A2 ______

### A3. Server and migrations

```bash
sudo apt install ./sparky-server_0.2.11_amd64.deb
```

The database needs migrations 000038 (Dead state), 000039 (container log
archives) and 000040 (container log settings). Check where it is and apply:

```bash
# bundled copy that ships with the package (adjust if your layout differs)
sudo -u sparky /opt/sparky/share/sparky-server/run-with-secrets-env.sh sh -c \
  '/opt/sparky/share/sparky-server/migrate -path /opt/sparky/share/sparky-server/migrations -database "$DATABASE_URL" version'
# probably 37 before; run the same command with `up` instead of `version`, then `version` again: expect 40
```

If the bundled path is not there, use any `migrate` binary with the repo's
`migrations/` directory and your `DATABASE_URL`.

```bash
sudo systemctl restart sparky-server
sudo systemctl status sparky-server --no-pager     # active
```

Open the web UI and sign in. **Expect:** the sidebar shows **Container logs**
under Models (you are Admin), and Settings has a **Container logs** section
showing 12 months.

Result: A3 ______

### Handy: running SQL

Several checks read the database. Use whichever works on your server host:

```bash
# if psql is installed and DATABASE_URL is available
sudo -u sparky /opt/sparky/share/sparky-server/run-with-secrets-env.sh sh -c 'psql "$DATABASE_URL" -c "SELECT 1"'
# if the database is the packaged podman Postgres
sudo podman exec -it sparky-local-postgres psql -U sparky -d sparky
```

---

## Part B - Container names and labels (PR #150, PR #162)

You already confirmed the names look right. Check only what is new: the labels.

### B1. Name and labels on a running container

Load any profile that works (a small one is fine).

```bash
docker ps --format '{{.Names}}'                    # sparky-<profile>-<YYYYMMDD-HHMMSS>, time is UTC
docker inspect <container> --format '{{json .Config.Labels}}'
```

**Expect:** labels include `sparky.managed=true`, `sparky.instance_id=<uuid>` and
**`sparky.profile_id=<uuid>`** (new in 0.2.10). The profile id is the uuid in the
profile's Edit link (`/profiles/<id>/edit`). A profile name containing `/` or `:`
appears in the container name with those turned into `-`.

Result: B1 ______

---

## Part C - Model id is the profile name (PR #153)

Prerequisite: agents upgraded before the server (A1). Use a vLLM profile on a
Spark with a real model, and a llama.cpp profile if you have one.

### C1. llama.cpp `--alias` exists

```bash
docker run --rm ghcr.io/ggml-org/llama.cpp:server --help | grep -A1 -- --alias
# or, on a bare-metal node: llama-server --help | grep -A1 -- --alias
```

**Expect:** `--alias` is listed. If not, tell me - I will change the flag.

Result: C1 ______

### C2. vLLM serves the profile name as its model id

Create or use a vLLM profile whose name uses the allowed characters, for example
`team/Qwen3-8B:fp8` (letters, digits and `. _ - :`, `/` between parts, at most 64
characters, no spaces or commas). Load it and wait for `running`.

```bash
curl -s localhost:<port>/v1/models
```

**Expect:** `data[0].id` equals the exact profile name.

Result: C2 ______

### C3. Requests must use the name

```bash
curl -s localhost:<port>/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"<profile name>","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'
```

**Expect:** HTTP 200 with a reply. Repeat with the old local model path as
`model`: vLLM should answer 404 (expected; that is why clients must switch).

Result: C3 ______

### C4. llama.cpp profile (if available)

Load a llama.cpp profile and check `/v1/models` as in C2: the id should be the
profile name. Note whether a chat completion using the old path as `model` is
still accepted (llama.cpp may ignore the field).

Result: C4 ______

### C5. A saved `served_model_name` is ignored

Edit a vLLM profile's `engine_params` to include `"served_model_name":"old-name"`
(or use an older profile that has it). Load it.

**Expect:** it launches normally; `/v1/models` shows the profile name, not
`old-name`; the server log (`journalctl -u sparky-server`) has a line saying
`served_model_name` was ignored.

Result: C5 ______

### C6. A legacy name is refused at launch

Simulate a profile saved before the name rules existed. In SQL:

```sql
UPDATE model_profiles SET name = 'my model' WHERE id = '<profile id>';
```

Open the Profiles page and press Load.

**Expect:** the load is refused with a message saying the name is not a valid
model id and to rename the profile; **no** instance row is left behind
(`SELECT count(*) FROM running_instances WHERE profile_id = '<id>' AND status = 'starting'` is 0).
Rename it in the editor to a valid name and it loads.

Result: C6 ______

### C7. New names are validated

In the profile form, try a name with a space, a comma, a leading `-`, and the
name of an existing profile. Hover the `?` next to the name field.

**Expect:** each is rejected with a reason (the duplicate says another profile is
already named that); the `?` shows the naming guidance on hover and on keyboard
focus (Tab to it).

Result: C7 ______

### C8. Renaming a running profile

Rename a profile that is running.

**Expect:** the running instance's `/v1/models` id does not change until you
Unload and Load again.

Result: C8 ______

### C9. New agent, old server (optional)

With an 0.2.5+ agent and a pre-0.2.5 server, a load still reaches `running`. Only
do this if you have such a combination; it is not required.

Result: C9 ______

---

## Part D - Dead state (PR #156)

### D1. A killed container shows Dead

Load a profile; wait until it is `running` and `healthy`.

```bash
docker kill <container>
```

The health check runs every 60 seconds (`SPARKY_HEALTH_CHECK_INTERVAL_SECONDS`).
To see it immediately, `sudo systemctl restart sparky-agent` instead: the
reconnect sweep reports it.

**Expect:** on the Profiles page and the Dashboard the instance's Health shows
**dead on a red background**; its Instance status stays `running`; the Load
button is NOT offered (it still has an active instance).

Result: D1 ______

### D2. Unload clears a dead instance

Press Unload on the dead instance.

**Expect:** it becomes stopped without an error, the exited container is gone
(`docker ps -a`), and the profile can be loaded again. (With the archive from
#161 the Container logs page also gets an entry for it - see F1.)

Result: D2 ______

### D3. A container removed behind Sparky's back

Load again; then `docker rm -f <container>`.

**Expect:** Dead as in D1. Unload clears it with no error (there is nothing to
archive, so no new Container logs entry).

Result: D3 ______

---

## Part E - Health reporting resumes after an agent restart (PR #157)

### E1. Health keeps updating across an agent restart

Load a profile; wait for `running` and `healthy`.

```sql
SELECT last_health_check_at FROM running_instances WHERE status = 'running';
```

Note the value, then `sudo systemctl restart sparky-agent`. The container keeps
running (`docker ps`).

**Expect:** within a few seconds of the agent reconnecting,
`last_health_check_at` advances (the first check runs at once), and keeps
advancing about every minute after that.

Result: E1 ______

### E2. An engine that hangs after the restart is noticed

With the agent already restarted as in E1:

```bash
docker pause <container>        # the engine stops answering but the container is still "running"
```

**Expect:** within about a minute the instance's Health becomes **unhealthy**
(not frozen on healthy). `docker unpause <container>`, and it returns to healthy.

Result: E2 ______

### E3. A container started by an older agent (optional)

Containers started before 0.2.4 carry no labels. If one is running across the
upgrade, its health should also resume after the sweep, because the central app
sends the port and engine type. Only if you happen to have one.

Result: E3 ______

---

## Part F - Log archive on Unload (PR #161)

### F1. Unload saves the log and removes the container

Load a profile and send it a request or two so it has something to log. Press
Unload.

```bash
docker ps -a --format '{{.Names}}' | grep sparky-      # the profile's container is gone
```

**Expect:** the instance becomes stopped; **no** leftover container; the
**Container logs** page lists a new entry with the right node and profile, reason
"unloaded", state `exited`, exit code 143 (clean stop) or 137 (killed after the
grace period), and a line count.

Result: F1 ______

### F2. The saved log is the real one

Open the entry.

**Expect:** the text ends with the engine's shutdown messages; it matches what
`docker logs --tail 50` showed just before the Unload (do that comparison once);
the **Download the full text** link gives the same text as a file.

Result: F2 ______

### F3. No removal without a stored log

This proves the container is kept when the server does not confirm. Load a
profile, press Unload and **immediately** stop the server:

```bash
sudo systemctl stop sparky-server
```

(This is a race: the agent halts the container first and then uploads, so you are
interrupting the upload. If the archive finishes before the server stops, the
test proves nothing - retry with a profile that has logged more, or press Unload
and stop the server in one command line, for example
`sudo systemctl stop sparky-server` typed ready in a second terminal.)

Start the server again.

**Expect:** `docker ps -a` shows the exited `sparky-<profile>-...` container still
there, and `journalctl -u sparky-agent` has a line saying it is keeping the
stopped container because its log was not archived. Remove it by hand:
`docker rm <container>`.

Result: F3 ______

### F4. Dead instance is archived with its exit reason

Load, `docker kill <container>`, wait for Dead (D1), press Unload.

**Expect:** a Container logs entry appears with exit code 137 and the container is
removed. (If you can provoke a genuine out-of-memory kill, the entry should read
"killed for running out of memory"; optional.)

Result: F4 ______

### F5. Old server (optional)

New agent with an older server: Unload completes normally and the exited
container is left behind (expected; nothing confirms the archive).

Result: F5 ______

---

## Part G - Failed launches and replace-on-launch (PR #162)

### G1. A failed launch leaves nothing behind

Make a vLLM profile that cannot start, for example `engine_params`:

```json
{"max_model_len": 99999999}
```

Press Load.

**Expect:**
- the load ends as failed (it fails fast once vLLM exits);
- the Profiles page shows **Last run: Failed <time> - process/container exited
  before becoming ready** (or the engine's own first line) with a **View log** link;
- `docker ps -a` shows **no** leftover container for it;
- the Container logs page has an entry with reason "failed launch", exit code 1
  (or whatever vLLM exits with) and vLLM's real error text;
- the Load button is offered again.

Result: G1 ______

### G2. Replace-on-launch clears a profile's old containers

Take a profile that works. Make sure it is not loaded. Create a stale container
carrying its profile id, and a decoy carrying a different one:

```bash
PROFILE=<the profile uuid from its Edit link>
docker run -d --name stale-test  --label sparky.managed=true --label sparky.instance_id=stale-1 \
  --label sparky.profile_id=$PROFILE busybox sleep 1000
docker run -d --name stale-other --label sparky.managed=true --label sparky.instance_id=stale-2 \
  --label sparky.profile_id=00000000-0000-0000-0000-000000000000 busybox sleep 1000
```

(Use any image you already have with `--entrypoint sleep` if `busybox` cannot be
pulled.) Press Load on the profile.

**Expect:**
- `stale-test` is gone from `docker ps -a` (stopped and removed before the new
  container started);
- the Container logs page has an entry with reason "replaced" (its profile column
  shows `-` because `stale-1` is not a real instance id; that is expected);
- `stale-other` is **untouched** (still running);
- the profile loads normally.

Clean up: `docker rm -f stale-other`.

Result: G2 ______

### G3. A hung engine is stopped (optional)

If you can provoke an engine that starts but never answers, lower
`SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS` for the test (restart the agent after
editing `secrets.env`), then Load it.

**Expect:** after the timeout the load is reported failed, the container is
stopped and removed, GPU memory is released (`nvidia-smi`), and a "failed launch"
entry is saved. Put the timeout back afterwards.

Result: G3 ______

### G4. Old server (optional)

New agent, older server: a load works; a failed launch keeps its stopped
container (nothing confirms the archive).

Result: G4 ______

---

## Part H - Live logs and retention (PR #163)

### H1. Live view of a real container

Load a profile. On the Profiles page open the **Logs** link next to its status.

**Expect:** the page names the profile, says `State: running`, and shows the last
lines of the engine's output. Compare once with:

```bash
docker logs --tail 50 <container>
```

(stdout and stderr merged, so interleaving can differ slightly). Choose 100 and
1000 lines: the amount shown changes. Send the model a request, press **Refresh**:
new lines appear.

Result: H1 ______

### H2. Dashboard link

The Dashboard's running-instances row has the same **Logs** link and opens the
same page. Check a Read-only user does NOT see the Logs link or the Container
logs menu item, and gets Access denied at `/logs`.

Result: H2 ______

### H3. After Unload

Unload the instance, then open the same live URL again (back button or the link
you copied).

**Expect:** a red banner saying the instance has no container or process on the
node any more, and a link to its saved log, which opens.

Result: H3 ______

### H4. A dead but not-yet-unloaded instance can still be read

Load, `docker kill <container>`, wait for Dead (D1), open **Logs** before
pressing Unload.

**Expect:** the page still shows the log with `State: exited` and exit code 137
(the exited container is still there until Unload).

Result: H4 ______

### H5. Old agent (optional)

On a node whose agent has not been upgraded, the live page says the node did not
answer in time (expected).

Result: H5 ______

### H6. Retention setting and expiry

As Admin, Settings > Container logs: change the retention (try 6), save.

**Expect:** "Container log retention updated"; the **Audit log** page shows
`updated_container_log_retention` with who changed it. Values outside 1 to 24 are
refused with a reason.

Then check the expiry job. Set retention to 1, age one saved log, restart the
server, wait about a minute:

```sql
UPDATE container_log_archives SET created_at = now() - interval '40 days' WHERE id = '<archive id>';
```

```bash
sudo systemctl restart sparky-server
```

**Expect:** about a minute after the server starts that log is gone from the
Container logs page, and the Audit log shows `expired_container_logs` with **no
actor** (that means the system) and a detail of how many were deleted. Set the
retention back to 12 afterwards.

Result: H6 ______

---

## Part I - Quick regression pass

Things that should still work exactly as before; a minute each.

- **I1.** Load and Unload a profile normally; the Dashboard shows the instance
  while it runs, healthy, and the GPU strips update.
- **I2.** Inventory page: scan for unknown models and the Select all box still work.
- **I3.** The profile form saves a valid profile (create, edit, delete).
- **I4.** Agent journal after all of this has no repeating errors:
  `journalctl -u sparky-agent --since "1 hour ago" --no-pager | grep -i -E "error|warn" | tail`.

Result: I1 ____ I2 ____ I3 ____ I4 ____

---

## Part J - Clean up

```bash
docker ps -a --format '{{.Names}}' | grep -E 'stale-|sparky-'   # remove leftovers you do not want
docker rm -f stale-test stale-other 2>/dev/null
```

Put back: retention to 12 months, `SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS` to its
normal value (and restart the agent), any profile you renamed in SQL (C6) to a
valid name, and delete the deliberately broken profile from G1 if you no longer
need it.

---

## Results

| ID | Pass / Fail / Skipped | Notes |
|----|-----------------------|-------|
| A2 docker group | | |
| A3 migrations and server | | |
| B1 labels | | |
| C1 llama.cpp --alias | | |
| C2 vLLM model id | | |
| C3 request by name | | |
| C4 llama.cpp id | | |
| C5 ignored served_model_name | | |
| C6 legacy name refused | | |
| C7 name validation and hint | | |
| C8 rename while running | | |
| D1 dead shows red | | |
| D2 unload clears dead | | |
| D3 container removed by hand | | |
| E1 health across restart | | |
| E2 hung engine noticed | | |
| F1 unload archives | | |
| F2 saved log is real | | |
| F3 kept when unconfirmed | | |
| F4 dead instance archived | | |
| G1 failed launch clean | | |
| G2 replace-on-launch | | |
| H1 live view | | |
| H2 dashboard link and tiers | | |
| H3 after unload | | |
| H4 dead instance readable | | |
| H6 retention and expiry | | |
| I1-I4 regression | | |

## If something fails

Collect, and send me, for the failing test:

```bash
journalctl -u sparky-agent --since "15 min ago" --no-pager      # on the Spark
journalctl -u sparky-server --since "15 min ago" --no-pager     # on the server
docker ps -a ; docker inspect <container>                       # on the Spark
```

plus a screenshot of the page and the exact profile name and engine parameters
used. Useful database peeks:

```sql
SELECT id, status, health_status, error_message, last_health_check_at FROM running_instances ORDER BY started_at DESC LIMIT 5;
SELECT id, reason, container_state, exit_code, oom_killed, lines_kept, truncated, created_at FROM container_log_archives ORDER BY created_at DESC LIMIT 5;
SELECT action, actor_id, detail, created_at FROM audit_log ORDER BY created_at DESC LIMIT 10;
SELECT retention_months FROM container_log_settings;
```

## Known limits (not failures)

- A bare-metal node's saved and live logs hold only the last 16 KiB the agent
  kept in memory, and lose it when the agent restarts.
- Containers started before 0.2.10 have no profile label, so replace-on-launch
  cannot find them; nothing cleans those up yet (the scheduled and manual
  cleanup is still to be built).
- An interrupted log upload is not resumed; the container is simply kept.
- The audit log's own retention setting is displayed but not enforced; only the
  container log retention is.
