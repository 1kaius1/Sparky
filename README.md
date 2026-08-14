# Sparky

Sparky gives a team of developers self-service control over loading and tuning LLM
inference engines across a small fleet of GPU compute nodes - NVIDIA DGX Spark
hardware and other Docker/Podman GPU hosts alike - authenticated against Active
Directory, with tiered permissions and full audit visibility.

---

## Status

**v0.1.0 implementation in progress.** See `PLANNING.md` for the full milestone
breakdown and current status. This README will be updated as v0.1.0 takes shape.

---

## What It Does

- Load, configure, and monitor LLM inference engines (vLLM, Aphrodite, and
  llama.cpp-style partial-GPU-offload engines) via saved, reusable profiles
- Authenticate against on-prem Active Directory (with an Entra ID / OIDC migration
  path built in), with a login gate separate from four internal permission tiers:
  Read-only, Developer, PowerDev, and Admin - plus an isolated SuperAdmin break-glass
  account for recovery when AD itself is unavailable
- Cluster multiple linked Spark nodes for models too large for a single node,
  with explicit head/worker role assignment and a reduced-capacity fallback when a
  node in the cluster isn't available
- Live and historical GPU/CPU/memory dashboards, with every state-changing action
  recorded in an immutable audit log
- Runs on bare-metal Linux, Podman, or Kubernetes, with identical configuration
  across all three

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

```bash
helm install sparky ./deploy/helm
```

### Podman

<!-- Podman-native install steps - see PLANNING.md v0.5.0 -->

Full installation and first-run setup details are in `docs/AGENT.md` (for node
agents) and `CLAUDE.md` (for the central app).

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
