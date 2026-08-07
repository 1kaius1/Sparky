# Planning

This document tracks project goals, milestones, open questions, and design decisions.
It is a living document - update it as the project evolves. Claude Code should read
this file at the start of every session to understand current project state.

Companion documents: `ARCHITECTURE.md` (technical reference), `docs/AGENT.md` (agent-
specific conventions).

---

## Project Goal

Sparky gives a team of developers full, self-service control over loading and tuning
LLM inference workloads across a small cluster of NVIDIA DGX Spark nodes (and,
eventually, other GPU hosts), authenticated against Active Directory, with tiered
permissions, full audit visibility, and live and historical hardware telemetry.
Success looks like: a developer can load a model with a saved profile, see it running
and its GPU/CPU/memory usage, and an admin can see exactly who did what, when.

---

## Non-Goals

- Not a general-purpose container orchestrator - Sparky manages LLM inference
  workloads specifically, not arbitrary containerized applications. Kubernetes/Podman
  remain the orchestrator where used; Sparky sits on top for this one purpose.
- Not a training or fine-tuning platform - inference serving only.
- Not a metrics/observability platform - Sparky ships its own narrowly-scoped
  telemetry for its own dashboards; it deliberately does not attempt to replace or
  compete with Prometheus/Grafana-style general observability stacks.
- Not a multi-tenant SaaS product - single-organization internal tool.
- Does not administer Active Directory - reads group membership for authentication
  and the login gate; never creates, modifies, or manages AD users or groups.

---

## Current Status

**Status:** Planning

Architecture and design are complete (auth/permissions, full data model, component
boundaries, protocol design, tech stack, deployment tooling for bare metal, Podman,
and Kubernetes). Implementation has not yet started.

---

## Milestones

- [ ] **v0.1.0** - Core foundation (non-Spark first: laptop RTX 4090 / Dell Precision RTX 3080Ti as primary dev and test hardware)
  - [ ] AD/LDAP bind auth (login-gate group), session handling
  - [ ] Users, RBAC tiers (Read-only/Developer/PowerDev/Admin), SuperAdmin break-glass
  - [ ] `sparky setup` CLI first-run wizard
  - [ ] Audit log covering all state-changing actions, including SuperAdmin
  - [ ] Node registry with `node_type` and the `gpu_memory_gb` / `cpu_memory_gb` split from the start
  - [ ] Agent: Docker/Podman runtime backend (Docker-Engine-API-compatible), agent-initiated WebSocket, bearer token, CDI GPU passthrough
  - [ ] Model profiles: single-node only; vLLM (full-residency) and llama.cpp-style (partial-offload) adapters both from the start, with `requires_full_gpu_residency` - the laptop's 32GB RAM budget makes partial offload immediately relevant, not a later nice-to-have
  - [ ] Model transfers: Hugging Face download only (no peer replication yet)
  - [ ] Running instances: single-node load/unload
  - [ ] Metrics: live telemetry collection and dashboard (no historical retention yet)
  - [ ] Dashboard UI (htmx), sidebar nav, Read-only through Admin views
  - [ ] Bare-metal install script (apt + dnf)

- [ ] **v0.2.0** - Spark bare-metal support
  - [ ] Agent: bare-metal runtime backend for Spark (`serviceloop`, direct process exec)
  - [ ] Validate the full stack against real Spark hardware

- [ ] **v0.3.0** - Multi-Spark clustering
  - [ ] Fabric groups, physical linkage tracking
  - [ ] Clustered model profiles (Profile cluster nodes: head/worker/rank)
  - [ ] Node model inventory, Green/Blue/Red launch eligibility
  - [ ] Peer-to-peer rsync replication over the cluster link
  - [ ] Reduced-capacity launch prompt and relaunch-at-full-capacity flow
  - [ ] Running instance nodes (actual runtime topology, may differ from profile intent)
  - [ ] Aphrodite engine adapter

- [ ] **v0.4.0** - Historical metrics
  - [ ] Metrics retention: 6 months raw, configurable downsampled aggregate window
  - [ ] NFS/S3 export configuration for aggregates
  - [ ] Historical trend charts in the dashboard
  - [ ] Per-user PowerDev download/delete grant in active use
  - [ ] Configurable audit log retention (up to 24 months) and optional syslog/GELF forwarding (Graylog-compatible)

- [ ] **v0.5.0** - Deployment maturity
  - [ ] Helm chart (Kubernetes), `existingSecret` support
  - [ ] Podman-native deployment path for the central app
  - [ ] OIDC/Entra ID identity provider (migration path activated)

- [ ] **v1.0.0** - Production-ready release
  - [ ] Full test coverage
  - [ ] Complete documentation
  - [ ] Cross-platform verification (Debian/Ubuntu + RHEL/Fedora)
  - [ ] Security review pass

---

## Current Sprint / Active Work

- [ ] Compile `ARCHITECTURE.md` from the completed design
- [ ] Compile `CLAUDE.md` (tech stack, repo layout, env vars, API conventions)
- [ ] Write `docs/AGENT.md`, `README.md`, `.clauderules`, `.env.example`

