# Bare-metal backend: fix the home-directory bug, then validate on real hardware

Working notes for the next pass on PLANNING.md's v0.2.0 bare-metal runtime
backend item - specifically its last open sub-item, "Real hardware validation
against the RTX 4090 laptop." Not yet started; kept here instead of only in an
ephemeral session plan so it survives between sessions.

## Context

Before writing the validation runbook, checking what a real validation pass
would actually hit surfaced a concrete, confirmed bug spanning two already-merged
PRs (the bare-metal packaging work and the bare-metal backend work):

- `deploy/systemd/sparky-agent.service` sets `ProtectHome=true` - systemd makes
  `/home/*` inaccessible to the service process.
- `scripts/packaging/lib/agent-common.sh`'s `ensure_serviceloop_user` runs
  `useradd --system --no-create-home ...` - `/home/serviceloop` is never created
  at all.
- `agent/config`'s bare-metal default (`bareMetalDefaultModelStoragePath`) is
  `/home/serviceloop/models`.

Put together: a bare-metal node started via the real systemd unit, using the
documented default model storage path, would fail outright - the directory
doesn't exist, and even if it did, `ProtectHome=true` would hide it from the
process. This would have been the very first thing hardware validation ran into.

Proposed fix, confirmed with the user: give `serviceloop` its home directory at
`/opt/sparky/serviceloop` instead of `/home/serviceloop`. This keeps every
sparky-agent-owned path under the one `/opt/sparky` tree already used for the
binary and packaged share files, and sidesteps `ProtectHome=true` entirely rather
than carving out an exception to it - `/opt` isn't a path that directive touches,
so the unit's hardening stays fully intact with zero special-casing.
`agent/transfer.Executor.Download` already `MkdirAll`s everything under the model
storage root at write time, so the packaging-side fix only needs to ensure the
home directory itself exists and is `serviceloop`-owned; nothing else downstream
needs to change. `userdel serviceloop` (no `-r`, unchanged) still won't touch
directory contents on purge, so real model data already survives a purge today
under the old path and continues to under the new one - no new purge-time
deletion logic needed or wanted.

Getting a model onto the laptop for the validation pass itself: confirmed with
the user to do this by manual file placement, not by building the (separately
missing, pre-existing, unrelated) transfer-initiation UI/endpoint -
`internal/transfers.Service.InitiateTransfer` is fully built and RBAC-gated but
no HTTP handler calls it anywhere; that's real, already-existing scope this task
deliberately does not take on.

Everything in Part A below is buildable and testable in a normal dev sandbox
(no GPU needed). Part B is a precise runbook to execute on the actual RTX 4090
laptop - it needs real GPU hardware and a real llama.cpp/vLLM install, so it's
written precisely enough to run as-is, with results (and any bugs found) coming
back as normal follow-up fixes.

## Approach

### Part A - fix the home-directory bug

- **`scripts/packaging/lib/agent-common.sh`**: `ensure_serviceloop_user` gains
  `--home-dir /opt/sparky/serviceloop` on the `useradd` call (still
  `--no-create-home` - directory creation is handled explicitly, not via
  useradd's own skel-copying, which is irrelevant for a system account and less
  predictable to reason about). New function `ensure_model_storage_dir`,
  following `ensure_secrets_file`'s existing style: `install -d -o serviceloop -g
  serviceloop -m 0750 /opt/sparky/serviceloop` - idempotent, safe to run on
  every install and upgrade.
- **`scripts/packaging/postinstall.sh`** and **`scripts/install_agent.sh`**: call
  `ensure_model_storage_dir` right after `ensure_serviceloop_user`, same call
  order both files already share for the other `ensure_*` functions.
- **`agent/config/config.go`**: `bareMetalDefaultModelStoragePath` becomes
  `/opt/sparky/serviceloop/models`; doc comment updated to explain why (sidesteps
  `ProtectHome=true` rather than needing an exception to it).
- **`agent/config/config_test.go`**: update the two assertions that check the
  literal default path string.
- **`docs/AGENT.md`**: Configuration table's `SPARKY_MODEL_STORAGE_PATH` row; the
  "serviceloop service account" bullet under "All three methods" gains a mention
  of the home directory now being created too; a short note that purge
  deliberately leaves this directory's contents (real model data) in place, same
  as it already would have for `/etc/sparky-agent/secrets.env`'s sibling
  reasoning.
- **`ARCHITECTURE.md`**: Transfer Executor & Local Store Manager's path reference.
- **`deploy/secrets.env.template`**: comment referencing the old default path.
- **`PLANNING.md`**: new Decisions Log entry recording the bug, why `/opt/sparky`
  was chosen over the alternative (dropping `ProtectHome`/switching to
  `--create-home`), and that it was found by inspection before any hardware was
  touched. The in-progress bare-metal-backend sub-bullet's own text (written in
  the same session the backend was built, describing the default as already
  correctly implemented) gets corrected in place - it predates this fix and
  would otherwise misstate history within the same still-open item, not the kind
  of already-shipped record the never-rewrite-history rule protects.
- **`CHANGELOG.md`**: fold into the existing `[Unreleased]` bare-metal entry
  (still unreleased, so this is a correction to an in-progress entry, not a new
  one) or add a short adjacent `### Fixed` line - whichever reads more naturally
  once the section is reopened.

