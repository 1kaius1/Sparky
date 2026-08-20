# AGENT.md - Sparky Agent (Service/Daemon, Go)

Companion to `CLAUDE.md` and `ARCHITECTURE.md`, scoped to `cmd/sparky-agent`. Read
this before touching anything under `agent/` or `cmd/sparky-agent/`. General Go style
and workflow rules still come from `CLAUDE.md` - this file covers what's specific
to running as a background daemon on compute hardware rather than a web app on
general-purpose infrastructure.

---

## Project Overview

The agent runs on every compute node - an NVIDIA DGX Spark or a generic GPU host,
using whichever runtime backend fits its own GPU-passthrough situation (`SCHEMA.md`
Nodes' `runtime_backend`: `docker` / `podman` / `bare-metal`) - and does four things
on the central app's behalf: manages the lifecycle of
inference engine processes, collects hardware telemetry, executes model transfers
(downloads and peer-to-peer replication), and reports its own health. It connects
*outbound* to the central app over a persistent WebSocket and never accepts inbound
connections itself - see `ARCHITECTURE.md` Protocol for why.

---

## Tech Stack

| Component       | Choice                                      | Reason                                   |
|------------------|------------------------------------------------|---------------------------------------------|
| Language        | Go                                             | Shared with the server; single toolchain |
| Config format   | Environment variables                          | Consistent with the server, not the generic Service-type default of `~/.app_name/config.yaml` - see Configuration below |
| Transport       | Agent-initiated persistent WebSocket, JSON     | See `ARCHITECTURE.md` Protocol             |
| Container mgmt  | `github.com/moby/moby/client` (Docker Engine API) | Targets Docker and Podman identically - see CLAUDE.md Tech Stack for the import-path rename note |
| GPU passthrough | `nvidia` mechanism (Docker) / CDI (Podman) - see `runtime.GPUDeviceMechanism` | Chosen per `runtime_backend`, not one mechanism standardized across both - Docker and Podman need genuinely different requests through the same Engine API, confirmed empirically (see `ARCHITECTURE.md` Runtime Backends) |

---

## Repository Layout

See `CLAUDE.md` for the full repo tree. Agent-specific code lives under:

```
agent/
- connection/          # Connection goroutine: dial, hello/auth handshake, heartbeat, reconnect-with-backoff
- provision/           # serviceloop account, model storage dir, GPU-group membership - see `sparky-agent setup` below
- runtime/
  - baremetal/      # Direct process exec, runs as `serviceloop` - for hosts without GPU passthrough, not specific to Spark
  - containers/      # Docker/Podman: shared Docker-Engine-API-compatible backend
- telemetry/          # nvidia-smi and /proc collectors
cmd/sparky-agent/      # Entry point; `setup` subcommand (agent/provision's only caller)
deploy/
- systemd/sparky-agent.service   # Unit file, shared by all three install methods below
- secrets.env.template            # Config template - see Configuration and Data Storage
scripts/
- install_agent.sh     # Tarball installer
- uninstall_agent.sh   # Tarball uninstaller (--purge to also remove serviceloop/secrets.env)
- build_packages.sh    # Builds all three artifacts (amd64 + arm64) into dist/ - maintainer-facing
- packaging/            # nfpm config, scriptlets, and the shared install logic they call -
                        # see Install (bare metal) below
```

---

## Build and Install

### Build

```bash
go build -o bin/sparky-agent ./cmd/sparky-agent
```

Cross-compilation for the Spark's ARM64 CPU and x86_64 workstations works with
standard `GOOS`/`GOARCH` - no special tooling needed even from within the monorepo:

```bash
GOOS=linux GOARCH=arm64 go build -o bin/sparky-agent-arm64 ./cmd/sparky-agent
GOOS=linux GOARCH=amd64 go build -o bin/sparky-agent-amd64 ./cmd/sparky-agent
```

### Install (bare metal)

Three install methods, all producing the same end state (binary, systemd unit,
`serviceloop` service account, GPU-group membership, an empty `secrets.env` to
fill in) - built by `scripts/build_packages.sh` via [nfpm](https://nfpm.goreleaser.com)
into `dist/` (see PLANNING.md Decisions Log for why nfpm over hand-rolled
`dpkg-deb`/`rpmbuild` or GoReleaser). All three land the binary at
`/opt/sparky/bin/sparky-agent`, with a `/usr/local/bin/sparky-agent` symlink for
convenience (`/opt/sparky/bin` isn't on any distro's default `$PATH`).

**.deb** (Debian/Ubuntu):

```bash
sudo apt install ./sparky-agent_<version>_<arch>.deb
```

**.rpm** (RHEL/Fedora/Rocky/Alma):

```bash
sudo dnf install ./sparky-agent-<version>-1.<arch>.rpm
```

RPM has no equivalent of `apt purge` - `%postun` scriptlets only ever receive a
numeric install-count, with no "and delete everything" signal the way dpkg's
`purge` argument provides. `dnf remove` stops and disables the service and
removes the binary, same as `apt remove`, but leaves `serviceloop` and
`/etc/sparky-agent` in place; a copy of `purge_rpm.sh` is left behind at
`/usr/local/sbin/sparky-agent-purge.sh` specifically so a full cleanup command is
always available on the box after removal:

```bash
sudo dnf remove sparky-agent
sudo /usr/local/sbin/sparky-agent-purge.sh   # optional - removes serviceloop + /etc/sparky-agent
```

**Tarball** (any systemd-based distro):

```bash
tar xzf sparky-agent-<version>-linux-<arch>.tar.gz
cd sparky-agent-<version>-linux-<arch>
sudo ./install_agent.sh
```

Not package-manager-owned, so the systemd unit installs to
`/etc/systemd/system/sparky-agent.service` instead of the `.deb`/`.rpm` packages'
`/usr/lib/systemd/system/sparky-agent.service`. `sudo ./uninstall_agent.sh
[--purge]` reverses it - a copy is also persisted at
`/opt/sparky/share/sparky-agent/uninstall_agent.sh` so it can be run later even if
the original tarball is gone.

All three methods invoke `sparky-agent setup` (see below) after placing the
binary, which:
- Creates the `serviceloop` service account (bare-metal hosts use this account -
  see SCHEMA.md Nodes), with its home directory at `/opt/sparky/serviceloop`
  (0750, owned by `serviceloop`) rather than the default `/home/serviceloop` -
  the systemd unit's `ProtectHome=true` makes `/home/*` inaccessible to the
  running process, so this sidesteps that entirely instead of needing an
  exception carved out. This is also the parent of
  `SPARKY_MODEL_STORAGE_PATH`'s bare-metal default (see Configuration below) -
  a purge deliberately leaves its contents (real downloaded model data) in
  place, same reasoning as leaving `secrets.env` behind on a plain `remove`.
  Also joins the account to whichever of the `video`/`render` groups actually
  exist on this distro/driver combination (both are joined if both exist -
  which one actually gates GPU device access varies)
- Install `/etc/sparky-agent/secrets.env` (0600, owned by `serviceloop`) from the
  packaged template, but only if it doesn't already exist - an upgrade never
  overwrites an already-configured secrets file
- Enable the systemd unit but deliberately do **not** start it - an unconfigured
  `secrets.env` would just crash-loop. Fill in the real values (see Configuration
  below), then:
  ```bash
  sudo systemctl start sparky-agent
  ```
- On an upgrade of an already-running install, safely restart the service onto
  the new binary automatically - see Signal Handling below for why an agent
  restart doesn't disrupt an already-loaded model on the container-runtime
  backend

Only the owning service account and root can read `secrets.env` - see
`ARCHITECTURE.md` Security Considerations for the reasoning.

#### `sparky-agent setup`

```bash
sudo sparky-agent setup
```

Creates/verifies the `serviceloop` system account, its model storage home
directory, and its GPU-passthrough group membership (`agent/provision`) -
idempotent, safe to re-run. Requires root; exits with a clear error otherwise
(no other subcommand in either binary self-checks this, but no other
subcommand needs `useradd`/`usermod` either). All three install methods above
call this automatically after placing the binary - running it by hand is only
needed for diagnostics or to repair an already-provisioned node.

Verified via `nfpm`-built packages installed/upgraded/removed/purged inside
disposable Debian and Rocky Linux podman containers running real systemd
(`--systemd=always /sbin/init`), covering both `video`-only and `video`+`render`
group-detection cases. Real GPU hardware, real Spark ARM64 execution, and true
bare-metal PID 1 behavior (journald integration, udev device-permission timing,
real boot ordering) remain unverified - a container's systemd is a reasonable
proxy, not identical to real hardware, same honest gap already on record for CDI
passthrough verification.

---

## Running the Service

```bash
sudo systemctl start sparky-agent
sudo systemctl stop sparky-agent
sudo systemctl status sparky-agent
journalctl -u sparky-agent -f
```

### Signal Handling

| Signal   | Behavior                                                        |
|----------|--------------------------------------------------------------------|
| SIGTERM  | Graceful shutdown - stop managed engine processes cleanly, close the WebSocket connection |
| SIGINT   | Graceful shutdown - same as SIGTERM                              |

No config-reload signal is defined - a config change requires a restart. Nothing in
the design calls for hot-reloading agent config, so this stays simple rather than
adding a SIGHUP handler for a need that hasn't come up.

"Stop managed engine processes cleanly" means different things per runtime backend.
For the Docker/Podman backend (`agent/runtime/containers`, built for the Docker/Podman
runtime backend work), a Running instance's container is managed by the container
runtime daemon via its API, not spawned as a child process of the agent - the
container keeps running fine independent of the agent's own process lifetime, the
same way it survives any other API client disconnecting. Sparky deliberately leaves
it running across an agent restart rather than tearing it down: a plain agent restart
(a deploy, a crash-restart) is not the same event as an operator-initiated unload, and
treating them the same would make a routine agent bounce needlessly disruptive to an
already-loaded model that may be serving real requests and took real time to load.
Graceful shutdown for this backend is limited to what Run's `sync.WaitGroup`s already
cover: letting an in-flight `load_instance`/`unload_instance`/transfer goroutine reach
a safe stopping point, then closing the WebSocket connection - see
`agent/connection.Conn.Run`. The bare-metal backend (`agent/runtime/baremetal`,
direct process exec) is where this row's literal meaning applies: an exec'd engine
process is a real child of the agent, so an unclean agent exit risks orphaning or
corrupting it. `Run`'s exit path calls the runtime backend's `Shutdown` method after
its in-flight `load_instance`/`unload_instance` goroutines finish (so a still-starting
load can't race a shutdown that's already stopping everything) - a no-op for
Docker/Podman (containers are deliberately left running, as above), but for
bare-metal this sends SIGTERM to every process it's still tracking, waits up to a
grace period for a clean exit, then escalates to SIGKILL for anything still running.
A crash or `kill -9` of the agent itself still orphans a bare-metal-managed process -
nothing short of a supervising process tree (out of scope here) closes that gap
entirely - but a normal SIGTERM/SIGINT-driven shutdown (a `systemctl stop`, a
deploy) does not.

---

## Configuration and Data Storage

Deliberately deviates from the generic Service-type default
(`~/.app_name/config.yaml` with CLI-flag and env-var overrides) in favor of
environment-variables-only, matching the central server's convention - see
`ARCHITECTURE.md` Security Considerations for why consistency across both binaries
mattered more here than following the generic per-type default.

On bare metal, these are delivered via systemd's `EnvironmentFile=` pointing at the
`0600` secrets file created during install. On Podman/Kubernetes, the same variables
come from Secrets, identically to the server.

| Variable                        | Required | Default | Description                                         |
|-----------------------------------|----------|---------|---------------------------------------------------------|
| `SPARKY_CENTRAL_URL`             | Yes      | -       | WebSocket URL of the central app to dial                |
| `SPARKY_BEARER_TOKEN`            | Yes      | -       | Presented at connect time - see `ARCHITECTURE.md` Protocol |
| `SPARKY_NODE_NAME`               | Yes      | -       | Must match this node's registered name in the central app |
| `SPARKY_RUNTIME_BACKEND`         | Yes      | -       | `docker`, `podman`, or `bare-metal` - see `SCHEMA.md` Nodes |
| `SPARKY_MODEL_STORAGE_PATH`      | No       | `/opt/sparky/serviceloop/models` on a bare-metal host | Per-`runtime_backend` configurable, not hardcoded |
| `SPARKY_LLAMACPP_BINARY_PATH`    | No       | -       | Bare-metal only - local `llama.cpp` server executable for a `llamacpp` `load_instance`. Unset means this node doesn't run that engine type |
| `SPARKY_VLLM_BINARY_PATH`        | No       | -       | Bare-metal only - local vLLM executable/entrypoint for a `vllm` `load_instance`. Unset means this node doesn't run that engine type |
| `SPARKY_ENGINE_INSTALL_PATH`     | No       | `/opt/sparky/serviceloop/engines` on a bare-metal host | Bare-metal only - root directory a `start_engine_transfer` provisioning run installs into. See Engine binary provisioning below |
| `SPARKY_TELEMETRY_POLL_INTERVAL` | No       | `5s`    | How often telemetry is collected and pushed              |
| `LOG_LEVEL`                      | No       | `info`  | |
| `LOG_FORMAT`                     | No       | `json`  | |

### Engine binary provisioning

Self-service download/install of a maintainer-built compiled-engine release
(`llamacpp` today - see PLANNING.md's 2026-08-15 Decisions Log entry for why Python-
based engines like vLLM use a different v0.3.0 mechanism instead), triggered by an
Admin/SuperAdmin from the central app's "Engine transfers" UI page
(`internal/engineprovision.Service.ProvisionEngine`, gated by `rbac.CanManageNodes` -
see CLAUDE.md Frontend Conventions) and executed
by `agent/enginetransfer` on the target node: download the release tarball and its
sibling `.sha256` checksum file from Sparky's own GitHub Releases, verify the
checksum, extract into a versioned install directory, and atomically repoint a
`latest` symlink at it:

```
$SPARKY_ENGINE_INSTALL_PATH/
  llamacpp/
    b4523/              # a previously-installed version - left in place, not
      llama-server       # deleted, when a newer one is provisioned
      lib*.so
    b4610/              # most recently provisioned version
      llama-server
      lib*.so
    latest -> b4610/    # symlink, repointed atomically on each successful run
```

Multiple versions coexist on disk by design, not as a transient state - this is what
lets a future Model profile pin a specific engine version for side-by-side comparison
against another (not yet built - see `PLANNING.md`'s deferred `engine_version`
follow-up item). `SPARKY_LLAMACPP_BINARY_PATH`/`SPARKY_VLLM_BINARY_PATH` semantics
are unchanged by any of this - still a static, operator-set path to one executable,
per docs/AGENT.md Configuration above. The one-time setup step after a version is
first provisioned: point `SPARKY_LLAMACPP_BINARY_PATH` at
`$SPARKY_ENGINE_INSTALL_PATH/llamacpp/latest/llama-server` and restart the agent
(`systemctl restart sparky-agent`) - provisioning a later version only swaps the
symlink target, so no further config edit is needed, though a restart is still
required to pick up the new binary for the agent's next `load_instance`, matching
this file's existing "config change requires a restart" policy.

Checksum verification is mandatory here, unlike the Hugging Face Transfer Executor's
own model-weight downloads (`agent/transfer`), which perform none - see
`SCHEMA.md` Engine transfers.

**Producing these release bundles.** This section covers the consumer side -
how `agent/enginetransfer` downloads and installs a bundle once one exists. The
bundle itself is built and published with `scripts/build_engine_release.sh`
(compiles the pinned upstream tag, packages the exact `<engine>-<version>-
<arch>.tar.xz` + `.sha256` shape this consumer expects) and
`scripts/publish_engine_release.sh` (publishes an already-built bundle as a
GitHub Release on this repo, matching `agent/enginetransfer`'s hardcoded
source). Both are maintainer-invoked today, not wired into CI - see
`PLANNING.md`'s Decisions Log for the full design and the planned follow-up
automation.

**Per-profile pinned versions.** A Model profile may pin a specific installed
version instead of leaving a `load_instance` to resolve through the `latest`
symlink - see `SCHEMA.md` Model profiles' `engine_version` column. This changes
nothing about the on-disk layout or the `SPARKY_<ENGINE>_BINARY_PATH`-points-at-
`latest` convention above; at load time, `agent/connection`'s
`resolveEngineBinaryPath` reuses the *filename* of the operator's static
`SPARKY_<ENGINE>_BINARY_PATH` config under the pinned version's own directory
instead of `latest` - `$SPARKY_ENGINE_INSTALL_PATH/<engine_type>/<version>/<same
filename>` - so no additional per-version configuration is needed on the node. An
unpinned profile (the common case) resolves exactly as it always has. A pin that
doesn't correspond to an actually-installed version is not validated centrally
before dispatch - it fails clearly at launch time via the same "no such file or
directory" error path any other misconfigured `SPARKY_<ENGINE>_BINARY_PATH`
already produces, reported back as a failed `instance_result`.

---

## Service Architecture Notes

The agent runs a small set of long-lived goroutines rather than a single blocking
loop:

- **Connection goroutine**: owns the WebSocket lifecycle - dial, handshake with the
  bearer token, read loop, and reconnect-with-backoff on disconnect. Every other
  goroutine sends outbound messages through this one rather than managing the socket
  itself.
- **Command loop**: reads incoming messages, dispatches to the appropriate runtime
  backend (start/stop an engine) or transfer executor (start a download/replication),
  and writes the result back.
- **Telemetry goroutine**: polls `nvidia-smi` and `/proc` on `SPARKY_TELEMETRY_POLL_INTERVAL`
  and pushes readings over the same connection - does not wait on the command loop.
- **Transfer goroutines**: one per active transfer, so a long-running download or
  rsync replication never blocks command handling. Progress is streamed back
  periodically, not just on completion.
- **Engine transfer goroutines**: one per active engine binary provisioning run
  (`agent/enginetransfer`), tracked in their own `sync.WaitGroup` separate from
  Transfer goroutines' - a model-weight download and an engine provisioning run are
  unrelated operations with no reason to block each other's shutdown wait. See Engine
  binary provisioning above.

`context.Context` is propagated through all of these for cancellation on shutdown;
`sync.WaitGroup` tracks in-flight transfers so graceful shutdown can wait for them to
reach a safe stopping point rather than killing a transfer mid-write.

---

## Init and Service Manager Files

```
deploy/
- systemd/sparky-agent.service
- secrets.env.template
```

When agent startup, shutdown, or dependency behavior changes, update
`sparky-agent.service` in the same change - do not let it drift from what the
binary actually does. When a configuration environment variable is added, removed,
or changes its default, update `secrets.env.template` in the same change too - see
Configuration and Data Storage below for the full variable list this template
must stay in sync with.

---

## Code Style

Same rules as the rest of the repo - see `CLAUDE.md`'s Code Style and Conventions
section. Nothing agent-specific to add beyond what's already in this file.
