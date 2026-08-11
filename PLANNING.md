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

**Status:** Implementation started (v0.1.0)

Architecture and design are complete (auth/permissions, full data model, component
boundaries, protocol design, tech stack, deployment tooling for bare metal, Podman,
and Kubernetes). The Go module and repository skeleton (both binaries' entry points
and environment configuration validation) is in place; the v0.1.0 milestone items
below are otherwise not yet built.

---

## Milestones

- [ ] **v0.1.0** - Core foundation (non-Spark first: laptop RTX 4090 / Dell Precision RTX 3080Ti as primary dev and test hardware)
  - [x] AD/LDAP bind auth (login-gate group), session handling
    - [x] Phase 1: Users repository (`internal/db`) - create, find-by-AD-SID,
          update last login, update tier (elevation), matching SCHEMA.md's
          `users` table. Complete when covered by integration tests against a
          real Postgres instance. Done - PR #5.
    - [x] Phase 2: LDAP identity provider (`internal/auth`) - service-account
          bind, user search within `LDAP_BASE_DN`, `LDAP_ACCESS_GROUP_DN`
          membership check via `LDAP_MATCHING_RULE_IN_CHAIN`, behind the
          identity-provider interface ARCHITECTURE.md describes, using
          `go-ldap/v3` per PLANNING.md's 2026-08-09 decision. Complete when a
          user's credentials can be verified and their login-gate group
          membership resolved, independent of any HTTP or session code.
          Done - login matches on `sAMAccountName`. Password verification
          uses a dedicated connection, separate from the service-account
          search connection, so a user's own (potentially more limited)
          directory permissions never replace the service account's for the
          group-membership lookup. Guards explicitly against LDAP's
          unauthenticated-bind pitfall (RFC 4513 SS5.1.2: a bind with a valid
          DN and an empty password succeeds without checking the password).
          Unit-tested against a fake LDAP connection, since standing up a
          real AD-compatible server isn't practical to do disposably.
    - [x] Phase 3: Session handling and HTTP wiring - `chi` router, login/logout
          handlers, signed-cookie session middleware, wiring Phase 1 and Phase 2
          together. Complete when a browser can complete a full login
          round-trip against a real (or fake) LDAP server and receive a valid
          session cookie.
          Done - `internal/session` is a hand-rolled HMAC-SHA256 signed
          cookie (no server-side store), matching the "own it, don't add
          dependencies you don't need" reasoning already on record for
          `chi`. `internal/httpapi` holds the router, `RequireSession`
          middleware (unused by any route yet - nothing to protect until
          RBAC/Dashboard UI land), and `LoginService`, which enforces the
          login-gate group check and provisions first-time users at
          `TierReadOnly`. Verified with a real `httptest` browser round
          trip (cookiejar, real Set-Cookie parsing) against a fake
          `IdentityProvider`, and separately smoke-tested the actual
          `sparky-server` binary end-to-end (real Postgres, unreachable
          LDAP, graceful SIGTERM shutdown) - login against a real AD/LDAP
          server itself is still unverified, same caveat as Phase 2.
  - [x] Users, RBAC tiers (Read-only/Developer/PowerDev/Admin), SuperAdmin break-glass
    - [x] Phase A: Permission overrides + break-glass storage (`internal/db`) -
          migration and repository for the `manage_model_store` capability
          grant (SCHEMA.md Permission overrides), and for the isolated
          break-glass credential row (SCHEMA.md Break-glass credential -
          password hash only, never a `Users` row). Complete when both are
          migrated and covered by integration tests, matching the Users
          repository's pattern from the auth work's Phase 1.
          Done - PR #10. This checkbox was left unchecked when that PR
          merged (its branch predated PR #9's Phase A/B/C text, so it
          couldn't touch this line, and the follow-up noted in the PR
          description never happened) - caught and fixed while syncing
          state against work done on another workstation.
    - [x] Phase B: `internal/rbac` - elevation-rule enforcement (who can
          promote whom, per SCHEMA.md Users Elevation rules), permission-
          override checks, and the SuperAdmin bypass short-circuit. Pure
          logic, testable without a database. Complete when tier changes go
          through rule checks instead of calling `UserRepository.UpdateTier`
          directly.
          Done - `CanElevate` and `CanManageModelStore` are pure functions
          (exhaustively table-tested across all tier pairs); `Service.
          ElevateTier` is the only sanctioned path to a tier change, since
          it looks up the target's current tier fresh, checks the rule,
          and only then persists. Resolved one real ambiguity in the
          Elevation rules text (demotion range) with the user: Admin
          demotion is single-step, symmetric with promotion. Also
          discovered and fixed a gap from the auth work's Phase 1:
          `UserRepository.UpdateTier`'s `elevatedBy` was a required
          `string`, which can't represent a SuperAdmin-made change (the
          SuperAdmin is not a `Users` row, so `elevated_by` must be able to
          store `NULL`) - changed to `*string`, verified the `NULL` actually
          persists against real Postgres.
    - [x] Phase C: SuperAdmin break-glass login - the
          `sparky set-superadmin-password` CLI subcommand, and a distinct
          login path in `internal/httpapi` for the SuperAdmin identity,
          which is not an LDAP/AD user and cannot go through the existing
          `LoginService.Login`. Complete when the manual test checklist
          item "Break-glass SuperAdmin login and first-Admin bootstrap flow"
          (ARCHITECTURE.md Testing Strategy) can actually be exercised.
          Done - `POST /login/break-glass` is a separate endpoint, not a
          special case inside the regular login handler, per the user's
          confirmed choice. Password hashing is a hand-rolled Argon2id
          wrapper (PHC-string-like encoding) around `golang.org/x/crypto/argon2`
          (already indirect via `go-ldap`); the CLI subcommand reads the
          password with no terminal echo via the newly-added
          `golang.org/x/term`. `internal/session` and `internal/httpapi`'s
          `RequireSession` now carry an `IsSuperAdmin` flag alongside
          `UserID`, matching `internal/rbac.Actor`'s existing shape.
          Verified as a genuine end-to-end flow, not just unit tests: ran
          the interactive CLI subcommand through a real pseudo-terminal
          (`golang.org/x/term`'s no-echo behavior needs a real tty, not a
          pipe), confirmed the hash actually persisted in Postgres, then
          started the real compiled server and logged in against that
          exact stored credential over HTTP - correct password, wrong
          password, and missing password all behaved correctly.
  - [x] `sparky setup` CLI first-run wizard
        Done - completion is inferred from whether the break-glass password
        has been set (`internal/db.BreakGlassRepository`), not a dedicated
        setup-state table - it was already exactly this codebase's one
        piece of database-resident, first-run-relevant state, and every
        other setting is already an env var validated on every boot.
        `internal/httpapi`'s `setupGate` middleware blocks every route with
        503 `SETUP_REQUIRED` until then; it checks the database only until
        it first observes setup complete, then caches that forever, so a
        `sparky-server setup` run in another terminal takes effect on the
        very next request with no restart needed - verified live, not just
        asserted: started the server unset-up (confirmed 503 on every
        route), ran `setup` interactively over a real pty against the
        still-running server, and confirmed the very next request passed
        the gate without a restart. `setup` and `set-superadmin-password`
        share the same prompt/hash/store logic; `setup` adds onboarding
        messaging and a database-readiness check (fails with a clear
        `migrate ... up` hint if the schema isn't there) and is safely
        re-runnable (resets the password, with a message noting it was
        already configured, rather than refusing).
  - [x] Audit log covering all state-changing actions, including SuperAdmin
        Done - migration `000004_create_audit_log` creates the `audit_log`
        table per SCHEMA.md; `internal/db.AuditRepository` is its
        append-only writer (no Update or Delete). `internal/audit.Recorder`
        is the single sanctioned path to it - writes the authoritative
        Postgres record, then additionally emits a structured JSON line
        (marked `"type":"audit"`) to a configured stream (stdout in
        production), matching ARCHITECTURE.md's always-on shipper-friendly
        stream. Wired into `internal/rbac.Service.ElevateTier` - the one
        state-changing action that already existed in the codebase and
        that SCHEMA.md itself uses as the audit log's own example
        (`elevated_user`) - covering both a regular Admin actor and the
        SuperAdmin (nil `actor_id`, `is_superadmin_action` true) in the
        same code path, so there is no separate SuperAdmin-only branch to
        accidentally exempt. Verified with real integration tests against
        Postgres (`internal/db`), including that the `000004` migration's
        `down` reverses cleanly, not just `up`.

        Deliberately out of scope for this pass, to keep it to what
        currently has a real caller rather than speculative plumbing: the
        `audit_settings` table (retention/forwarding config) and the
        active syslog/GELF push - see Decisions Log 2026-08-10. Also not
        audited: authentication events themselves (login, first-login
        user provisioning) - SCHEMA.md's own examples
        (`loaded_model`/`elevated_user`/`deleted_model_copy`) are all
        administrative actions on domain objects, not session/auth
        bookkeeping, and `last_login_at` already covers "did this user
        authenticate." The generic HTTP "Audit Middleware" ARCHITECTURE.md
        describes has no concrete shape yet either, since no
        state-changing HTTP handler exists to wrap - `internal/audit.
        Recorder` is the writer such a middleware (or any direct service
        call, as `rbac.Service` does today) would use once one exists.
  - [x] Node registry with `node_type` and the `gpu_memory_gb` / `cpu_memory_gb` split from the start
        Done - migration `000005_create_nodes` creates the `nodes` table
        per SCHEMA.md, with `node_type`/`container_runtime`/`agent_status`
        as Postgres enums and a `CHECK` constraint
        (`nodes_container_runtime_matches_type`) enforcing that
        `container_runtime` is set if and only if `node_type = docker-gpu`
        - database-level, not just application-level, matching the
        `break_glass_credential` singleton `CHECK` precedent.
        `fabric_group_id` is deliberately not part of this migration -
        Fabric groups doesn't exist until v0.3.0, so there is nothing yet
        for it to reference; it lands via an `ALTER TABLE` alongside that
        table. `registered_by` was made nullable from the start (unlike
        `elevated_by`, which needed a follow-up fix - see the Phase B
        entry above) since the same SuperAdmin-is-not-a-`Users`-row gap
        was obvious going in this time.

        `internal/db.NodeRepository` covers Create, FindByID, and List -
        List because a "registry" that cannot be enumerated isn't really
        one, unlike the audit log's Write-only scope. `rbac.CanManageNodes`
        (Admin or SuperAdmin, no permission-override path - node
        registration is infrastructure-level, not a per-user grantable
        exception) and `internal/nodes.Service.RegisterNode` are the
        single sanctioned path to a new node: RBAC check, then parameter
        validation (duplicating the database's `CHECK` constraint in Go
        for a specific error message instead of an opaque constraint
        violation), then persist, then audit (`registered_node` - the
        audit log's second real caller, after RBAC's `elevated_user`).
        No HTTP handler yet - same "logic layer ahead of HTTP wiring"
        precedent as RBAC Phase B and the audit log itself. Verified with
        integration tests against real Postgres, including that the
        `CHECK` constraint actually rejects a mismatched
        `node_type`/`container_runtime` pair and that the migration's
        `down` reverses cleanly.
  - [ ] Agent: Docker/Podman runtime backend (Docker-Engine-API-compatible), agent-initiated WebSocket, bearer token, CDI GPU passthrough
    - [x] Phase 1: Docker/Podman runtime backend (`agent/runtime/containers`) -
          `docker/docker/client` integration targeting either runtime's
          Docker-Engine-API-compatible socket, CDI GPU passthrough
          (`--device nvidia.com/gpu=all`), a start/stop container primitive.
          Self-contained - no protocol dependency on later phases, testable
          against a real local Docker/Podman daemon. Complete when a
          container can actually be started and stopped with GPU access
          via CDI, verified against real Podman, not just mocked.
          Done, with one honest exception to that original bar: the
          non-GPU container lifecycle (create, pull-if-missing, start,
          inspect, stop, remove) is fully verified against a real local
          Podman daemon, but CDI GPU passthrough itself is not - see the
          2026-08-10 Decisions Log entry and the updated Known Issues row.
          The actual import is `github.com/moby/moby/client` - the
          upstream project renamed from `docker/docker`, `go get
          github.com/docker/docker/client` fails outright now (also
          recorded in the Decisions Log and fixed in CLAUDE.md/
          docs/AGENT.md). `pullImage`'s not-found-then-retry logic and
          the CDI `DeviceRequests` construction are unit-tested against a
          fake client; the CDI request shape itself was confirmed correct
          against `moby/moby`'s own `daemon/cdi.go` source before being
          written, not guessed.
    - [x] Phase 2: `internal/agentproto` - shared WebSocket/JSON protocol
          message types (envelope with request ID per ARCHITECTURE.md
          Protocol, hello/auth handshake, heartbeat, error), used by both
          binaries. Pure types, no networking - testable via marshal/
          unmarshal round-trips alone.
          Done - `Envelope{Type, RequestID, Payload}` plus `Hello`/
          `HelloAck`/`Heartbeat`/`ErrorPayload` payload types and
          `NewEnvelope`/`DecodePayload` helpers. `DecodePayload` rejects
          unknown fields (`json.Decoder.DisallowUnknownFields`) so decoding
          a payload as the wrong type fails loudly instead of silently
          zero-filling non-overlapping fields - a real footgun for a
          multiplexed channel where `Type` and the Go type used to decode
          `Payload` can drift apart. No networking, bearer-token
          enforcement, or connection lifecycle yet - that's Phases 3-5.
    - [x] Phase 3: Node bearer token issuance and storage - extends the
          `nodes` schema with a hashed token column (same pattern as the
          break-glass credential: only the hash is stored). `RegisterNode`
          generates a random token at registration time and returns the
          plaintext once, for the Admin to put into that node's
          `SPARKY_BEARER_TOKEN` - standard API-token UX, confirmed with the
          user 2026-08-10 since nothing in the docs specified this before.
          Done - migration `000006_add_node_bearer_token` adds
          `nodes.bearer_token_hash text NOT NULL` (verified up and down
          against a real local Postgres instance, including the fresh
          `up` applying cleanly with zero existing rows to backfill).
          `internal/auth`'s new `GenerateNodeToken`/`HashNodeToken`/
          `VerifyNodeToken` use a `spk_`-prefixed 256-bit random token and
          plain SHA-256, not Argon2id - a memory-hard KDF defends against
          brute-forcing a low-entropy human password, which buys nothing
          against an already-uniformly-random 256-bit token, and would
          only add latency to every agent reconnect handshake. `Service.
          RegisterNode` now returns `(*db.Node, string, error)`, the
          string being the plaintext token shown this once.
          `NodeRepository.Create` takes the hash but `bearer_token_hash`
          is deliberately excluded from `nodeColumns`/`Node`, so it can
          never flow out through `FindByID`/`List` once those eventually
          back a Nodes dashboard. No token-verification lookup yet - that
          belongs to Phase 4, which is the first actual caller.
    - [x] Phase 4: Server-side Agent-Communication Layer - the WebSocket
          endpoint ARCHITECTURE.md's Component Breakdown describes as "the
          only component that speaks the agent protocol." Accepts inbound
          connections, validates the bearer token against Phase 3's stored
          hash, associates the connection with its node, tracks
          `agent_status` (`online`/`offline`/`unreachable`) against
          connection lifecycle. Uses `github.com/coder/websocket`
          (formerly `nhooyr.io/websocket`) - confirmed with the user
          2026-08-10 over `gorilla/websocket`, since it's actively
          maintained and minimal, matching the "own it, don't add
          dependencies you don't need" pattern already on record for
          `chi`/`internal/session`.
          Done - new `internal/agentconn` package: `Handler` (mounted at
          `GET /agent/connect`, outside `/api/v1` since it's a WebSocket
          upgrade, not REST) runs the hello/auth handshake against
          `internal/nodes.AuthService` (new - `db.NodeCredential`/
          `FindCredentialByName`/`SetAgentStatus`, the first real caller
          of Phase 3's stored hash), and `Registry` tracks which node
          owns which live connection (bookkeeping only - no send/dispatch
          API yet, since nothing generates real commands until Model
          profiles/Running instances exist). The success `hello_ack` is
          sent only after the node is registered and marked online, so
          the agent can never observe acceptance before this layer's own
          state reflects it - verified directly with a race-detector test
          that awaits the status-store call before asserting on it, not
          a sleep. A rejected handshake (unknown node name or wrong
          token) gets the same generic `hello_ack` reason either way, so
          it can't be used to enumerate node names. `agent_status` only
          tracks `online`/`offline` for now - `unreachable` (a connection
          that's open but has gone silent) needs a heartbeat timeout
          mechanism that has nothing to detect yet, since the agent side
          doesn't send heartbeats until Phase 5; tracked as a real,
          explicit gap below, not silently treated as covered. One-off
          verified end-to-end (then removed, not kept as a permanent
          test - the unit/integration tests already cover this logic in
          isolation) through the actual compiled `internal/httpapi`
          router: a real `RegisterNode` call issuing a real token against
          a real Postgres instance, dialed with a real `coder/websocket`
          client, confirming `agent_status` flips `online` then `offline`
          in the real database across connect/disconnect.
    - [ ] Phase 5: Agent-side connection goroutine - dial
          `SPARKY_CENTRAL_URL`, present `SPARKY_BEARER_TOKEN`, reconnect
          with backoff on disconnect, per docs/AGENT.md Service
          Architecture Notes. Wires Phase 1's runtime backend to commands
          received over Phase 2's protocol. No real command payloads exist
          yet beyond a stub - Model profiles and Running instances (later,
          separate v0.1.0 items) are what will actually generate
          container-start commands; this phase makes the connection itself
          real, the same "logic layer ahead of its eventual caller"
          precedent already used for RBAC Phase B, the audit log, and the
          node registry.
  - [ ] Model profiles: single-node only; vLLM (full-residency) and llama.cpp-style (partial-offload) adapters both from the start, with `requires_full_gpu_residency` - the laptop's 32GB RAM budget makes partial offload immediately relevant, not a later nice-to-have
  - [ ] Model transfers: Hugging Face download only (no peer replication yet)
  - [ ] Running instances: single-node load/unload
  - [ ] Metrics: live telemetry collection and dashboard (no historical retention yet)
  - [ ] Dashboard UI (htmx), sidebar nav, Read-only through Admin views
  - [ ] Bare-metal install script (apt + dnf)

- [ ] **v0.2.0** - Spark bare-metal support
  - [ ] Agent: bare-metal runtime backend for Spark (`serviceloop`, direct process exec)
  - [ ] `sparky-agent setup` subcommand - creates/verifies the `serviceloop` system
        account and its GPU-passthrough group membership (`video`/`render`,
        distro-dependent), idempotent, supports both environment-variable-driven and
        interactive invocation; `scripts/install.sh` is trimmed to placing the binary
        and systemd unit and delegates account provisioning to this subcommand rather
        than running `useradd`/`usermod` itself
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
| 2026-08-07 | `sparky-agent setup` is a subcommand of the agent binary itself, not bash-only logic in `scripts/install.sh` - it creates/verifies the `serviceloop` system account and its GPU-passthrough group membership | Mirrors the existing `sparky setup` precedent (CLI logic lives in the binary, not a web wizard or an external script); the provisioning logic gets real `go test` coverage and stays idempotent/re-runnable independent of the install script's version, versus duplicating `useradd`/`usermod` handling across bash's apt/dnf branches. Still requires root and still shells out to `useradd`/`usermod` under the hood - this centralizes that logic, it does not remove the privilege requirement | A standalone bash routine in `scripts/install.sh` handling account/group creation directly (rejected: no test coverage, logic duplicated per distro branch, can drift from what the binary itself expects at runtime) |
| 2026-08-09 | `chi` confirmed as the HTTP router, ahead of the AD/LDAP auth work's login handlers needing one | No new information changed the reasoning already recorded in CLAUDE.md's tech stack table (thin stdlib-based router, not a heavy framework) | None raised at confirmation time |
| 2026-08-09 | Signed cookie session confirmed as the session mechanism, ahead of the AD/LDAP auth work's session handling | No new information changed the reasoning already recorded in CLAUDE.md's tech stack table (fits the htmx server-rendered approach, no browser-side JWT handling) | None raised at confirmation time |
| 2026-08-09 | `github.com/go-ldap/ldap/v3` chosen as the LDAP client library for the identity-provider's on-prem AD implementation | Standard library has no LDAP support; go-ldap is the widely-used, actively maintained Go LDAP client and supports the `LDAP_MATCHING_RULE_IN_CHAIN` extended match this design already depends on for nested group resolution | None seriously considered - it is the de facto standard for Go |
| 2026-08-10 | Argon2id chosen for the break-glass credential hash, over bcrypt (SCHEMA.md left this open) | Current OWASP-recommended default for password hashing. `golang.org/x/crypto/argon2` is already an indirect dependency via `go-ldap`, so this promotes it to direct rather than adding anything new; its API is only the raw key-derivation function, so the salt/encode/verify wrapper is hand-rolled, matching the same reasoning already used for `internal/session` | bcrypt (rejected: `golang.org/x/crypto/bcrypt` has a more complete ready-made API, but is the older/second-choice algorithm per current guidance, and the credential this protects - unrestricted, AD-independent break-glass access - is exactly the case worth the extra code for the stronger default) |
| 2026-08-06 | Audit records always emitted as structured JSON to stdout (picked up by Filebeat or any shipper); optional active syslog/GELF push available on top for environments without a shipper. Local retention configurable up to 24 months | Day-to-day logging backend is Elasticsearch/OpenSearch via Filebeat; the stdout stream needs zero new code and works identically across bare metal (journald), Podman, and Kubernetes, letting any shipper (Filebeat, Fluentd, Vector) forward to Elasticsearch, OpenSearch, Graylog, or anywhere else | A syslog-push-only design (superseded - required active network client code for a problem a shipper already solves); native GELF-only integration (kept as an optional alternate protocol, not the default) |
| 2026-08-10 | `internal/audit.Recorder` built as a service other components call directly (`rbac.Service.ElevateTier` today), not as generic chi HTTP middleware, and `audit_settings` (retention, syslog/GELF forwarding) deferred rather than built alongside `audit_log` | No state-changing HTTP handler exists yet for a generic "wrap every handler" middleware to attach to - the only real state-changing action in the codebase so far (`ElevateTier`) is a service-layer call, same situation RBAC Phase B was built into ahead of any HTTP wiring. `audit_settings` has no consumer either: no retention-pruning job and no forwarding push exist to read it, and the always-on stdout stream (the actually load-bearing path per ARCHITECTURE.md) needs no settings row at all | Building the settings table and a stub forwarding path now regardless (rejected: speculative infrastructure with nothing to configure yet, same reasoning already on record against Vault sidecar/CSI integration below) |
| 2026-08-10 | Nodes' `fabric_group_id` left out of the v0.1.0 `nodes` migration entirely, to be added later by an `ALTER TABLE` alongside v0.3.0's `fabric_groups` table; `registered_by` made nullable from the start; `container_runtime`/`node_type` consistency enforced by a database `CHECK` constraint in addition to `internal/nodes` validation | A `REFERENCES fabric_groups (id)` FK cannot be created before `fabric_groups` exists, and clustering is explicitly out of v0.1.0 scope - no migration in this codebase has ever forward-referenced a not-yet-existing table. `registered_by` follows the exact shape of the `elevated_by` gap already fixed once (2026-08-09 RBAC Phase B entry above) - no reason to reintroduce the same bug knowingly. The `CHECK` constraint mirrors `break_glass_credential`'s singleton constraint: an invariant worth enforcing at the database level, not just trusting every future caller of `NodeRepository.Create` to get right | Adding an unconstrained nullable `fabric_group_id` column now (rejected: an unenforced FK is dead weight with no consumer until v0.3.0); leaving `registered_by` `NOT NULL` and simply disallowing SuperAdmin node registration (rejected: inconsistent with the project's "SuperAdmin is unrestricted, like root" stance already applied to `CanElevate` and `CanManageModelStore`) |
| 2026-08-10 | Node bearer tokens are generated by `RegisterNode` at registration time and returned to the caller once, plaintext; only a hash is stored | Nothing in the original design specified how a node gets its token at all. Generate-once-show-once is standard API-token UX (GitHub PATs, etc.) and mirrors the break-glass credential's already-established hash-only storage pattern, rather than inventing a new one | A separate "issue token" action distinct from registration (rejected: two steps for one concern, and registration without a usable token is a node that can't ever connect); storing the plaintext token, relying on `SPARKY_BEARER_TOKEN` file permissions alone (rejected: same reasoning that already put the break-glass credential behind a hash, not plaintext) |
| 2026-08-10 | `github.com/coder/websocket` (formerly `nhooyr.io/websocket`) chosen for `internal/agentproto`'s transport, over `gorilla/websocket` | Actively maintained and minimal API surface, matching the "own it, don't add dependencies you don't need" reasoning already on record for `chi` and `internal/session`'s hand-rolled cookie signing | `gorilla/websocket` (rejected: the long-time standard choice and still very widely used, but its maintenance status has been in flux - archived, then revived under a new org - a less certain footing for a protocol boundary this central to the whole system) |
| 2026-08-10 | `agent/runtime/containers` imports `github.com/moby/moby/client`, not `github.com/docker/docker/client` as CLAUDE.md's tech stack table originally said | The upstream project renamed from `docker/docker` to `moby/moby`; `go get github.com/docker/docker/client` fails outright (`module declares its path as: github.com/moby/moby/client`) - this is not a naming preference, it is the only path that resolves | None - not a real alternative, `docker/docker/client` simply does not exist as an importable module under that path anymore |
| 2026-08-10 | CDI GPU passthrough via Podman's Docker-Engine-API-compatible socket needs verification on the actual target Podman version before it can be trusted, tracked as a known gap rather than assumed to work | Empirically found while building Phase 1, against a real local Podman 4.9.3 daemon (no GPU hardware): Podman's own CLI resolves CDI-qualified device names correctly (`podman run --device nvidia.com/gpu=all` fails with a proper "unresolvable CDI devices" error - the expected failure with no CDI spec present, proving CDI parsing works), but neither Docker API mechanism for requesting CDI devices worked through the compat socket - `HostConfig.DeviceRequests` with `Driver: "cdi"` (confirmed correct against `moby/moby`'s own `daemon/cdi.go` source, so right for a real Docker daemon) was silently accepted and dropped with no error and no device (confirmed via `podman inspect`, which does not even have a `DeviceRequests` field to report), and `HostConfig.Devices` with a CDI name as `PathOnHost` was treated as a literal filesystem path and failed a plain `stat()`. Non-GPU container lifecycle (create, pull-if-missing, start, inspect, stop, remove) is unaffected and fully verified against the same real daemon | Chasing a workaround further in this environment (rejected: no GPU hardware and an old Podman version here make this untestable to a real conclusion either way - this is exactly the kind of gap ARCHITECTURE.md's existing manual test checklist item "CDI GPU passthrough verified on Podman" already anticipates needing real target hardware, not something to guess past) |

---

## Known Issues and Technical Debt

| Issue | Severity | Deferred Because |
|-------|----------|-------------------|
| Mid-session behavior when a user loses AD access-group membership is undefined | Low | Not a stated priority during auth design; existing session likely persists until natural expiry |
| CDI GPU passthrough via Podman's Docker-Engine-API-compatible socket not yet verified on target hardware/Podman version - neither Docker API mechanism for requesting a CDI device (`HostConfig.DeviceRequests` with `Driver: "cdi"`, or `HostConfig.Devices` with a CDI name as `PathOnHost`) triggered CDI resolution against a real local Podman 4.9.3 daemon, even though Podman's own CLI resolves CDI names correctly - see 2026-08-10 Decisions Log entry for the full empirical finding | Medium | Requires the actual target Podman version (likely much newer than the 4.9.3 available here) and real GPU hardware to determine whether this is fixed upstream or still needs a workaround; `agent/runtime/containers` implements the documented, correct Docker API contract regardless, which is right for Docker and the best available attempt for Podman |
| `agent_status` never reaches `unreachable` - `internal/agentconn` only ever sets `online` (on a successful handshake) or `offline` (on any disconnect, clean or not), even though SCHEMA.md's third state exists for a connection that's still technically open but has gone silent | Low | Detecting that case needs a heartbeat timeout, which needs the agent side to actually send heartbeats first - that's Phase 5 (agent runtime/WebSocket work), not yet built. Revisit once Phase 5 lands |
| `agent/config`'s `SPARKY_MODEL_STORAGE_PATH` has no default - docs/AGENT.md documents `/home/serviceloop/models` as the Spark default, but that depends on `SPARKY_NODE_TYPE` at runtime and isn't implemented yet | Low | No bare-metal runtime backend exists yet to consume this default; implement alongside the v0.2.0 backend and `sparky-agent setup` work |
| `rbac.Service.ElevateTier`'s tier update and its audit-log write are two separate calls, not one database transaction - a tier change can persist while its audit record fails to write (surfaced to the caller as an error after the fact, but not rolled back) | Low | No cross-repository transaction pattern exists anywhere else in the codebase yet to extend; the failure mode requires the audit Postgres write itself to fail immediately after a successful update, which is rare enough not to block this pass |

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
