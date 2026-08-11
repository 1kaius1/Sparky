# AGENT.md - Sparky Agent (Service/Daemon, Go)

Companion to `CLAUDE.md` and `ARCHITECTURE.md`, scoped to `cmd/sparky-agent`. Read
this before touching anything under `agent/` or `cmd/sparky-agent/`. General Go style
and workflow rules still come from `.clauderules` - this file covers what's specific
to running as a background daemon on compute hardware rather than a web app on
general-purpose infrastructure.

---

## Project Overview

The agent runs on every compute node - an NVIDIA DGX Spark or a generic Docker/Podman
GPU host - and does four things on the central app's behalf: manages the lifecycle of
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
| GPU passthrough | CDI (`nvidia.com/gpu=all`)                     | Standardized across both container runtimes |

---

## Repository Layout

See `CLAUDE.md` for the full repo tree. Agent-specific code lives under:

```
agent/
- connection/          # Connection goroutine: dial, hello/auth handshake, heartbeat, reconnect-with-backoff
- runtime/
  - baremetal/      # Spark: direct process exec, runs as `serviceloop`
  - containers/      # Docker/Podman: shared Docker-Engine-API-compatible backend
- telemetry/          # nvidia-smi and /proc collectors
cmd/sparky-agent/      # Entry point
deploy/systemd/        # sparky-agent.service unit template
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

The one-script installer (`scripts/install.sh`) automates all of this; documented
here for what it actually does:

<!-- Steps 1-2 (serviceloop account creation and group membership) are planned to
move into a `sparky-agent setup` subcommand alongside the v0.2.0 bare-metal runtime
backend, with install.sh delegating to it instead of calling useradd/usermod
directly - see PLANNING.md Decisions Log, 2026-08-07. Not yet implemented; the steps
below still reflect what install.sh actually does today. -->

```bash
# 1. Create the dedicated service account (Spark targets specifically use `serviceloop`)
sudo useradd --system --no-create-home --shell /usr/sbin/nologin serviceloop

# 2. Add it to the group that gates GPU device access (video or render, depending on distro)
sudo usermod -aG video serviceloop

# 3. Install the binary
sudo cp bin/sparky-agent /usr/local/bin/sparky-agent

# 4. Create the secrets directory and file - 0600, owned by the service account
sudo mkdir -p /etc/sparky-agent
sudo cp secrets.env.template /etc/sparky-agent/secrets.env
sudo chown serviceloop:serviceloop /etc/sparky-agent/secrets.env
sudo chmod 0600 /etc/sparky-agent/secrets.env
# Fill in the real values - see Configuration below

# 5. Install and enable the systemd unit
sudo cp deploy/systemd/sparky-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sparky-agent
```

Only the owning service account and root can read `secrets.env` - see
`ARCHITECTURE.md` Security Considerations for the reasoning.

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
| `SPARKY_NODE_TYPE`               | Yes      | -       | `spark` or `docker-gpu` - selects the runtime backend    |
| `SPARKY_CONTAINER_RUNTIME`       | No       | -       | `docker` or `podman` - only meaningful when `SPARKY_NODE_TYPE=docker-gpu` |
| `SPARKY_MODEL_STORAGE_PATH`      | No       | `/home/serviceloop/models` on Spark | Per-`node_type` configurable, not hardcoded |
| `SPARKY_TELEMETRY_POLL_INTERVAL` | No       | `5s`    | How often telemetry is collected and pushed              |
| `LOG_LEVEL`                      | No       | `info`  | |
| `LOG_FORMAT`                     | No       | `json`  | |

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

`context.Context` is propagated through all of these for cancellation on shutdown;
`sync.WaitGroup` tracks in-flight transfers so graceful shutdown can wait for them to
reach a safe stopping point rather than killing a transfer mid-write.

---

## Init and Service Manager Files

```
deploy/systemd/
- sparky-agent.service
```

When agent startup, shutdown, or dependency behavior changes, update this file in the
same change - do not let it drift from what the binary actually does.

---

## Code Style

Same rules as the rest of the repo - see `.clauderules`. Nothing agent-specific to
add beyond what's already in this file.
