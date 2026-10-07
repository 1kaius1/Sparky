# SERVER.md - Sparky Central Server: Zero to Running

Companion to `CLAUDE.md` (full reference) and `ARCHITECTURE.md` (technical
design). This file is a different kind of document from both: a single,
linear, assume-nothing walkthrough for standing up `sparky-server` for the
first time, start to finish, ending at the point where the server is up,
you're logged in, and it's waiting for a node agent to dial in. It is not a
reference - every command here is also documented, with every option and
edge case, in `CLAUDE.md`'s Build and Run section; this doc exists to get
you through it once without having to decide anything you don't need to
decide yet.

This doc stops once the server itself is ready. Installing `sparky-agent` on
a GPU node (so it actually has something to connect) is a separate step,
covered in `docs/AGENT.md` - come back to that once this doc says you're
done.

---

## Before you start

You need:

- A Linux host with `systemd` - Debian/Ubuntu or RHEL/Fedora/Rocky/Alma.
  Root (`sudo`) access on it.
- Outbound internet access on that host, at least briefly (to pull a
  Postgres container image, unless you already have your own database).
- A free TCP port for the web UI - `8080` by default, configurable.

Two decisions this walkthrough makes for you, so you don't have to think
about them yet:

1. **Database**: this walkthrough has Sparky run its own Postgres for you,
   in a Podman container it manages. If you already have a separate
   Postgres server you want to use instead, skip ahead to "Already have
   your own Postgres?" below Step 4.
2. **Login**: this walkthrough uses Sparky's built-in local accounts, not
   Active Directory - no LDAP server needed. If you want AD/LDAP login
   from the start, see "Using Active Directory instead" near the end; it's
   a small addition on top of everything else here, not a different path.

---

## Step 1: Get the sparky-server package

If someone already handed you a built `.deb`, `.rpm`, or tarball, skip to
Step 2. Otherwise, build one yourself - there's no public release download
yet, so this is the normal path today:

```bash
# Needs: Go 1.26+, and nfpm (a build-time packaging tool, not a Go
# dependency - go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest)
git clone <this repo's URL>
cd Sparky
./scripts/build_packages.sh
ls dist/*.deb dist/*.rpm dist/*.tar.gz
```

Pick the artifact matching your host's distro family and CPU architecture
(`amd64` or `arm64`) - the rest of this doc shows `.deb`; swap in `.rpm` or
the tarball's own commands (shown in Step 2) if that's what you have.

---

## Step 2: Install it

```bash
sudo apt install ./sparky-server_<version>_<arch>.deb
```

(`.rpm`: `sudo dnf install ./sparky-server-<version>-1.<arch>.rpm`. Tarball:
`tar xzf sparky-server-<version>-linux-<arch>.tar.gz && cd
sparky-server-<version>-linux-<arch> && sudo ./install_server.sh`.)

This creates a `sparky` system account, writes
`/etc/sparky-server/secrets.env` (a template, not yet configured), and
enables the `sparky-server` systemd unit - but deliberately does not start
it yet. Confirm:

```bash
systemctl status sparky-server
```

You should see it listed as `enabled` but `inactive (dead)`. That's
expected - there's nothing configured yet for it to run against.

---

## Step 3: Fill in secrets.env

Open `/etc/sparky-server/secrets.env` in an editor (`sudo nano
/etc/sparky-server/secrets.env` or similar). For this walkthrough's
simplest path, you only need to touch three things:

1. **Leave `DATABASE_URL` alone for now** - Step 4 sets it for you.
2. **Generate a real `SESSION_SECRET`** - the placeholder value is not
   safe to run with:
   ```bash
   openssl rand -hex 32
   ```
   Paste the output in as `SESSION_SECRET=...`.
3. **Leave all five `LDAP_*` variables blank or commented out.** An unset
   group of `LDAP_*` variables means Sparky uses local-only accounts
   instead of Active Directory - the simplest path for a first run (see
   "Using Active Directory instead" near the end if you want AD now
   instead).

Everything else can stay at its default for a first run.

Then - this is the part that's easy to miss and that the next two scripts
both check for - find this line near the top of the file:

```
SPARKY_SETUP_UNACKNOWLEDGED=1
```

Change the `1` to `0` (or delete the line entirely). This is a deliberate
"did you actually look at this file" check: the setup scripts below refuse
to run against a `secrets.env` that still looks exactly like the raw
template, specifically to stop you from accidentally running real database
setup against placeholder values. If you forget this step, the next
command will stop and tell you exactly this.

---

## Step 4: Database

```bash
sudo /opt/sparky/share/sparky-server/sparky-server-db-setup.sh podman
```

This one command: generates a random Postgres password (never the same one
twice, never hardcoded), starts a persistent, systemd-managed Postgres
container (`sparky-local-postgres.service`, bound to `127.0.0.1` only - not
reachable from outside this host), writes the real `DATABASE_URL` into
`/etc/sparky-server/secrets.env` for you, and runs every database migration
- all using a `migrate` binary and the migration files already bundled into
the package. Nothing else to install, nothing else to configure.

It takes a little while the first time (pulling the Postgres container
image). When it finishes, confirm the database is actually healthy before
moving on:

