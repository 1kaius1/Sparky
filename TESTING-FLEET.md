# Testing: Two Independent (Unclustered) Sparks

Runbook for validating that Sparky works correctly with two DGX Spark nodes
registered and operated **independently** - no fabric group, no NCCL, no
shared model, nothing from v0.3.0's clustering milestone. This proves the
actual premise of the project ("a small fleet of GPU compute nodes," per
CLAUDE.md's Project Overview) for the first time with two real nodes
connected at once - every real-hardware pass so far, across every session,
has only ever had one Spark live at a time.

**Architecturally, this should already work** - the Nodes registry has no
cap, `fabric_group_id` is nullable and explicitly "null for any node
incapable of clustering" (SCHEMA.md), and a `single_node`-topology Model
profile targets exactly one node with no shared state between profiles.
Nothing here is expected to require a code change. The point of this pass
is to actually prove that with real hardware, and to record whatever real
finding results - including if something breaks, which is itself the
valuable outcome, not a failure of the plan.

---

## Before you do anything else

This repo's `CLAUDE.md`, `ARCHITECTURE.md`, `SCHEMA.md`, `docs/AGENT.md`,
and `PLANNING.md` all auto-load into context at the start of a session -
read them if you haven't internalized them yet, especially `PLANNING.md`'s
Decisions Log entries dated 2026-08-29 through 2026-08-31, which cover the
model-transfer-initiation UI, the load-readiness/health-check fix, and the
two most recent real-hardware confirmations. This document does not repeat
anything already established there - it only covers what isn't written
down anywhere else: real hardware inventory/access, and working norms
confirmed with the user this past week that aren't (yet) codified as hard
rules in `CLAUDE.md`.

**You are starting with zero conversation history.** Nothing from prior
sessions carries over except what's in this git repo. If anything below
turns out to be stale (an IP changed, a credential rotated, a described
behavior no longer matches the code), trust the live system and the repo
over this document, and update this document once you've confirmed the
real state - don't silently work around a stale assumption.

---

## Working norms for this project (confirmed with the user, not all yet in CLAUDE.md)

- **Auto-merge is authorized.** For this project, open a PR and merge it
  yourself right after opening (squash, delete branch) - don't wait for
  separate merge approval. This reverses what might read as the more
  cautious default; it's a confirmed, standing instruction for this repo.
- **One task per prompt.** Do the thing the user actually asked for in
  their message, then stop and report. Don't chain into the next unasked
  step, even an obviously-related one - let the user drive what happens
  next.
- **Every change still goes through a feature branch + PR**, including
  pure documentation/`PLANNING.md` updates from a real-hardware finding -
  this has been done consistently for every change this project has made,
  docs-only or not. Don't treat a `PLANNING.md`-only change as exempt from
  the branch/PR/merge flow.
- **Verify with real evidence, not assumption.** Every real-hardware claim
  in this project's `PLANNING.md` Decisions Log is backed by an actual
  command's actual output - a `docker inspect`, a real `curl` response, a
  real `nvidia-smi` reading - quoted or described precisely, not "should
  work" or "this looks right." Match that standard here. If a step can be
  verified two ways (e.g., a config's syntax via `nginx -t` in a
  disposable container, then the real behavior via a real request), do
  both rather than stopping at the first.
- **Confirm before anything destructive or hard-to-reverse on real
  hardware** - stopping or removing a real running container, deleting
  real files, anything that touches a production workload. Ask first,
  every time, even if a similar action was approved earlier in the same
  session - approval doesn't carry across contexts.
- **Record real findings in `PLANNING.md`'s Decisions Log**, matching the
  density and format already there (see any 2026-08 entry as a template) -
  Date | Decision | Rationale (with the actual evidence) | Alternatives
  Considered. A `CHANGELOG.md` entry too, if the change is user-facing or
  touches shipped behavior; skip it for a pure real-hardware confirmation
  with no code change (see the 2026-08-30/08-31 Driver/env-var entries for
  that exact distinction already made twice).
- **Tear down test infrastructure when a testing pass is done**, unless
  told to leave it running - test control-node servers, test model
  downloads, test profiles/instances. Leave real production workloads
  (anything not created by this testing pass) untouched.
- **Confirm the approach before a major change**, and present real
  trade-offs rather than deciding unilaterally when more than one
  reasonable path exists - this has been the norm throughout, not just a
  suggestion.

---

## Real hardware inventory

### Spark #1 - already known, previously used for real validation this project

- SSH: `developer@10.0.2.8` (confirm this key/access still works before
  relying on it - environments change; if it doesn't work, stop and ask
  the user rather than trying to work around it).
- Real hostname: `nmn-1984525004012.ad-dev.issgovernance.com`.
- Architecture: `aarch64` (ARM64) - `sparky-agent` needs
  `GOOS=linux GOARCH=arm64` cross-compiled builds.
- OS: Ubuntu 24.04.4.
- GPU: one NVIDIA GB10, ~128GB unified memory (`gpu_memory_gb` ==
  `cpu_memory_gb` when registering this node - see SCHEMA.md Nodes).
- `runtime_backend`: `docker` - this fleet's Sparks ship with Docker and
  stay on it deliberately (see PLANNING.md's Decisions Log); never
  configure `podman` or `bare-metal` for a real Spark node.
- **This node sometimes runs a real production vLLM workload** (observed
  this project using ~110GB+ of GPU memory under a container named like
  `vllm-qwen3-coder-30b-a3b-instruct-fp8-*`). Check
  `docker ps -a`/`nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv`
  before doing anything - if something real is running, get the user's
  explicit permission before stopping it, and never assume a prior
  session's permission still applies.
- Real test-only model storage used previously:
  `/home/developer/sparky-test-models` (owned by `developer:developer`,
  `0755`) - kept deliberately separate from the user's real model library
  at `/home/Models` (owned by `developer:models`, real production
  content - never touch this directory or its contents).

### Spark #2 - not yet known

No connection details for a second Spark exist anywhere in this repo or
this document. **Ask the user directly, at the start of this pass**, for:
its SSH access (host/user/key), its real architecture (confirm - don't
assume ARM64 just because Spark #1 is), and whether it currently runs
anything real that shouldn't be disturbed. Do not proceed past node
registration for this second node until you have real, confirmed answers,
not assumptions carried over from Spark #1.

### Control node

Use `scripts/dev-server.sh` (already in this repo, has its own complete
header doc-comment) - a disposable Postgres via podman, a local build of
`sparky-server`, and a throwaway break-glass password you choose at
prompt time. Do not use any real/production Postgres instance or a real
`.env` file for this testing pass. Confirm the control node has real
network reachability to both Sparks' agent-connect WebSocket endpoint and
HTTP ports before registering nodes - if it doesn't (e.g. a different
network segment than prior sessions assumed), that's a real environment
fact to establish first, not something to guess past.

---

## Test plan

Work through these in order; each step should be independently verified
with real command output before moving to the next, not assumed from the
step's own success message.

1. **Stand up the control node.** `scripts/dev-server.sh`, confirm it's
   reachable (`curl` the login page) from wherever you'll be issuing
   commands from.

2. **Register both Sparks as independent Nodes** via the real HTTP
   form (`POST /nodes/register`), not by inserting rows directly - this
   is the same path a real operator uses, and confirms the real handler
   works for a second node in the same run, not just the first. Capture
   each node's one-time-shown bearer token.

3. **Deploy and start `sparky-agent` on both nodes**, cross-compiled per
   each node's real confirmed architecture, pointed at the control node's
   real reachable address. Confirm **both** show `agent_status = online`
   in the Nodes page/DB **at the same time** - the actual point of this
   step, not just that each one connects when tested alone.

4. **Confirm telemetry from both nodes concurrently, correctly
   attributed.** Let both agents run for a few telemetry poll intervals,
   then check `metrics`/`gpu_metrics` rows for both `node_id`s - confirm
   readings are real (compare a GPU metrics row against a direct
   `nvidia-smi` query on that same node, matching the standard this
   project has already held telemetry to), distinct per node, and not
   cross-attributed to the wrong node.

5. **Load one real model profile per Spark, concurrently.** Use the real
   `/transfers/new` download-initiation form (built and verified this
   project - don't hand-place model files) to get a small model (e.g.
   `Qwen/Qwen2.5-0.5B-Instruct`, already proven fast and reliable in this
   project) onto each node's own test model storage path, into two
   separate profiles each targeting its own node. Load both. Given the
   real load-readiness check (`agent/connection.Conn.waitForReady`,
   PLANNING.md 2026-08-30), expect `running_instances.status` to sit at
   `starting` for real seconds before flipping to `running` once each
   engine genuinely proves it can generate - this is correct, expected
   behavior, not a hang; don't intervene early.

6. **Confirm both run correctly and independently at the same time.**
   Send a real chat completion to each node's instance and confirm a
   real, distinct, correct answer from each - not just that both report
   `running`. Let the periodic health check (once a minute by default)
   tick at least once for both and confirm `health_status`/
   `health_detail` populate correctly and independently per instance.

7. **Confirm the dashboard/SSE layer handles two nodes' concurrent
   events sanely** - watch `/events` (or the rendered dashboard) while
   both are mid-load, and note whether events from the two nodes
   interleave correctly with no cross-node bleed in what's displayed.

8. **Unload both instances**, confirm clean, fully independent teardown
   on each node (container removed, GPU memory released - verified via
   `nvidia-smi` on each node, not assumed from the HTTP response alone).

9. **Write up the real finding** in `PLANNING.md`'s Decisions Log,
   whatever it turns out to be - a clean pass confirming the
   architecture's own claim, or a real bug this pass surfaced (matching
   this project's own repeated experience: several "should just work"
   real-hardware passes this project has run turned up genuine bugs
   nobody predicted - approach this the same way, not as a formality).
   Update the relevant `ARCHITECTURE.md`/`SCHEMA.md`/Known Issues framing
   if the finding changes what's actually true there.

10. **Tear down** the test control node, both agents, and any test model
    profiles/instances/downloaded model files on both Sparks - leave
    both nodes exactly as clean as you found them, same as every other
    real-hardware pass this project has done.

---

## What not to do

- Don't touch anything on either Spark that you didn't create for this
  test, without explicit permission - especially a real running
  container or the real `/home/Models` library on Spark #1.
- Don't assume Spark #2's architecture, runtime backend, or safety
  situation mirror Spark #1's - confirm each independently.
- Don't skip the branch/PR/merge flow for the `PLANNING.md` write-up.
- Don't treat "the architecture says this should work" as a substitute
  for actually running the steps above - that's the entire point of this
  pass.
- Don't leave test infrastructure running or test data on either node
  once the pass is complete, unless the user says otherwise.