### Part B - the validation runbook (executed on the laptop)

1. **Install.** Any of the three packaged methods (`.deb`/`.rpm`/tarball) from
   this project's own `dist/`, or `go run ./cmd/sparky-agent` directly for a
   quicker first pass that skips systemd/`serviceloop` entirely (useful to
   isolate "does the backend work at all" from "does it work exactly as
   packaged" - if using this path, `SPARKY_MODEL_STORAGE_PATH` must be set
   explicitly, since the packaged `serviceloop` home never exists here).
2. **Register the node** via the Nodes page (Admin+): name, hostname/IP,
   `runtime_backend = bare-metal`, `gpu_memory_gb` = the RTX 4090's real VRAM
   (24), `cpu_memory_gb` = the laptop's real RAM. Capture the one-time bearer
   token shown on success.
3. **Configure** `/etc/sparky-agent/secrets.env` (or equivalent env vars for the
   `go run` path): `SPARKY_CENTRAL_URL`, `SPARKY_BEARER_TOKEN`,
   `SPARKY_NODE_NAME` (must match step 2's name exactly), `SPARKY_RUNTIME_BACKEND
   = bare-metal`, `SPARKY_LLAMACPP_BINARY_PATH` pointing at a real
   CUDA-enabled `llama-server` build. Leave `SPARKY_MODEL_STORAGE_PATH` unset on
   a packaged install, to exercise the new default directly.
4. **Start** the agent; confirm the Nodes page shows it online.
5. **Place a model manually**: pick a small quantized GGUF model to keep the
   loop fast, and place its single `.gguf` file at exactly
   `${SPARKY_MODEL_STORAGE_PATH}/<model_ref>/<file>.gguf` (slashes in `model_ref`
   become directories - see `agent/connection.resolveModelPath`), owned/readable
   by `serviceloop`.
6. **Create a Model profile** (PowerDev+): `engine_type = llamacpp`, `model_ref`
   matching step 5's directory exactly, `requires_full_gpu_residency = false`,
   target node = the node from step 2, a free port, and any `engine_params`
   (`n_gpu_layers`/`ctx_size`/`threads`) worth exercising.
7. **Load** the instance (Developer+) and confirm all of:
   - `InstanceResult` comes back `running`, the Running instance's status
     matches
   - the real `llama-server` process is a visible child of `sparky-agent` (`ps
     --ppid`) on the assigned port
   - `nvidia-smi` shows GPU memory/utilization attributed to that process -
     confirms the offload flags actually reached the GPU, not a silent CPU
     fallback
   - a real request against the server's port succeeds (a completion or health
     endpoint) - confirms the arguments llama.cpp received were valid, not just
     that the process started and then died
8. **Unload** and confirm: SIGTERM reaches the process, it exits, the port is
   freed, and the Running instance flips to stopped.
9. **Shutdown behavior**: with an instance still loaded, `systemctl stop
   sparky-agent` (skipping the explicit unload) and confirm the new `Shutdown`
   path fires - the child process is signaled and exits before the unit's stop
   timeout, no orphan left in `ps`. Restart and confirm nothing auto-relaunches
   the model (expected - an operator must explicitly load again); note if the
   `running_instances` row is left stale (still `running` in the database with
   no real process behind it) as a follow-on gap to record, not fix in this pass.
10. **Telemetry cross-check** (same hardware, same session, low extra cost):
    confirm the Metrics dashboard shows real GPU utilization/memory while a
    model is loaded - closes a second, separate, already-tracked Known Issues
    row opportunistically.
11. **vLLM, only if time allows, explicitly exploratory**: install vLLM, set
    `SPARKY_VLLM_BINARY_PATH`, and try loading a profile with `engine_type =
    vllm`. This is the one open question this whole effort has carried since the
    backend was built - whether `LaunchSpec.Args` (flags only) is the right
    invocation, or whether the installed vLLM version's CLI needs a `vllm serve
    <model>` subcommand form instead. Whatever's actually observed gets recorded
    in PLANNING.md and, if it's a real mismatch, fixed in `internal/engines/vllm.go`
    as its own follow-up - not assumed to fit in the same pass as everything
    above.

Any real bug surfaced by steps 7-11 comes back as a normal follow-up fix, same
discipline as every other verification pass in this project - Known Issues gets
updated either way (closed if confirmed working, given a precise new row if not).

## Verification

**Testable without the laptop (Part A):**
- `go build ./...`, `go vet ./...`, `go test ./...` and `go test -race ./...`
  after the `agent/config` change.
- Re-run the same podman-based install/upgrade verification the original
  packaging PR used (disposable Debian/Rocky containers, real systemd via
  `--systemd=always /sbin/init`) - confirms `/opt/sparky/serviceloop` is created
  with the right owner/mode on a fresh install, survives an upgrade, and that a
  process started under the real unit (`ProtectHome=true` in effect) can
  actually read/write under it - all without needing a GPU. This directly tests
  the exact bug found, not just the individual shell functions in isolation.

**Not testable without it, Part B's whole point:** every laptop step above needs
real GPU hardware, real drivers, and a real llama.cpp/vLLM install - the runbook
is written precisely enough to run as-is, with results reported back for
follow-up fixes.