```bash
sudo systemctl status sparky-local-postgres
sudo podman exec sparky-postgres pg_isready -U sparky
```

The first should say `active (running)`; the second should say `accepting
connections`. If either doesn't, don't proceed to Step 5 yet - see
Troubleshooting below.

**If the script refused to run and printed something about
`SPARKY_SETUP_UNACKNOWLEDGED`**, go back to Step 3 - you haven't disarmed
that sentinel yet.

**Already have your own Postgres?** Skip this script entirely. Set
`DATABASE_URL` in `secrets.env` to your own connection string instead, and
run migrations yourself - see `CLAUDE.md`'s Database Setup and Database
Migrations sections for the exact commands against your own server. Then
continue to Step 5 below.

---

## Step 5: First-run setup

This is the one interactive step - it sets the break-glass SuperAdmin
recovery password (a fallback credential independent of everything else,
rarely used day to day) and, the first time it runs, also creates a default
local `admin` account you'll actually sign in with.

```bash
sudo -u sparky /opt/sparky/share/sparky-server/run-with-secrets-env.sh /opt/sparky/bin/sparky-server setup
```

It will prompt you to set the break-glass password (typed input is hidden,
same as a normal `sudo` password prompt - type it twice to confirm). Once
that's done, it prints a **system-generated password for the `admin`
account, shown exactly once** - copy it somewhere safe right now. You'll
use it in Step 7.

---

## Step 6: Start the service

```bash
sudo systemctl start sparky-server
sudo systemctl status sparky-server
```

Confirm it says `active (running)`, not `failed`. If it crash-loops, double
check `DATABASE_URL` in `secrets.env` actually matches a reachable,
migrated database (Step 4 should have handled this correctly - see
Troubleshooting if not).

---

## Step 7: Log in

Open `http://<this-host's-address>:8080/` in a browser (adjust the port if
you changed `LISTEN_PORT`). Log in with username `admin` and the password
you copied in Step 5.

---

## Step 8: Register your first node

In the sidebar, go to **Nodes**, then **Register node**. Fill in a name,
hostname, IP address, the runtime backend that node will use
(`docker`/`podman`/`bare-metal` - see `CLAUDE.md` Nodes for what each
means), and its GPU/CPU memory. Submit.

The confirmation page shows a **bearer token, exactly once** - copy it now.
There's no way to see it again after you navigate away (only its hash is
ever stored); if you lose it, delete and re-register the node.

---

## Step 9: Point an agent at it

The server is now up, configured, and has a node registered - it's waiting
for that node's agent to actually connect and present the bearer token from
Step 8. That's the agent-side install, covered start to finish in
`docs/AGENT.md`. Once the agent connects, the Nodes page flips that node's
status to "online" - at that point, this walkthrough is done.

---

## Troubleshooting

- **"I started/stopped the Postgres container by hand outside the script
  and now nothing can connect."** The script-managed container's real
  password lives in `/etc/sparky-server/local-postgres.env`, separate from
  what's written into `secrets.env`'s `DATABASE_URL` - if you started a
  container yourself with different credentials, or a different container
  name, those two will disagree and every connection will fail with an
  authentication (not network) error. Check both files match, and check
  `podman ps -a` shows a container actually named `sparky-postgres` that's
  running, not just created. When in doubt, stop whatever you started by
  hand and re-run Step 4's script - it's safe to re-run.
- **`sparky-server-db-setup.sh` refuses immediately, mentioning
  `SPARKY_SETUP_UNACKNOWLEDGED`.** You haven't edited `secrets.env` and
  cleared that sentinel yet - back to Step 3.
- **The web UI returns `503 SETUP_REQUIRED` for every page.** Step 5
  (`sparky-server setup`) hasn't been run yet, or didn't complete.
- **Can't reach the web UI from another machine at all.** Check a host
  firewall isn't blocking `LISTEN_PORT` (default `8080`); the service
  itself listens on all interfaces by default.
- **`systemctl start sparky-server` fails or the process exits
  immediately.** `journalctl -u sparky-server -n 50` will show the real
  error - almost always a `secrets.env` value that's still wrong (a
  malformed `DATABASE_URL`, a partially-filled `LDAP_*` group - see
  `CLAUDE.md` Configuration for the "all five or none" rule).

---

## Using Active Directory instead

If you want AD/LDAP login instead of (or alongside) local accounts, fill in
all five `LDAP_SERVER_ADDR`/`LDAP_BIND_DN`/`LDAP_BIND_PASSWORD`/
`LDAP_BASE_DN`/`LDAP_ACCESS_GROUP_DN` variables in `secrets.env` before
Step 6 (restart the service if you add them after it's already running) -
see `CLAUDE.md`'s Configuration and Environment Variables for what each one
means. With all five set, the login page offers a choice between AD and
local accounts; the local `admin` account from Step 5 keeps working either
way.

---

## Where to go next

- `docs/AGENT.md` - install `sparky-agent` on a GPU node (Step 9 above).
- `CLAUDE.md` - the full reference this walkthrough is a shortcut through:
  every environment variable, every install option, RBAC tiers, API
  conventions, everything.
- `ARCHITECTURE.md` - the technical design behind all of it.
- `PLANNING.md` - current project status and roadmap.