---

## Open Questions

| # | Question | Raised | Notes |
|---|----------|--------|-------|
| 1 | What happens to an active session when a user is removed from the AD access group mid-session? | 2026-08-06 | Set aside as a refinement, not a blocker, during auth design. |
| 2 | Does CDI GPU-passthrough behave identically on Podman across target distros/hardware? | 2026-08-06 | A known CDI-hook filesystem gotcha exists on some Podman setups; needs verification on real Spark and Dell Precision hardware. |
| 3 | Exact default for the configurable downsampled-aggregate retention window (raw window is fixed at 6 months). | 2026-08-06 | No default chosen yet - needs a decision before v0.3.0. |

---

## Decisions Log

| Date | Decision | Rationale | Alternatives Considered |
|------|----------|-----------|-------------------------|
| 2026-08-06 | Agent-per-node control plane | Coordinated multi-node launches and per-node telemetry need direct per-node control | Central SSH orchestration (rejected: fragile CLI-output parsing, broad SSH privilege) |
| 2026-08-06 | Agent-initiated persistent WebSocket connection, not central-app-initiated REST | Sparks need zero inbound network exposure; central app never dials into compute hardware | REST-to-agent (rejected: requires an open listening port on every Spark) |
| 2026-08-06 | Node registry treats cluster size and linkage as data, not a hardcoded assumption | Budget for the 4-node switch was uncertain at design time | Hardcoding a fixed 2-node (or 4-node) assumption (rejected) |
| 2026-08-06 | On-prem AD via LDAP bind first; OIDC/Entra ID migration later, behind an identity-provider interface | Hybrid AD environment; on-prem is the current priority | Entra ID only from the start (deferred, not rejected) |
| 2026-08-06 | Login gated by a separate dedicated AD group; permission tiers are internal app RBAC, not derived from AD groups | Decouples AD group administration from in-app authorization; Admins manage tiers without needing AD access | Tier-per-AD-group mapping (rejected: less flexible, couples two different admin domains) |
| 2026-08-06 | Nested AD group membership resolved via `LDAP_MATCHING_RULE_IN_CHAIN` | Standard AD mechanism for transitive group membership, server-side | Manual recursive resolution in application code (rejected: unnecessary complexity) |
| 2026-08-06 | SuperAdmin break-glass account, isolated credential storage, unrestricted like root | Guarantees a recovery path independent of AD availability | None seriously considered - explicit requirement |
| 2026-08-06 | Every state-changing action is audited with no exceptions, including SuperAdmin's | Accountability matching real-world root/sudo logging conventions | Exempting SuperAdmin actions from audit (rejected) |
| 2026-08-06 | Model download permission defaults to Admin/SuperAdmin, grantable per-user to PowerDev; delete-from-disk tied to the same grant | Symmetric add/remove permission is simpler than two separate overrides | Baseline Developer delete permission (rejected: inconsistent with download gating) |
| 2026-08-06 | Users keyed by an internal UUID with AD SID as an external reference | Survives AD username renames and the planned Entra ID migration without breaking foreign keys | Using AD username/UPN directly as the primary key (rejected) |
| 2026-08-06 | Physical Spark cluster linkage tracked as data (Fabric groups) and validated before a clustered launch | Prevents defining a profile across nodes that are not actually cabled together | Trusting profile authors to know the cabling (rejected) |
| 2026-08-06 | Clustered profiles require explicit head/worker/rank assignment | Consistent with explicit-over-implicit approach used throughout; keeps role assignment auditable | Automatic head selection by convention (rejected: silent behavior change if fabric membership changes) |
| 2026-08-06 | Node-local model storage; large models replicated peer-to-peer over the cluster link rather than re-downloaded per node | Preserves local NVMe load performance; avoids repeated 200+GB internet downloads | Shared network storage for models (rejected for this use case) |
| 2026-08-06 | Three-state (Green/Blue/Red) node eligibility for clustered launches, with a reduced-capacity prompt instead of a hard block | Real-world operational flexibility - a down node shouldn't block getting a smaller cluster running | Hard-blocking any launch with an ineligible node (rejected) |
| 2026-08-06 | `required_memory_gb` on Model profiles is optional; unset means attempt and report failure | Precise VRAM estimation depends on quantization/context/KV cache in ways the app can't reliably compute | Mandatory field with app-computed estimate (rejected: false confidence) |
| 2026-08-06 | REST/JSON over HTTPS for all APIs (agent control plane and frontend) | Debuggability, consistency with vLLM's own OpenAI-compatible convention, approachable for open-source contributors | gRPC (rejected at this scale: adds tooling overhead with no real performance need) |
| 2026-08-06 | Server-Sent Events for dashboard real-time updates | Updates are one-directional; user actions already go through normal REST calls | WebSocket (rejected: unneeded bidirectional complexity for this use) |
| 2026-08-06 | htmx + server-rendered Go templates for the frontend | Matches the project's minimal-infrastructure philosophy; single-binary deployment via `embed.FS` | React SPA (rejected: separate build pipeline and dependency ecosystem not justified for this audience) |
| 2026-08-06 | Bearer token for agent authentication, presented by the agent at connect time | Simplicity, consistent with systemd env-file secret handling used elsewhere | mTLS (rejected: adds certificate issuance/rotation with no PKI need elsewhere in the design) |
| 2026-08-06 | PostgreSQL as the database; JSONB used natively for engine params and export config | JSONB is built into core Postgres; fits the variable per-engine parameter shape | A separate schema-per-engine approach (rejected: forces a migration per new engine) |
| 2026-08-06 | CLI-interactive first-run setup (`sparky setup`), not a web-based wizard | Avoids an unauthenticated network-exposed setup page; consistent with the SSH-is-the-trust-boundary model used throughout | Web-based setup wizard (rejected on security grounds) |
| 2026-08-06 | Own lightweight telemetry collection (nvidia-smi and /proc parsing) rather than adopting Prometheus/Grafana | Avoids duplicate collection since the agent already needs this data; Prometheus/Grafana don't map cleanly onto RBAC-gated dashboards or the NFS/S3 export requirement | Running Prometheus + Grafana alongside the app (rejected) |
| 2026-08-06 | `node_type` plus a `gpu_memory_gb` / `cpu_memory_gb` split added to Nodes | A Spark's unified memory is a special case (both fields equal) of the more general two-pool model needed for traditional workstations | Single `memory_capacity_gb` field (rejected once non-Spark hardware was in scope) |
| 2026-08-06 | One Docker-Engine-API-compatible backend serves both Docker and Podman; GPU passthrough standardized on CDI | Podman exposes a Docker-compatible socket; CDI is supported by both runtimes | Separate native-API implementations per runtime (rejected: unnecessary duplication) |
| 2026-08-06 | Helm chart supports `existingSecret` for bring-your-own secret management | App already reads all secrets from environment variables, so any external secret store that materializes into a Kubernetes Secret works without app-side integration code | Native Vault sidecar/CSI integration in the chart (rejected: speculative infrastructure for a product most users won't have) |
| 2026-08-06 | Monorepo to start; shared protocol code isolated in `internal/agentproto` | Protocol is still actively evolving; avoids premature multi-repo coordination and version-matrix overhead | Three repos (server, agent, common) from day one (deferred, not rejected - plan is to split once the protocol stabilizes) |
| 2026-08-06 | v0.1.0 targets non-Spark hardware first (Docker/Podman runtime backend, `node_type`, memory split, both engine adapters); Spark bare-metal support moved to v0.2.0 | Actual dev/test hardware in hand is a laptop (RTX 4090, 32GB RAM) and a Dell Precision (RTX 3080Ti); building and validating the core against accessible hardware before Spark-specific work is faster to iterate on | Spark-first sequencing as originally planned (superseded - non-Spark hardware is the more practical initial dev loop) |
| 2026-08-06 | Tests categorized as automated (CI-gated, hard pass/fail) versus manual (explicit checklist, operator-confirmed before release tagging) | Several critical paths (multi-node NCCL launches, CDI GPU passthrough) require real hardware and can't be safely faked in CI | Treating everything as CI-gated (rejected: would either block CI on hardware it doesn't have, or give false confidence by skipping these paths silently) |
| 2026-08-06 | Audit records always emitted as structured JSON to stdout (picked up by Filebeat or any shipper); optional active syslog/GELF push available on top for environments without a shipper. Local retention configurable up to 24 months | Day-to-day logging backend is Elasticsearch/OpenSearch via Filebeat; the stdout stream needs zero new code and works identically across bare metal (journald), Podman, and Kubernetes, letting any shipper (Filebeat, Fluentd, Vector) forward to Elasticsearch, OpenSearch, Graylog, or anywhere else | A syslog-push-only design (superseded - required active network client code for a problem a shipper already solves); native GELF-only integration (kept as an optional alternate protocol, not the default) |

---

## Known Issues and Technical Debt

| Issue | Severity | Deferred Because |
|-------|----------|-------------------|
| Mid-session behavior when a user loses AD access-group membership is undefined | Low | Not a stated priority during auth design; existing session likely persists until natural expiry |
| CDI GPU-passthrough hook behavior on Podman not yet verified on target hardware | Medium | Requires the actual Spark and Dell Precision hardware to test; noted as a known Podman-specific gotcha in research |

---

## Dependencies and Blockers

- Budget approval for a 4-node, 200GB/s switch is pending - blocks true 4-node
  clustering, but 2-3 node direct-cable clustering is unblocked and can proceed.
- Real DGX Spark and Dell Precision hardware needed to validate CDI/Podman GPU
  passthrough behavior before v0.4.0 can be considered complete.

---

## Future Ideas

- Native Vault sidecar/CSI integration in the Helm chart, if a concrete user need
  emerges (deliberately deferred - see Decisions Log)
- Additional engine adapters beyond vLLM/Aphrodite/llama.cpp as the ecosystem evolves
- Cross-node comparison views in the historical metrics dashboard
