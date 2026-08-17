# Sparky

Sparky gives a team of developers self-service control over loading and tuning LLM
inference engines across a small fleet of GPU compute nodes - NVIDIA DGX Spark
hardware and other GPU hosts alike, each using whichever runtime backend (Docker/
Podman with CDI GPU passthrough, or direct bare-metal execution) fits its own
GPU-passthrough situation - authenticated against Active Directory, with tiered
permissions and full audit visibility.

---

## Status

**v0.1.0 (core foundation) is substantially complete**: AD/LDAP auth and RBAC
tiers, the audit log, model profiles, model transfers, running-instance load/
unload, live telemetry, and the full Dashboard UI are all built and merged. Its one
remaining item - CDI GPU passthrough verification for the Docker/Podman runtime
backend - is blocked on real DGX Spark hardware not yet in hand, not unfinished
work.

**v0.2.0's bare-metal runtime backend and its `sparky-agent setup` subcommand are
also done**, validated end-to-end on real GPU hardware: a real inference engine
has been loaded, served a request, and cleanly unloaded through it, not just
exercised in simulation. Engine-binary provisioning from GitHub Releases and
per-profile engine version pinning are also done. v0.2.0's only remaining item is
the same Spark-hardware CDI validation v0.1.0 is waiting on.

See `PLANNING.md` for the full milestone breakdown, decisions log, and what's
next.

---

## What It Does

- Load, configure, and monitor LLM inference engines - vLLM and llama.cpp-style
  partial-GPU-offload engines today, with Aphrodite support planned - via saved,
  reusable profiles
- Authenticate against on-prem Active Directory (with an Entra ID / OIDC migration
  path built in), with a login gate separate from four internal permission tiers:
  Read-only, Developer, PowerDev, and Admin - plus an isolated SuperAdmin break-glass
  account for recovery when AD itself is unavailable
- Run inference engines inside a Docker/Podman container with CDI GPU passthrough,
  or as a direct bare-metal process when passthrough isn't viable (e.g. a
  workstation GPU already claimed by its own host session) - node agents
  self-provision via `.deb`, `.rpm`, or a tarball
- Live GPU/CPU/memory dashboards, with every state-changing action recorded in an
  immutable audit log

Multi-node clustering, historical metrics retention, and Podman/Kubernetes
deployment for the central app itself are planned but not yet built - see
`PLANNING.md` for the full roadmap.

See `ARCHITECTURE.md` for the full technical picture and `SCHEMA.md` for the data
model.

---

## Quick Start

### Bare metal (Debian/Ubuntu or RHEL/Fedora)

**Central app** - no packaged installer yet; build and run the binary directly,
then complete first-run setup (see `CLAUDE.md` Build and Run for the full
sequence):

```bash
go run ./cmd/sparky-server setup
go run ./cmd/sparky-server
```

**Node agent** - install via `.deb`, `.rpm`, or a tarball (see `docs/AGENT.md`
Build and Install for all three and full configuration details):

```bash
sudo apt install ./sparky-agent_<version>_<arch>.deb      # Debian/Ubuntu
sudo dnf install ./sparky-agent-<version>-1.<arch>.rpm     # RHEL/Fedora/Rocky/Alma
tar xzf sparky-agent-<version>-linux-<arch>.tar.gz && cd sparky-agent-<version>-linux-<arch> && sudo ./install_agent.sh
```

### Kubernetes

<!-- Helm chart for the central app - see PLANNING.md v0.5.0, not yet built -->

### Podman

<!-- Podman-native deployment path for the central app - see PLANNING.md v0.5.0, not yet built -->

Kubernetes and Podman-native deployment are for the **central app**; compute-node
agents already install identically everywhere via the bare-metal packages above,
regardless of how the central app itself is deployed. Full installation and
first-run setup details are in `docs/AGENT.md` (for node agents) and `CLAUDE.md`
(for the central app).

---

## Documentation

| Document | Covers |
|---|---|
| `ARCHITECTURE.md` | Components, protocol, security, deployment model |
| `SCHEMA.md` | Full database schema reference |
| `PLANNING.md` | Goals, milestones, open questions, and the full decisions log with rationale |
| `docs/AGENT.md` | Node agent specifics - install, config, service architecture |
| `CLAUDE.md` | Tech stack, repo layout, build/test commands, conventions |

---

## Contributing

See `CONTRIBUTING.md`. Contributions require agreeing to the CLA (`CLA.md`).

---

## License

Dual-licensed: [AGPLv3](https://www.gnu.org/licenses/agpl-3.0.html) or a commercial
license. See `LICENSE` for the AGPLv3 terms, or contact <!-- PROJECT_OWNER_NAME -->
for commercial licensing.
