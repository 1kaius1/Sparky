# Architecture

This document describes the technical architecture of Sparky. It is a stable
reference - update it when structural changes are made, not for every commit. Claude
Code should read this alongside `CLAUDE.md` at the start of every session.

The full data model lives in the companion document `SCHEMA.md` - referenced
throughout this file rather than duplicated.

---

## Overview

Sparky gives AD-authenticated developers self-service control over loading LLM
inference engines onto a fleet of GPU hosts, with tiered permissions, full audit
logging, and live and historical hardware telemetry. It is two Go binaries: a central
web application (REST/JSON API, Server-Sent Events for live updates, htmx
server-rendered frontend) and a lightweight agent that runs on every compute node -
NVIDIA DGX Spark hardware and generic Docker/Podman GPU machines alike. The agent
dials out to the central app and holds a persistent WebSocket connection open; the
central app never initiates a connection into a compute node. Postgres is the single
source of truth for all persistent state. Hard constraints: compute nodes require
zero inbound network exposure, and the application must run identically on bare-metal
Linux (systemd), Podman, and Kubernetes.

---

## Application Lifecycle

### Central app (`cmd/sparky-server`)

```
Start
 |
 v
[Entry Point]
 |
 v
[Config / Env Validation]      <-- fail fast if required env vars are missing
 |
 v
[Setup Check]                  <-- if minimal config doesn't exist, refuse to
 |                                  serve normal routes; direct operator to
 |                                  `sparky setup` (see Security Considerations)
 v
[Database Connection Pool]     <-- Postgres, verified before proceeding
 |
 v
[Middleware Stack Init]        <-- request ID, logging, recovery, auth, audit
 |
 v
[Route Registration]           <-- all routes registered before accepting traffic
 |
 v
[Start HTTP Listener]          <-- REST + SSE
 |
 v
[Block on Signal]              <-- SIGTERM / SIGINT triggers shutdown
 |
 v
[Graceful Shutdown]            <-- drain in-flight requests, close pool, exit
 |
 v
exit(0)
```

### Agent (`cmd/sparky-agent`)

```
Start
 |
 v
[Entry Point]
 |
 v
[Config / Env Validation]      <-- central app address, bearer token, node identity
 |
 v
[Runtime Backend Init]         <-- bare-metal exec, or Docker/Podman API client
 |
 v
[Telemetry Collector Init]     <-- nvidia-smi + /proc readers
 |
 v
[Dial Central App]             <-- opens persistent WebSocket, presents bearer token
 |
 v
[Command Loop]                 <-- receive commands, send telemetry/progress,
 |                                  reconnect with backoff on disconnect
 v
[Block on Signal]
 |
 v
[Graceful Shutdown]            <-- stop managed processes cleanly, close connection
 |
 v
exit(0)
```

### Signal Handling

| Signal  | Behavior                                                      |
|---------|----------------------------------------------------------------|
| SIGTERM | Graceful shutdown - drain in-flight work, then exit           |
| SIGINT  | Graceful shutdown - same as SIGTERM                            |

---

## Component Breakdown

### Central app

#### Entry Point
Reads environment, runs the setup check, initializes subsystems, registers routes,
starts the listener. No business logic - composition root only.

#### Configuration
Reads all config from environment variables, per twelve-factor convention. Validates
required values at startup; fails fast on anything missing or invalid. No other
package reads the environment directly.

#### Auth & Identity Provider
On-prem AD via LDAP bind today, behind an identity-provider interface so an OIDC/
Entra ID implementation can be added later without touching anything downstream of
"authenticated user + resolved group memberships." Login itself is gated by
membership in a dedicated AD group, separate from the tier-defining groups. Nested
group membership is resolved server-side via AD's `LDAP_MATCHING_RULE_IN_CHAIN`
extended match, not walked recursively in application code.

#### RBAC & Permission Overrides
Resolves a session's tier (see `SCHEMA.md` Users), evaluates elevation rules, checks
Permission overrides for the download/delete grant, and short-circuits every check
for the SuperAdmin - the one identity in the system that bypasses authorization
entirely, the way root bypasses Unix permission bits.

#### Audit Middleware
Cross-cutting, not a service other components call directly. Wraps every
state-changing handler and writes to Audit log (`SCHEMA.md`) - including SuperAdmin
actions, which bypass the RBAC check but never bypass the audit write.

#### Node & Fabric Registry
CRUD for Nodes and Fabric groups. Fabric group membership is an explicit, manually
declared Admin action (the app cannot sense physical cabling) - see `SCHEMA.md`
Fabric groups.

#### Model Profile Management
CRUD for Model profiles and Profile cluster nodes. Validates that a clustered
profile's node set actually belongs to its declared fabric group, and that
`engine_params` matches the shape the selected engine adapter expects.

#### Model Lifecycle Orchestrator
Owns "load" and "unload." Computes launch eligibility per node - Green (in cluster,
model present), Blue (in cluster, no model, but room to copy from another node -
offers a source list before proceeding), Red/Gray (not in cluster, or no model and no
room) - re-checking current Fabric group membership at launch time, not just at
profile-creation time. On a Red node, prompts rather than blocks: the operator can
choose to launch at reduced capacity (provided the head node specifically is not the
Red one) and relaunch at full capacity later once the missing node returns.
Translates the result into commands sent to the relevant agent(s) via the
agent-communication layer, and writes the outcome to Running instances and Running
instance nodes.

#### Model Transfer Orchestrator
Handles both Hugging Face downloads and peer-to-peer rsync replication over the
cluster link, writing to Model transfers and updating Node model inventory on
completion. Computes the free-space side of the Blue-state eligibility check.

#### Metrics Ingestion & Retention
Polls agents for telemetry on an interval, writes to Metrics, and runs two background
jobs: downsampling raw data older than 6 months into aggregates, and exporting those
aggregates to the configured NFS or S3 destination (Metrics export config).

#### HTTP/API + Dashboard
REST/JSON for all actions, Server-Sent Events for live telemetry and transfer
progress push to the dashboard. Frontend is server-rendered Go templates with htmx
partial swaps - see Frontend section below.

#### Agent-Communication Layer
The only component that speaks the agent protocol (see Protocol below). Every other
central-app component that needs something to happen on hardware goes through this
layer rather than managing WebSocket connections itself.

### Agent

#### Runtime Backends
Pluggable, selected by a node's `runtime_backend` (`SCHEMA.md` Nodes) - not tied to
any particular hardware class:
- **Bare-metal**: execs the engine process directly, running as the `serviceloop`
  system account for direct filesystem access to model storage. Used when GPU
  passthrough isn't viable - e.g. a single-GPU workstation already using that GPU
  for its own host session, not something specific to DGX Spark hardware. A
  Spark's GB10 GPU supports passthrough to a container without affecting a
  display connected to it (NVIDIA's supported use case for that hardware), so a
  Spark can use the Docker/Podman backend below just as well.
- **Docker/Podman**: targets the standard Docker Engine API against either runtime's
  socket - Podman exposes a Docker-Engine-API-compatible socket, so one
  implementation serves both. GPU passthrough is standardized on CDI
  (`--device nvidia.com/gpu=all`) across both runtimes rather than Docker's classic
  `--gpus`/`--runtime nvidia` flag, since CDI is now supported by both.

#### Telemetry Collector
Reads `nvidia-smi` and `/proc/stat`, `/proc/meminfo`, `/proc/diskstats` directly -
the same technique proven by existing Spark-hardware Prometheus exporters, and one
that generalizes to any Linux box with an NVIDIA GPU without modification.

#### Transfer Executor & Local Store Manager
Executes downloads and rsync replications, writing to node-local storage
(`/home/serviceloop/models/` on a bare-metal host; a per-`runtime_backend`
configurable path on Docker/Podman hosts). Deletion of a local model copy to free
space is a distinct action from unloading a running instance - see `SCHEMA.md`
Permission overrides.

---

## Protocol

| Boundary | Protocol | Notes |
|---|---|---|
| Browser <-> Central app | REST/JSON + Server-Sent Events | SSE chosen over WebSocket - updates flow server-to-client only; actions are ordinary REST calls |
| Central app <-> Agent | Agent-initiated persistent WebSocket, JSON messages | Agent dials out and holds the connection open; central app never connects inbound to a compute node. Bearer token presented by the agent at connect time. Request-ID field on messages provides correlation over the shared channel |

REST/JSON was chosen over gRPC for both boundaries: consistency with vLLM's own
OpenAI-compatible convention, and approachability for an open-source audience (no
protobuf toolchain required to inspect traffic).

The agent WebSocket endpoint is `GET /agent/connect` - deliberately outside
`/api/v1` (CLAUDE.md API Conventions), since it's a WebSocket upgrade
authenticated by the node's own bearer token, not a REST call authenticated by a
session cookie. This is what `SPARKY_CENTRAL_URL` (docs/AGENT.md Configuration)
resolves to. The first message the agent sends after the upgrade must be a
`hello` envelope (`internal/agentproto`) carrying its node name and bearer
token; the central app replies with a `hello_ack` either way, using the same
generic rejection reason for an unknown node name and a wrong token, so the
handshake can't be used to enumerate registered node names.

---

## Request Lifecycle

### Browser to central app

```
Browser -> Reverse Proxy (TLS termination) -> HTTP Listener
        -> Middleware Chain (request ID, logging, recovery, auth, audit)
        -> Router -> Handler -> Service Layer -> Repository -> Postgres
        -> (response flows back) -> HTTP Response
```

### Central app to agent

```
Service Layer -> Agent-Communication Layer -> WebSocket message (JSON, request ID)
              -> Agent Command Loop -> Runtime Backend / Transfer Executor / Telemetry
              -> WebSocket response/stream -> Agent-Communication Layer -> Service Layer
```

---

## Logging

- stderr; `text` in development, `json` in production
- Every log line within a request includes the request/correlation ID
- Never log: passwords, tokens, the break-glass credential, full request bodies with
  sensitive fields

---

## Error Handling

- Unhandled errors caught by recovery middleware; logged at CRITICAL; return 500
- 4xx logged at INFO; 5xx logged at ERROR with full context
- Raw database errors never exposed to the client
- Agent-side failures (e.g. a launch that doesn't fit in available memory) surface
  through Running instances' `error_message` field, not a generic 500 - the operator
  needs to know *why*, since resolving it means editing the profile's parameters

### HTTP Status Code Conventions

| Situation                               | Status Code |
|-----------------------------------------|-------------|
| Success (with body)                     | 200         |
| Created                                 | 201         |
| Success (no body)                       | 204         |
| Validation error                        | 400 / 422   |
| Unauthenticated                         | 401         |
| Unauthorized (no permission)            | 403         |
| Not found                               | 404         |
| Conflict (duplicate, constraint)        | 409         |
| Internal server error                   | 500         |
| First-run setup not complete            | 503         |

---

## Security Considerations

- Login gated by a dedicated AD group, separate from tier-defining groups; nested
  membership resolved via `LDAP_MATCHING_RULE_IN_CHAIN`
- Permission tiers are internal app state, not derived live from AD on every request
- SuperAdmin (break-glass) credential is isolated - stored so only the application
  process can read or validate it, set via an interactive CLI subcommand
  (`sparky set-superadmin-password`), never through the web UI
- `/login/break-glass` (both its browser form and its JSON API contract) can
  optionally be restricted to a configured IP/CIDR allowlist
  (`BREAKGLASS_ALLOWED_IPS`) - off by default, allowing from anywhere, same as every
  other optional security control in this project
- Every state-changing action is audited with no exceptions, including SuperAdmin's
- Agent authentication: bearer token, presented by the agent when it dials out.
  Chosen over mTLS for simplicity, consistent with the secret-handling approach below
- Secrets are read exclusively from environment variables everywhere - never a
  product-specific secret-store SDK compiled into the app. This is what makes every
  deployment target below work identically and what makes the app compatible with
  any external secret manager (Vault, cloud KMS-backed secrets) without any
  Vault-specific code: those tools all ultimately materialize a secret as something
  that looks like a normal environment source
  - **Bare metal**: a restrictive-permission env file (`chmod 0600`, owned by a
    dedicated service account), loaded via systemd's `EnvironmentFile=`. Root and the
    owning service account can read it; nothing else can
  - **Podman/Kubernetes**: Kubernetes Secrets, or Podman's equivalent. The Helm chart
    supports an `existingSecret` value so anyone running Vault, External Secrets
    Operator, or similar can point the chart at a Secret their own tooling manages,
    rather than only supporting inline values
- First-run setup is CLI-only (`sparky setup`), not a web wizard - avoids exposing an
  unauthenticated configuration surface on the network before setup completes.
  Requires shell access, matching the trust boundary ops already operates in via SSH
- GPU passthrough via CDI on both Docker and Podman - known gotcha: CDI hooks can
  behave differently on Podman when writing to a read-only-mounted filesystem; verify
  on target hardware (see `PLANNING.md` Open Questions)
- Model file lifecycle stays inside the app - sysadmin SSH access is for host-level
  concerns outside the app's managed storage, not for manually touching model files,
  so the database can stay authoritative without filesystem-reconciliation logic

---

## Deployment Model

Three supported targets, one binary each, sharing the same environment-variable
configuration surface:

- **Bare metal**: one-script installer (apt and dnf both supported), dedicated
  purpose-built host assumption for the central app, systemd units for both binaries,
  Postgres installed locally by the script or pointed at a remote instance via config
- **Podman**: preferred runtime for compute nodes using the Docker/Podman backend
  (`SCHEMA.md` Nodes' `runtime_backend`); Docker-Engine-API compatibility means the
  agent's container-management code does not fork per runtime
- **Kubernetes**: Helm chart controlling the same parameters `sparky setup` would
  prompt for interactively; `existingSecret` support for bring-your-own secret
  management

Central app and its database always run on separate infrastructure from the compute
nodes - never on a Spark - so the break-glass recovery path stays independent of the
hardware it might be needed to fix, and so ops's SSH access to the control plane isn't
also SSH access to a GPU node mid-workload.

---

## Testing Strategy

Tests are categorized as automated (CI-gated) or manual (required sign-off before
release tagging). This split exists because several critical paths - multi-node GPU
coordination, CDI passthrough - cannot be exercised without real hardware and aren't
safe to fake convincingly in CI.

### Automated Tests (CI-gated)

All automated tests must pass before a merge or a release tag. This is a hard gate -
never suggest a PR or a tag with a failing automated test.

#### Unit Tests
Service layer with mocked repositories and a mocked agent-communication layer; no
real database or network.

#### Feature / Integration Tests
Full request path against a real test database. Agent-protocol tests run against a
fake agent implementation that speaks the WebSocket protocol without touching real
hardware.

### Manual Tests (required before release tagging)

Not run in CI, and not implied by automated tests passing. Each item below must be
explicitly confirmed by the releasing operator before a version is tagged.

- [ ] CDI GPU passthrough verified on Docker, target distro(s)
- [ ] CDI GPU passthrough verified on Podman, target distro(s) - including the known
      read-only-filesystem hook behavior
- [ ] Multi-node NCCL/MPI launch verified on physically linked Sparks (2+ nodes)
- [ ] Peer-to-peer rsync replication verified over the real cluster link
- [ ] Partial-GPU-offload engine (llama.cpp) verified on memory-constrained hardware
      (the 32GB RAM laptop target)
- [ ] Break-glass SuperAdmin login and first-Admin bootstrap flow
- [ ] `sparky setup` CLI wizard verified end-to-end on a fresh host, both apt and dnf
      targets
- [ ] Reduced-capacity launch and relaunch-at-full-capacity flow verified on real
      hardware

---

## Audit Log

Every state-changing action taken by any user - including the SuperAdmin - appends a
record to the audit log. Records are immutable. See `SCHEMA.md` Audit log for the
exact column layout.

Not recorded: read/view actions (dashboard polling, listing resources). This was a
deliberate scope decision - logging every dashboard refresh would drown out
meaningful signal for no real accountability benefit.

Retention: configurable, up to 24 months of local storage in Postgres (see
`SCHEMA.md` Audit settings).

### Long-Term Forwarding

Every audit record is emitted as a structured JSON log line (distinguishable from
general app logs) to the same stdout stream the app already writes JSON logs to in
production. This costs nothing extra to implement and needs no configuration: it
means a log shipper already watching that output - Filebeat via journald on bare
metal, via container log tailing on Podman, or via its standard DaemonSet pattern on
Kubernetes - picks up every audit record automatically, and ships it onward to
Elasticsearch, OpenSearch, Graylog, or anything else, without Sparky knowing or
caring which. Shipping, buffering, retry, and backpressure stay the shipper's job,
not the app's - Sparky's responsibility stops at writing well-structured logs.

Optionally, Audit settings can additionally enable active network-push forwarding
(syslog by default, GELF as an alternate) directly to a configured destination - for
environments that want Sparky to push straight to something like Graylog without
running a shipper at all. This is the secondary path, not the default; the stdout
stream above already covers the common case.

Forwarding is fire-and-forget and additive either way - it never gates or delays the
local write to Postgres, which remains authoritative. A forwarding failure is logged
but does not fail the action being audited.

---

## Deployment Topology

### Tenancy Model

- Model: single-tenant - one Sparky instance per organization
- Data isolation: not applicable - there is no cross-tenant concern by design (see
  Non-Goals in `PLANNING.md`)

### Scaling Model

- Horizontal scaling: not supported - single central-app instance. At the scale this
  is designed for (a handful of compute nodes), this is not a constraint
- Shared state: Postgres is the only shared state; no cache layer
- Static assets: embedded in the binary via `embed.FS`, served directly - no CDN

---

## Extension and Integration Points

- Public API: internal only at this time - the REST API serves the htmx frontend and
  is not currently published as a versioned external contract
- Event stream: Server-Sent Events for live telemetry and transfer progress, internal
  use only
- **Engine adapters**: the primary extension mechanism for what Sparky can run.
  vLLM and Aphrodite (full GPU residency) and llama.cpp-style engines (partial
  offload) ship first; adding another engine is implementing the adapter interface,
  not a schema change
- **Runtime backends**: the second extension mechanism, for what Sparky can run
  *on*. Bare-metal and Docker/Podman ship first; both speak to the same
  agent-communication layer
- Inbound/outbound webhooks: none

---

## Known Limitations

- No caching layer - all reads hit Postgres directly. Acceptable at the target scale
- Mid-session behavior when a user is removed from the AD access group is undefined -
  an existing session likely persists until natural expiry (see `PLANNING.md`)
- No historical metrics until v0.3.0 - only live telemetry in earlier milestones
- CDI GPU-passthrough behavior on Podman not yet verified against real target
  hardware

---

## Future Architectural Considerations

- OIDC/Entra ID as a second identity provider, activating the migration path the
  identity-provider interface was built for
- Additional engine adapters as the inference-serving ecosystem evolves
- Native Vault integration in the Helm chart, if a concrete need emerges (see
  `PLANNING.md` Future Ideas - deliberately not built speculatively)
