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

**Status:** v0.1.0 substantially complete; v0.2.0 well underway - see Milestones
below for the authoritative, phase-level detail; this is a summary only.

Architecture and design are complete. Built and merged: AD/LDAP bind auth and
session handling; Users/RBAC tiers/SuperAdmin break-glass; the `sparky setup` CLI
wizard; the audit log; the node registry; the full agent runtime/WebSocket stack
(Docker/Podman runtime backend, `internal/agentproto`, node bearer tokens, the
server-side Agent-Communication Layer, and the agent-side connection goroutine -
all five phases done, though the top-level checklist item stays unchecked pending
real-hardware CDI GPU passthrough verification, a known gap, not an oversight); and
Model profiles (schema, engine adapters, RBAC-gated CRUD service). Model transfers
is done, all four phases (`model_transfers` + `node_model_inventory` schema and
repositories; `internal/agentproto`'s `TypeStartTransfer`/`TypeTransferProgress`
and `internal/agentconn`'s `Registry.Send`/`Handler.OnMessage`; `agent/transfer`'s
native Hugging Face downloader wired into `agent/connection.Conn`'s dispatch;
`internal/transfers.Service`'s RBAC-gated `InitiateTransfer` and its
`HandleTransferProgress` `OnMessageFunc` callback). Running instances is also done
(single-node load/unload - `running_instances` schema, engine adapters' translation
into an image/launch command, `TypeLoadInstance`/`TypeUnloadInstance`/
`TypeInstanceResult` protocol and agent-side dispatch, `internal/lifecycle.Service`'s
RBAC-gated `LoadInstance`/`UnloadInstance`). Metrics is also done (live
telemetry collection and ingestion, no historical retention yet -
`agent/telemetry.Collector`, `TypeTelemetry`, `internal/metrics.Service`).
Dashboard UI Phases 1-10 are done (base layout/sidebar shell,
session-gated routing, all eight sidebar sections have a working
read-only page - Dashboard overview, Nodes, Model profiles, Transfers,
Metrics, Audit log, Users & permissions, Settings; a real HTML login page
and a logout control; three write/action forms - Users & permissions
tier changes, node registration, model profile create/edit - no SSE
wiring yet). Two new singleton config tables (`metrics_export_config`,
`audit_settings`) were migrated as part of Phase 6, closing a gap where
SCHEMA.md had long documented their shape but neither had ever actually
been created; Phase 7 similarly gave `internal/metrics.Service` its first
real production caller (`Chart.js`, vendored per ARCHITECTURE.md's no-CDN
policy, is the Metrics page's chart library); Phases 8, 9, and 10 gave
`rbac.Service.ElevateTier`, `nodes.Service.RegisterNode`, and
`profiles.Service.CreateProfile`/`UpdateProfile` - all fully built and
tested well before this Dashboard UI milestone started - their first
HTTP callers; Phase 11, the milestone's last phase, gave
`lifecycle.Service.LoadInstance`/`UnloadInstance` their first HTTP
callers (the instance load/unload controls on the Model profiles page),
combined the three services' `OnMessageFunc`-shaped handlers into the
single dispatching callback `cmd/sparky-server/main.go` had been passing
`nil` for, and added SSE wiring (a new `internal/events` broker plus
`GET /events`) so the browser reflects a load/unload/transfer/telemetry
change without a manual reload. Dashboard UI is now fully done - all
eleven phases. v0.1.0's only remaining item is real-hardware CDI GPU
passthrough verification for the Docker/Podman runtime backend, blocked
on DGX Spark hardware not yet in hand - a known, documented gap, not
unfinished work.

v0.2.0 is well underway. Bare-metal packaging (`.deb`/`.rpm`/tarball,
`scripts/build_packages.sh` via `nfpm`) is done. The bare-metal runtime
backend itself (`agent/runtime/baremetal`) is done, including real-hardware
validation - not just built and unit-tested, but actually run: a real
`llamacpp` profile loaded as a genuine child process of `sparky-agent`
running as `serviceloop` on this project's own RTX 4090 laptop, GPU
offload confirmed via `nvidia-smi`, a real inference request served, and
clean unload/shutdown confirmed - see the 2026-08-14/2026-08-15 Decisions
Log entries for the two real bugs that validation pass found and fixed
(the `/opt/sparky/serviceloop` home-directory fix, `KillMode=mixed`). The
`sparky-agent setup` subcommand is also done, with its own real-hardware
verification. Engine-binary provisioning from GitHub Releases
(`agent/enginetransfer`, `internal/engineprovision`) is also done - real
test coverage throughout, though not yet exercised against an actual
published release tarball on real hardware, and with no HTTP/UI surface yet
(logic and agent-side mechanics only, matching this project's own repeated
"logic before HTTP wiring" precedent) - see the 2026-08-15 Decisions Log
entries. Per-profile engine version pinning - the follow-up deliberately
scoped out of that provisioning work - is also done: a nullable
`model_profiles.engine_version` column plus agent-side launch-time
resolution (`agent/connection.resolveEngineBinaryPath`), letting two
otherwise-identical profiles pin different installed versions for direct
comparison, with zero behavior change for any unpinned profile - see the
new dated Decisions Log entry. v0.2.0's only remaining item is the same
Docker/Podman-on-Spark CDI validation v0.1.0 is waiting on - see Current
Sprint / Active Work below.

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
    - [x] Phase 5: Agent-side connection goroutine - dial
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
          Done - new `agent/connection.Conn`: dial (with a 10s per-attempt
          timeout), the same hello/auth handshake internal/agentconn
          expects, a read loop, and reconnect with exponential backoff
          (1s to 30s, equal jitter, reset to 1s after any handshake that
          got accepted). Also sends a `heartbeat` envelope every 30s over
          the established connection - closing the loop this package's
          own Phase 4 Known Issues note committed to, though the
          server-side `unreachable` detection that would actually consume
          those heartbeats is still open (updated below). `dispatch`
          recognizes `heartbeat`/`error` (the only message types that
          exist today beyond `hello`/`hello_ack`) and logs anything else -
          `agent/runtime/containers.Backend` is wired in as a field ready
          for a real command type to call into, but nothing routes to it
          yet, matching the phase's stated scope. Verified end-to-end
          (ad hoc, not committed) with the real compiled `sparky-server`
          and `sparky-agent` binaries over a real TCP socket: connect,
          `agent_status` -> `online` in real Postgres, `SIGTERM` ->
          clean shutdown -> `agent_status` -> `offline`, then a second run
          with the server killed mid-connection showing real backoff
          growth (768ms, 1.2s, 2.6s, 4.4s, 15.3s) and a successful
          reconnect once the server came back.

    This top-level item stays unchecked even with all five phases done:
    its own stated scope includes CDI GPU passthrough, and that specific
    piece was never verified working (Phase 1's gap - see the 2026-08-10
    Decisions Log entry and Known Issues). Check it off once that's
    resolved, not before - the phase breakdown existing doesn't mean the
    original bullet's full claim is true yet.
  - [x] Model profiles: single-node only; vLLM (full-residency) and llama.cpp-style (partial-offload) adapters both from the start, with `requires_full_gpu_residency` - the laptop's 32GB RAM budget makes partial offload immediately relevant, not a later nice-to-have
    - [x] Phase 1: `model_profiles` schema + `internal/db.ProfileRepository` -
          matching SCHEMA.md Model profiles, Create/FindByID/List/Update/Delete.
          `fabric_group_id` is not part of this migration, same reasoning as
          `nodes.fabric_group_id` (Fabric groups doesn't exist until v0.3.0).
          Unlike that precedent, `topology` is declared with both
          `single_node` and `clustered` values up front (cheap - it's just an
          enum, not a FK to a nonexistent table, same reasoning as
          `agent_status` shipping `unreachable` before anything produces it) -
          a `CHECK` constraint (`model_profiles_single_node_only`) is what
          actually enforces v0.1.0's single-node-only scope, rejecting any row
          where `topology <> 'single_node'` or `target_node_id IS NULL`. A
          future v0.3.0 migration relaxes that constraint once
          `fabric_group_id` exists to hold the alternative. Complete when
          covered by integration tests against a real Postgres instance,
          including that the `CHECK` constraint rejects a `clustered`
          topology and that the migration's `down` reverses cleanly.
          Done - migration `000007_create_model_profiles`, verified up and
          down against a real local Postgres instance. `engine_params` is
          `jsonb NOT NULL DEFAULT '{}'::jsonb` (not nullable - it always
          represents real launch configuration, unlike `audit_log.detail`,
          which may legitimately be absent). `Update` takes the same
          mutable fields as `Create` (name, model_ref, engine_type,
          engine_params, requires_full_gpu_residency, required_memory_gb,
          target_node_id, port) - `topology` isn't updatable in v0.1.0,
          matching the CHECK constraint. `FindByID`/`Update`/`Delete`
          return `ErrProfileNotFound` unwrapped (not `fmt.Errorf("...: %w"`),
          matching `NodeRepository.FindByID`'s style, since it's an
          expected outcome calling code compares against directly, not an
          edge case. 13 integration tests, including that the CHECK
          constraint rejects both a `clustered` topology and a null
          `target_node_id` by direct `INSERT` (bypassing the repository
          entirely, to confirm the database enforces this regardless of
          caller discipline, not just that the Go API happens to never
          construct such a row).
    - [x] Phase 2: `internal/engines` - the adapter interface and registry
          (CLAUDE.md: "engines/ - Pluggable adapters"), plus two concrete
          adapters, vLLM and llama.cpp-style (Aphrodite is v0.3.0 per this
          milestone file's own split - see the v0.3.0 section below). An
          adapter validates `engine_params` for its `engine_type` and reports
          `requires_full_gpu_residency` (`true` for vLLM, `false` for
          llama.cpp) - see SCHEMA.md Model profiles. Deliberately does not
          translate a profile into an actual launch command yet - that's the
          Model Lifecycle Orchestrator's job (ARCHITECTURE.md Component
          Breakdown), which belongs to the separate "Running instances"
          v0.1.0 item below, not this one. Pure validation logic, no
          networking or container calls - testable via unit tests against
          valid/invalid `engine_params` payloads for both adapters.
          Done - `Adapter{RequiresFullGPUResidency() bool,
          ValidateParams(json.RawMessage) error}`, a `Registry` mapping
          `db.ProfileEngineType` to its `Adapter`
          (`db.ProfileEngineAphrodite` has none, returns
          `ErrUnknownEngineType`). Each adapter validates only the
          handful of keys Sparky recognizes when present (type/range
          checks) and passes everything else through unvalidated -
          `engine_params` stays deliberately opaque beyond that, per
          SCHEMA.md. llama.cpp's recognized keys (`n_gpu_layers`,
          `ctx_size`, `threads`) were confirmed against a real
          `llama-server --help` (the same
          `ghcr.io/ggml-org/llama.cpp:server` image `agent/runtime/
          containers` was verified against), not guessed. vLLM's
          (`tensor_parallel_size`, `gpu_memory_utilization`, `dtype`,
          `quantization`, `max_model_len`) reflect well-established,
          long-stable vLLM CLI arguments but were not verified against a
          live install here - vLLM's CUDA/torch dependency chain wasn't
          practical to install in this environment - an honest
          confidence gap between the two adapters, not glossed over.
    - [x] Phase 3: `internal/profiles.Service` - RBAC-gated (new
          `rbac.CanManageProfiles`: PowerDev, Admin, or SuperAdmin: see
          CLAUDE.md Frontend Conventions, Model profiles' sidebar tier
          "PowerDev create" - no permission-override path, same precedent as
          `CanManageNodes`) CRUD orchestration: validate params via Phase 2's
          adapter registry, confirm `target_node_id` refers to a real node
          (`internal/nodes`), persist via Phase 1, audit every create/update/
          delete. No HTTP handler yet, same precedent as the node registry
          and RBAC Phase B. Complete when covered by unit tests against fakes
          for the repository, adapter registry, node lookup, and audit
          dependencies, exercising RBAC denial, adapter validation failure,
          and the happy path for both engine types.
          Done - `CreateProfile`/`UpdateProfile`/`DeleteProfile`, all
          RBAC-gated the same way. `RequiresFullGPUResidency` is
          deliberately absent from `profiles.Fields` (the input struct) -
          it's derived from the resolved adapter, not caller-supplied,
          so it can never disagree with `EngineType`. A shared `resolve`
          step (field validation, adapter lookup + `ValidateParams`,
          target-node existence check) backs both Create and Update,
          since the same checks apply either way. `UpdateProfile`/
          `DeleteProfile` propagate `db.ErrProfileNotFound` unwrapped,
          matching `ProfileRepository`'s own style. 18 unit tests against
          fakes, including one using the real `engines.Registry` (not a
          fake) for both vLLM and llama.cpp end to end through the
          service, per this phase's own completion bar.

    All three phases done - checking off the top-level "Model profiles"
    item above. Unlike the agent runtime/WebSocket item, there's no
    similar functional gap blocking this: the vLLM adapter's parameter
    validation wasn't verified against a live install (Phase 2's honest
    confidence-level note), but that's a depth-of-verification caveat on
    a working adapter, not a missing capability the checklist item
    actually claims.
  - [x] Model transfers: Hugging Face download only (no peer replication yet)
    - [x] Phase 1: `model_transfers` + `node_model_inventory` schema and
          repositories - matching SCHEMA.md Model transfers and Node model
          inventory. `source_type`/`source_node_id` gets the same CHECK
          pairing as `nodes.container_runtime`/`node_type`
          (`source_node_id` required when and only when `source_type =
          'peer_node'`) - a real invariant worth enforcing at the database
          level even though nothing produces `peer_node` yet (that's
          v0.3.0's rsync work); `requested_by` is nullable, same reasoning
          as `nodes.registered_by` (the break-glass SuperAdmin can initiate
          a transfer too). `node_model_inventory` has no separate `id` -
          SCHEMA.md doesn't list one, so `(node_id, model_ref)` is the
          natural composite primary key, and the repository upserts on it
          rather than inserting a new row per transfer. Complete when
          covered by integration tests against a real Postgres instance,
          including that the CHECK constraint rejects both pairing
          violations and that both migrations' `down` reverse cleanly.
          Done - migrations 000008/000009, `internal/db/transfers.go`
          (`ModelTransferRepository`: `Create`, `FindByID`,
          `UpdateProgress`, `SetStatus`, `ListByDestNode`) and
          `internal/db/node_model_inventory.go`
          (`NodeModelInventoryRepository`: `Upsert`, `Get`, `ListByNode`).
          `SetStatus` stamps `completed_at` only for the three terminal
          statuses. 13 new integration tests, including both directions of
          the CHECK constraint violation and that `Upsert` replaces rather
          than duplicates a `(node_id, model_ref)` row. SCHEMA.md's Model
          transfers table updated in the same change to mark
          `requested_by` nullable, matching what this phase actually
          built.
    - [x] Phase 2: Protocol extension + dispatch capability -
          `internal/agentproto` gains `TypeStartTransfer` (central -> agent:
          transfer ID, model ref) and `TypeTransferProgress` (agent ->
          central: bytes transferred/total, status, error message).
          `internal/agentconn`'s `Registry` gains its first real send
          capability (`Send(nodeID, envelope) error`) - up to now it has
          only tracked which node owns which connection (Phase 4 of the
          agent runtime work), never actually written to one. `Handler`
          gains a pluggable `OnMessage` callback for message types it
          doesn't handle internally (hello/heartbeat/error stay internal),
          so this package stays generic - it does not need to know
          anything about transfers specifically, matching ARCHITECTURE.md's
          framing of it as the only component that speaks the agent
          protocol, not a place for feature-specific logic. Pure
          types/plumbing, testable the same way Phase 2 and Phase 4 of the
          agent runtime work were - real `coder/websocket` test
          connections, no actual download involved.
          Done - `TransferProgress.Status` is a plain string, not
          `db.TransferStatus`, since `agentproto` has no dependency on
          `internal/db` (it's shared by both binaries, and
          `sparky-agent` has no database access at all).
          `Registry.Send` only holds its mutex for the map lookup, not
          the network write - `coder/websocket` supports concurrent
          writes on one connection (confirmed against its own doc
          comments and `AGENTS.md`, not assumed), and holding the lock
          across a write would block every other node's `Send`/
          `Register`/`Unregister` on it. `NewHandler` takes the new
          `OnMessageFunc` as a required (but nil-able) parameter, same
          as its other dependencies - `cmd/sparky-server` currently
          passes `nil`, since nothing dispatches a real command until
          Phase 3+ exists. `readLoop` now decodes every message instead
          of discarding it unread; `hello`/`hello_ack`/`heartbeat` stay
          silently internal, `error` is logged, everything else goes to
          `onMessage`.
    - [x] Phase 3: Agent-side Transfer Executor (`agent/transfer`) - a
          native Go Hugging Face downloader, no external tool dependency
          (no Python/`huggingface_hub`, no `git`/`git-lfs`) - see this
          date's Decisions Log entry for why, and for the real HF Hub API
          behavior this was verified against (not assumed) before writing
          it: `GET /api/models/{repo}` lists files (`siblings`, no sizes),
          and `GET /{repo}/resolve/{revision}/{file}` - Go's `net/http`
          follows the redirect automatically - lands on a response with a
          real `Content-Length` and `Accept-Ranges: bytes`, confirmed
          against a real Hugging Face repo. v0.1.0 downloads every file in
          the repo's default revision - correct for a vLLM/full-residency
          model (which needs the whole HF Transformers-format directory
          anyway) but wasteful for a multi-quantization GGUF repo (only one
          `.gguf` file is actually needed) - a known, deliberate
          simplification, not silently assumed to be fine. Wired into
          `agent/connection.Conn`'s dispatch on `TypeStartTransfer`: one
          goroutine per active transfer, per docs/AGENT.md Service
          Architecture Notes, pushing `TypeTransferProgress` back
          periodically via Phase 2's plumbing. Complete when covered by
          unit tests against a local HTTP test server standing in for the
          Hugging Face API (for deterministic behavior - progress
          reporting, resume-via-Range, error handling), plus one real,
          ad hoc download against an actual small public Hugging Face repo
          verified manually, matching this project's empirical-verification
          discipline.
          Done - re-verified the real API shapes above against
          `Qwen/Qwen2.5-0.5B-Instruct-GGUF` before writing any code, plus
          one new finding neither PLANNING.md nor its Decisions Log had
          previously recorded: a small, non-LFS file served directly from
          `huggingface.co` (e.g. `LICENSE`) advertises `Accept-Ranges:
          bytes` but silently ignores a `Range` header and returns `200`
          with the full body anyway - only large LFS-tracked files
          (redirected to a CDN) actually honor it with a real `206`. A
          resumed request that comes back `200` is therefore treated as
          "the server is sending the whole file, start over," not
          appended to blindly - see the new Decisions Log entry.
          `agent/transfer.Executor` HEAD-requests every file's size up
          front (so `BytesTotal` is accurate from the first progress
          call, not discovered file by file) and reports progress
          throttled by a byte threshold (4 MiB default), not wall-clock
          time, so tests stay deterministic without a sleep-based poll.
          `Conn` gained a `sync.WaitGroup` (`transferWG`) around the
          goroutine `dispatch` spawns on `TypeStartTransfer`, and `Run`
          now waits on it before returning - the graceful-shutdown
          behavior docs/AGENT.md already documented but nothing built
          until this phase. 9 new unit tests in `agent/transfer` (full
          download, periodic progress, real resume, a server that ignores
          Range, skip-if-complete, list/download error handling, a
          canceled context, a nested file path) plus 2 new tests in
          `agent/connection` (`TypeStartTransfer` dispatch delivering
          `TypeTransferProgress` back over the connection, and `Run`
          provably blocking on an in-flight transfer until it finishes).
          Manually verified end to end against the real Hugging Face Hub
          (`hf-internal-testing/tiny-random-bert`, ~27 MB, 10 files
          including one nested path): full download landed byte-correct
          files; a re-run skipped every file (no GET calls, only HEAD);
          truncating the largest file and re-running resumed and produced
          output `md5sum`-identical to a fresh independent download.
    - [x] Phase 4: `internal/transfers.Service` - RBAC-gated with the
          existing `rbac.CanManageModelStore` (no new RBAC function needed:
          CLAUDE.md's sidebar tier note, "Transfers... Admin+grant
          initiate", already matches `CanManageModelStore`'s exact shape -
          Admin/SuperAdmin implicit, PowerDev only with the
          `manage_model_store` override). `InitiateTransfer` confirms the
          destination node has a live connection (Phase 2's `Registry`),
          creates the `model_transfers` row (`queued`), and dispatches
          `TypeStartTransfer`. A handler registered via Phase 2's
          `OnMessage` receives `TypeTransferProgress` and updates
          `bytes_transferred`/`status`, upserting `node_model_inventory` on
          completion. No HTTP handler yet, same precedent as the node
          registry and Model profiles. Complete when covered by unit tests
          against fakes for the repository, registry/dispatch, and audit
          dependencies, exercising RBAC denial, an offline destination
          node, and the happy path through to a completed transfer.
          Done - `Service.canManageModelStore` resolves
          `CanManageModelStore`'s `hasOverride` argument itself, querying
          `PermissionOverrideRepository.Get` only when actor is PowerDev
          (Admin/SuperAdmin already have the capability implicitly, and no
          other tier can have it regardless of `hasOverride`, so there is
          no reason to hit the overrides table for them).
          `InitiateTransfer` checks `Registry.Connected` and returns the
          new `ErrDestNodeOffline` before ever calling `Create` - an
          unreachable destination never leaves behind a queued transfer
          nothing will ever pick up, matching this phase's stated
          ordering. v0.1.0 only ever creates a `TransferSourceInternet`
          row (`InitiateTransferParams` has no source-node field), per
          this milestone item's "no peer replication yet" scope.
          `HandleTransferProgress` is `Service`'s `agentconn.OnMessageFunc`
          - it takes `nodeID` from the authenticated connection itself,
          not a value out of the transfer row, and only upserts
          `node_model_inventory` (`InventoryStatusPresent`) once status is
          `completed`, looking up the transfer's `model_ref` via
          `FindByID` since `TransferProgress`'s wire payload doesn't carry
          it. As an `OnMessageFunc`, it has no return value to propagate
          an error through, so it logs failures instead, the same
          `*log.Logger` dependency shape as `agentconn.Handler`'s own.
          17 unit tests against fakes, covering RBAC denial (including the
          PowerDev-with/without-override split), invalid params, an
          offline destination node, `Create`/`Send`/audit failures each
          left unrolled-back per this codebase's established precedent
          (`rbac.Service.ElevateTier`), and `HandleTransferProgress`'s
          progress/status/completion/failure/malformed-payload/
          wrong-message-type paths. `go test -race` clean.

    All four phases done - checking off the top-level "Model transfers"
    item above.
  - [x] Running instances: single-node load/unload
        Done - migration `000010_create_running_instances` creates the
        `running_instances` table per SCHEMA.md; `running_instance_nodes`,
        Green/Blue/Red eligibility, and reduced-capacity launches are all
        v0.3.0 clustering scope and do not exist yet (2026-08-12 Decisions
        Log). `internal/db.RunningInstanceRepository` covers Create,
        FindByID, FindActiveByProfileID (the non-terminal-status lookup
        `LoadInstance` uses to refuse a second concurrent load for the
        same profile), and SetStatus (COALESCE-preserves `actual_port`
        across a status transition that has nothing new to report, e.g.
        running -> stopping).

        `internal/engines.Adapter` gained `BuildLaunchSpec(params)
        (LaunchSpec, error)` - both adapters now translate their
        recognized `engine_params` keys into image + command-line flags
        (`llamaCPPImage` is the same `ghcr.io/ggml-org/llama.cpp:server`
        image already verified serving real inference; `vllmImage` is
        `vllm/vllm-openai:latest`, unverified against a live install, same
        honest gap as `vllmParams` already carried). Deliberately excludes
        `--model`/`--port` - see the 2026-08-12 Decisions Log entry on the
        server/agent split.

        `agentproto` gained `TypeLoadInstance`/`TypeUnloadInstance`
        (central -> agent) and `TypeInstanceResult` (agent -> central,
        `running`/`failed`/`stopped`). `agent/connection.Conn.dispatch`
        gained matching cases, following `TypeStartTransfer`'s goroutine-
        per-command pattern with its own `instanceWG` (separate from
        `transferWG` - unrelated operations, no reason to block each
        other's shutdown wait). `resolveModelPath` locates a model already
        on local storage - the whole directory for a full-GPU-residency
        engine, a single `.gguf` file for a partial-offload one (see this
        pass's new Known Issues row for the multi-quantization gap that
        creates). `agent/runtime/containers.Spec` gained `Cmd`/`Port`/
        `Mounts` fields (previously only `Image`/`Name`/`Env`/`CDIDevices`
        existed - nothing had needed them before now, per the 2026-08-11
        manual-fallback-check Decisions Log entry); `StartContainer` wires
        `Port` into `ExposedPorts`/`PortBindings` (host and container
        always the same port number - SCHEMA.md's `actual_port` has no
        separate host/container pair to reconcile) and `Mounts` into
        `HostConfig.Binds`, mounting the agent's whole model storage root
        read-only at the identical path inside the container, so a
        `--model` path resolved against the host needs no translation.
        `InstanceContainerName` (`sparky-instance-{id}`) is the
        deterministic name both load and unload key off of - see the
        2026-08-12 Decisions Log entry on why no `container_id` column
        exists.

        `rbac.CanLaunchInstances` (Developer, PowerDev, Admin, or
        SuperAdmin) matches CLAUDE.md's already-documented sidebar tier
        note ("Developer launch") - deliberately a lower bar than
        `CanManageProfiles`, since a Developer may launch a profile
        someone else created but not edit it. One function guards both
        load and unload, same reasoning as `CanManageModelStore` guarding
        both download and delete.

        `internal/lifecycle.Service` (the Model Lifecycle Orchestrator)
        ties it together: `LoadInstance` checks the rule, confirms no
        active instance already exists for the profile
        (`ErrAlreadyRunning`) and the target node is connected
        (`ErrTargetNodeOffline`) before ever persisting a row, resolves
        the engine adapter's `LaunchSpec`, creates the `running_instances`
        row, dispatches `TypeLoadInstance`, and audits (`loaded_model` -
        SCHEMA.md's own audit log example). `UnloadInstance` requires the
        instance to currently be `running` (`ErrInstanceNotRunning`),
        transitions it to `stopping`, dispatches `TypeUnloadInstance`, and
        audits (`unloaded_model`). `HandleInstanceResult` is `Service`'s
        `agentconn.OnMessageFunc` for `TypeInstanceResult`, updating
        `running_instances` status/`actual_port`/`error_message` from
        whatever the agent actually reports. No HTTP handler yet, same
        precedent as the node registry, Model profiles, and Model
        transfers.

        Verified with real integration tests against Postgres (migration
        up/down/up cycle, `internal/db`) and unit tests against fakes for
        `internal/engines`, `agent/runtime/containers`,
        `agent/connection` (including a real in-process WebSocket
        round-trip through `httptest`, and a shutdown-wait test for the
        new `instanceWG`), `internal/rbac`, and `internal/lifecycle`.
        `go test -race` clean across every touched package.
  - [x] Metrics: live telemetry collection and dashboard (no historical retention yet)
        Done, ingestion only - "and dashboard" in this item's own name is
        the eventual consumer of this data, not something built here; the
        actual UI/SSE wiring belongs to the separate, still-unstarted
        "Dashboard UI" item directly below, matching ARCHITECTURE.md's own
        component split (Metrics Ingestion & Retention is distinct from
        HTTP/API + Dashboard). Retention/downsampling and NFS/S3 export
        are also out of scope here - both are the separate v0.4.0
        Historical metrics milestone, and this item's own name says "no
        historical retention yet".

        `agent/telemetry.Collector` (new package, ARCHITECTURE.md's
        Telemetry Collector component) reads `nvidia-smi` (aggregated
        across however many GPUs it reports - averaged utilization,
        summed memory, matching Nodes' existing single `gpu_memory_gb`
        scalar per node; untested against a real multi-GPU node, an
        honest gap, not empirically verified, same standard as the vLLM
        adapter's launch spec) and `/proc/stat`/`/proc/meminfo` directly,
        confirmed against this dev machine's real files, not assumed. CPU
        utilization is a stateful delta between successive `Read` calls
        (a cumulative counter, not an instantaneous value) - the first
        reading after agent startup is always 0, with no prior sample to
        diff against.

        `agentproto` gained `TypeTelemetry` (agent -> central, unprompted
        - the central app never requests a reading) and its `Telemetry`
        payload; `RecordedAt` is the agent's own timestamp, the same
        trust level already extended to `Heartbeat.SentAt`. Migration
        `000011_create_metrics` creates the `metrics` table per
        SCHEMA.md - no separate `id` column, `(node_id, recorded_at)` is
        the composite primary key, same reasoning as
        `node_model_inventory`. `internal/db.MetricsRepository` is
        write-only for now, same precedent as `AuditRepository` - nothing
        yet reads metrics back. `RunningInstanceRepository` gained
        `FindActiveByNode` (status = `running` specifically, narrower
        than `FindActiveByProfileID`'s starting/running/stopping) so
        ingestion can resolve `running_instance_id` server-side - the
        agent has no reason to track its own Running instance state, so
        it isn't asked to.

        `agent/connection.Conn` gained a telemetry goroutine (own ticker,
        `Config.TelemetryPollInterval` - `SPARKY_TELEMETRY_POLL_INTERVAL`,
        parsed and validated fail-fast by `cmd/sparky-agent`, per the
        existing config precedent) alongside the existing heartbeat
        goroutine - "does not wait on the command loop" per docs/AGENT.md
        Service Architecture Notes. A zero/negative interval is guarded
        against inside `sendTelemetry` itself (logs and disables
        telemetry for that connection) rather than trusted blindly from
        config, since `time.NewTicker` panics on one and an unrecovered
        goroutine panic would take the whole agent process down over what
        should only ever disable one feature.

        `internal/metrics.Service.HandleTelemetry` is the
        `agentconn.OnMessageFunc` that persists an incoming reading -
        unlike `internal/transfers.Service`/`internal/lifecycle.Service`,
        there is no RBAC check or audit record, since a telemetry push is
        agent-initiated observational data, not a human actor's
        state-changing action (SCHEMA.md Audit log's own examples are all
        administrative actions on domain objects). A `FindActiveByNode`
        lookup failure (not "not found," an actual infrastructure error)
        does not drop the reading - it persists with a nil
        `running_instance_id` rather than losing a data point over a
        correlation lookup's own trouble. No HTTP handler yet, same
        precedent as every other v0.1.0 service so far.

        Verified with real integration tests against Postgres (including
        the migration's up/down/up cycle) and unit tests against fakes/
        real `/proc` fixtures across every touched package; `go test
        -race` clean.
  - [x] Dashboard UI (htmx), sidebar nav, Read-only through Admin views
    - [x] Phase 1: base layout/sidebar shell, session-gated routing, and
          three working read-only pages (Dashboard overview, Nodes,
          Model profiles) - establishing the htmx/template/handler
          pattern end to end, per the user's chosen scope (shell + core
          read views first, remaining sections as later phases, same
          phased-delivery precedent as Model transfers/Model profiles).
          No write/action forms yet (no launch/create/edit routes exist)
          and no HTML login page - see this phase's Known Issues rows.
          Complete when the compiled binary, driven with real HTTP
          requests against a real Postgres instance, serves all three
          pages with real data and correctly gates them behind a session.
          Done - new `web` package (`web.FS`, `//go:embed
          templates static`) holds `web/templates/layouts/base.html`
          (sidebar + main pane, per CLAUDE.md Frontend Conventions),
          `web/templates/pages/{dashboard,nodes,profiles}.html`, and
          `web/static/{css/main.css,js/htmx.min.js}` - htmx 2.0.10
          vendored (not CDN-loaded, matching the single-binary
          `embed.FS` deployment story), plain CSS as CLAUDE.md's Tech
          Stack table already defaulted to.
          `internal/httpapi.loadPageTemplates` parses each page together
          with the base layout into its *own* isolated
          `*template.Template` (base+one-page per entry, not one
          combined `ParseGlob`) - every page defines a block also named
          `"content"`, so parsing them all into one shared template set
          would make the last-parsed page's content block silently win
          for every page. `render` picks `"base"` (full document) or
          `"content"` (just the inner block) depending on whether the
          request carries htmx's own `HX-Request` header - one page
          template serves both the full-page load and the
          `hx-get`/`hx-target="#main-content"` partial swap, no
          duplicate markup between a `pages/` and `partials/` file for
          content that's identical either way.
          `internal/nodes.Service`/`internal/profiles.Service`/
          `internal/lifecycle.Service` each gained a `List*` method
          (`ListNodes`/`ListProfiles`/`ListInstances`) - unguarded by
          RBAC, since viewing is available at the lowest tier
          (CLAUDE.md Frontend Conventions) and read/view actions are
          never audited (ARCHITECTURE.md Audit Log). `httpapi.New` now
          also takes `nodeLister`/`profileLister`/`instanceLister`
          (narrow interfaces over those three `List*` methods) and a
          `*log.Logger`, and returns `(*API, error)` - a template parse
          failure is a build-time bug, caught at startup rather than
          surfacing as a broken page on first request. This is also the
          first time `internal/nodes.Service`, `internal/profiles.Service`,
          and `internal/lifecycle.Service` are wired into
          `cmd/sparky-server/main.go` at all - every prior phase that
          built one left it unwired pending exactly this. `RequireSession`
          (defined back in the AD/LDAP auth work, unused until now) gates
          `/dashboard`, `/nodes`, `/profiles`; `/`, redirecting to
          `/dashboard`, and `/static/*` stay public.
          `RunningInstanceRepository` gained `List` (Dashboard's fleet
          summary needed one; nothing had before). Verified two ways: unit
          tests against fakes for every new handler (full page vs. htmx
          partial, RBAC/session gating, list-failure handling, target-node
          name resolution), and a genuine end-to-end pass through the
          actual compiled `sparky-server` binary - real Postgres, a real
          `set-superadmin-password` run over a real pty, a real
          `POST /login/break-glass`, then `curl`+cookiejar against
          `/`, `/dashboard`, `/nodes`, `/profiles`, an `HX-Request: true`
          partial fetch, both static assets, and an unauthenticated
          request confirming the existing JSON 401 - not just asserted,
          actually run. No visual/browser check was possible in this
          environment (no display) - see this phase's Known Issues row.
    - [x] Phase 2: a real HTML login page and a logout control - the
          first slice of "Phase 2 and beyond," scoped down from that
          whole bundle the same way Phase 1 was scoped down from the
          full 8-section dashboard (confirmed with the user rather than
          assumed - see this date's Decisions Log entry). Complete when
          the compiled binary, driven with real HTTP requests against a
          real Postgres instance, serves a real login form, authenticates
          through it, and a logout control actually clears the session.
          Done - `GET /login` serves `web/templates/pages/login.html`, a
          standalone document (no sidebar - there's no session yet to
          show one for), parsed on its own rather than with
          `base.html` per `loadPageTemplates`' existing per-page-isolation
          reasoning. `POST /login` now serves two callers at the same URL:
          the existing JSON contract, unchanged, and the login page's own
          `application/x-www-form-urlencoded` submission
          (`isFormRequest`/`handleLoginFormSubmit` in the new
          `login_page.go`) - branching on Content-Type within one handler,
          not a separate endpoint, since (unlike break-glass vs AD) this
          is the same identity source and flow, just two response
          formats. A failed form submission re-renders the login page
          in place with an error message rather than redirecting back to
          `GET /login`, since no flash-message/session mechanism exists
          to carry an error across a redirect. `GET /login` redirects an
          already-authenticated request straight to `/dashboard`. The
          sidebar (`base.html`) gains a `Logout` control, `hx-post`ing to
          `/logout`; `handleLogout` now also sets `HX-Redirect: /login`
          (inert for a plain API caller, read by htmx to drive a full
          client-side navigation to the login page on success).
          `RequireSession`'s unauthenticated response is deliberately
          left as its existing JSON 401, not changed to redirect to
          `/login` - an htmx partial (`HX-Request`) fetch following a
          redirect would swap the login page's full standalone document
          into `#main-content`, which is broken - see this phase's Known
          Issues row. Verified two ways: 8 new unit tests (form rendering,
          already-authenticated redirect, successful submission setting
          the cookie and redirecting, each failure mode re-rendering with
          its error message, the JSON contract unaffected, logout's
          `HX-Redirect` header) and a genuine end-to-end pass through the
          actual compiled `sparky-server` binary - real Postgres, a real
          `set-superadmin-password` run over a real pty, then `curl`
          (no cookiejar needed - manual `-b`/`-c` cookie-file handling)
          through: the unauthenticated form, a failed submission against
          a real (dial-failing, since no real AD server exists in this
          environment) LDAP identity provider, a real
          `POST /login/break-glass` session, the already-authenticated
          `/login` redirect, the sidebar's real logout control markup,
          `POST /logout` clearing the cookie and setting `HX-Redirect`,
          and a post-logout request actually getting 401 again. Neither
          CSRF protection nor auth-endpoint rate limiting exist anywhere
          in this app yet, despite CLAUDE.md's Security Considerations
          calling for both - both are pre-existing, app-wide gaps
          spanning every POST route already built, not something specific
          to this page, so neither was added here; see this phase's Known
          Issues rows.
    - [x] Phase 3: the Transfers sidebar section as a fourth read-only
          page, following Phase 1's exact pattern - the first slice of
          "Phase 3 and beyond," scoped down the same way Phase 2 was
          (confirmed with the user via an explicit multi-option choice -
          see this date's Decisions Log entry). Complete when the
          compiled binary, driven with real HTTP requests against a real
          Postgres instance, serves a real Transfers page with real
          data, gated the same way Nodes/Model profiles already are.
          Done - `db.ModelTransferRepository` gained `List` (every
          transfer across every node, most recently requested first -
          distinct from the existing `ListByDestNode`'s per-node view);
          `internal/transfers.Service` gained `ListTransfers`, unguarded
          by RBAC like every other `List*` method Phase 1 added. New
          `internal/httpapi/transfers.go` resolves each transfer's
          `dest_node_id` to a node name the same way `handleModelProfiles`
          already resolves `target_node_id`, and formats
          `bytes_transferred`/`bytes_total` as simple `"N.N MB / N.N MB"`
          text - no smart unit-scaling, matching nodes.html's own
          `{{.GPUMemoryGB}} GB` precedent of plain, minimal formatting
          over a general-purpose byte-formatting helper. New CSS status
          classes for `queued`/`transferring`/`completed`/`cancelled`
          (reusing existing color tokens - grey/amber/green/grey;
          `failed` already existed). This is also the first time
          `internal/transfers.Service` is constructed in
          `cmd/sparky-server/main.go` at all - Model transfers Phase 4
          built it but never wired it in, since nothing had an
          HTTP-facing caller for it yet. `HandleTransferProgress` is
          deliberately still not wired as `agentconn`'s `onMessage`
          callback - `ListTransfers` is a pure read path that doesn't
          need it, and combining it with `HandleInstanceResult`/
          `HandleTelemetry` is its own explicitly-declined-for-now
          option (OnMessage dispatcher consolidation), not a side effect
          of this one. Verified two ways: new unit tests (dest-node-name
          resolution, empty state, list-failure handling, unauthenticated
          gating) plus integration tests for `ModelTransferRepository.List`
          against a real Postgres instance, and a genuine end-to-end pass
          through the actual compiled `sparky-server` binary - a real
          node and transfer row inserted directly via SQL (no write path
          exists yet to create one through the app itself), then
          `curl` confirming the full page renders the model ref, resolved
          node name, status badge, and formatted progress; the `HX-Request`
          partial correctly omits the sidebar; and the sidebar's own
          `Transfers` link renders correctly from another authenticated
          page.
    - [x] Phase 4: the Audit log sidebar section as a fifth read-only
          page - the first slice of "Phase 4 and beyond," scoped down
          the same way Phases 2 and 3 were (confirmed with the user via
          an explicit multi-option choice - see this date's Decisions
          Log entry). Unlike the four prior read-only pages, Audit log's
          sidebar tier floor is Admin, not Read-only (CLAUDE.md Frontend
          Conventions), so this phase is also the first Dashboard UI
          page needing a real RBAC check, not just a session check.
          Complete when the compiled binary, driven with real HTTP
          requests against a real Postgres instance, serves a real Audit
          log page gated by actual tier, not just session presence.
          Done - `rbac.CanViewAuditLog` (Admin/SuperAdmin only, no
          override path, same shape as `CanManageNodes`).
          `db.AuditRepository` gained `List` (most recently created
          first - the `created_at` index already existed for exactly
          this query, added back when `audit_log` was first migrated).
          `db.UserRepository` gained `List` too, ordered by display
          name - the future Users & permissions page's roster, and in
          the meantime this page's source for resolving each record's
          `actor_id` to a display name (the by-now-familiar
          map-of-names pattern from Model profiles/Transfers, applied to
          users instead of nodes). `audit.Recorder` (renamed internal
          `writer` dependency to `store`, since it now covers both the
          append and list paths to `audit_log`) gained `List(ctx, actor
          rbac.Actor)`, checking `CanViewAuditLog` and returning the
          existing `rbac.ErrNotPermitted` internally - the RBAC decision
          lives in the Service-layer method itself, not only at the HTTP
          layer, matching `transfers.Service.InitiateTransfer`'s own
          internal-check precedent, so the guarantee travels with the
          method regardless of caller. New `internal/httpapi/audit.go`
          resolves the viewer's own tier via a new `actorFromIdentity`
          helper (the session cookie deliberately carries no tier - see
          `internal/session`'s own doc comment - so every RBAC-gated
          handler resolves it fresh via `FindByID`), and maps
          `rbac.ErrNotPermitted` to a 403. The sidebar's `Audit log` link
          is shown unconditionally to every authenticated viewer, same
          as every other nav link - a non-Admin who clicks it gets a
          JSON 403, not a friendlier redirect or a hidden link; see this
          phase's Known Issues row for why that gap was deliberately not
          closed here. Verified two ways: new unit tests (RBAC
          permit/deny in `internal/rbac` and `internal/audit`, actor-name
          resolution/empty-state/list-failure/forbidden/unauthenticated
          handling in `internal/httpapi`) plus integration tests for
          `AuditRepository.List`/`UserRepository.List` against a real
          Postgres instance, and a genuine end-to-end pass through the
          actual compiled `sparky-server` binary - a real Admin user, a
          real Developer (non-Admin) user, and a real audit record
          inserted directly via SQL (no write path exists yet to
          produce a session for either user through the app itself, so
          a session cookie was signed directly via `internal/session`
          with the running process's own `SESSION_SECRET`), confirming
          the Admin session gets a real 200 with the record's action and
          resolved actor name, the Developer session gets a real 403,
          the break-glass SuperAdmin session gets 200 without needing a
          tier lookup at all, an unauthenticated request gets 401, and
          the `HX-Request` partial correctly omits the sidebar.
    - [x] Phase 5: the Users & permissions sidebar section as a sixth
          read-only page - the first slice of "Phase 5 and beyond", scoped
          down the same way Phases 2-4 were (confirmed with the user via
          an explicit multi-option choice - see this date's Decisions Log
          entry). Same Admin sidebar floor as Audit log, so the second
          Dashboard UI page needing a real RBAC check, not just a session
          check. Complete when the compiled binary, driven with real HTTP
          requests against a real Postgres instance, serves a real roster
          gated by actual tier, not just session presence. Done -
          `rbac.CanViewUsers` (Admin/SuperAdmin only, no override path,
          same shape as `CanViewAuditLog`). Unlike Audit log, whose RBAC
          check lives in `internal/audit.Recorder.List`, this page's full
          roster is exposed (not just an already-permitted record's
          `actor_id` resolved to a name), so the check lives in
          `rbac.Service.ListUsers` instead - `rbac.Service` already wraps
          `UserRepository` via its `userStore` interface (gained `List`)
          for `ElevateTier`, so this is the natural home rather than a new
          package. `rbac.Service` is wired into `cmd/sparky-server/main.go`
          for the first time this phase - `ElevateTier` itself still has
          no HTTP caller, since no write/action forms exist yet. New
          `internal/httpapi/users.go` resolves the viewer's own tier via
          the existing `actorFromIdentity` helper (shared with Audit log)
          and maps `rbac.ErrNotPermitted` to a 403; each row's
          `elevated_by` resolves to a display name via the same
          map-of-names pattern already used for actor names on Audit log
          and node names on Model profiles/Transfers - built from the
          roster response itself, not a second query. Verified two ways:
          new unit tests (RBAC permit/deny in `internal/rbac`,
          elevated-by-name resolution/empty-state/list-failure/
          forbidden/unauthenticated/HX-Request handling in
          `internal/httpapi`) plus a genuine end-to-end pass through the
          actual compiled `sparky-server` binary against a real Postgres
          instance - a real Admin user, a real Developer (non-Admin) user
          with `elevated_by`/`elevated_at` set to the Admin, and the
          break-glass SuperAdmin, following the same session-cookie-signed-
          out-of-band approach Phase 4 established (no write path exists
          yet to produce a session through the app itself) - confirming
          the Admin session gets a real 200 with both users and the
          elevator's name resolved, the Developer session gets a real 403,
          the SuperAdmin session gets 200 without a tier lookup, an
          unauthenticated request gets 401, and the `HX-Request` partial
          correctly omits the sidebar.
    - [x] Phase 6: the Settings sidebar section as a seventh read-only
          page - the first slice of "Phase 6 and beyond", scoped down the
          same way Phases 2-5 were (confirmed with the user via an
          explicit multi-option choice - see this date's Decisions Log
          entry). Same Admin sidebar floor as Audit log and Users &
          permissions. Complete when the compiled binary, driven with
          real HTTP requests against a real Postgres instance, serves the
          two singleton config rows (Metrics export config, Audit
          settings) gated by actual tier. Done - this phase discovered
          neither singleton table SCHEMA.md already documented
          (`metrics_export_config`, `audit_settings`) had ever been
          migrated; building the page required creating both first.
          Migrations 000012/000013 seed exactly one row each at creation
          time (`id boolean PRIMARY KEY DEFAULT true` plus a `CHECK (id)`
          singleton constraint, same pattern as `break_glass_credential`)
          rather than leaving them absent until first configured - unlike
          the break-glass credential, there is no real "not configured
          yet" state to represent for a setting the app reads on every
          page load. `audit_settings.retention_months` had no default
          decided anywhere before this phase (unlike
          `forwarding_protocol`, whose `syslog` default SCHEMA.md already
          stated) - confirmed with the user: 12 months, a middle value
          between the Metrics table's own 6-month raw-resolution window
          and this column's 24-month ceiling, which is now enforced with
          a CHECK constraint rather than left as an application-level-only
          convention. `rbac.CanViewSettings` (Admin/SuperAdmin only, no
          override path, same shape as `CanViewAuditLog`/`CanViewUsers`,
          but a distinct function rather than reusing either - three
          capabilities that happen to share a tier floor today, not one).
          Neither singleton row belongs to an existing Service's domain
          (`internal/metrics.Service` is scoped to telemetry ingestion
          only; `internal/audit.Recorder` is scoped to `audit_log`, not
          the separate `audit_settings` table), so this phase adds a new
          `internal/settings` package - `Service.Get` checks
          `CanViewSettings` and returns both rows as plain `*db` types
          (not a wrapping struct), so `internal/httpapi` doesn't need to
          import the new package at all, matching how
          `auditLister`/`transferLister`/`userRoster` are already defined
          there against `db` types alone. New `internal/httpapi/settings.go`
          reuses `actorFromIdentity` and resolves each row's `updated_by`
          via a single `FindByID` lookup rather than the audit/users pages'
          list-and-map pattern, since at most two IDs ever need resolving
          here. SCHEMA.md updated in this same change: both tables'
          `updated_by` notes corrected to nullable (the SuperAdmin case,
          same reasoning as Nodes' `registered_by`), and both tables'
          seeded-default behavior documented. Verified two ways: new unit
          tests (`rbac.CanViewSettings`'s tier matrix,
          `settings.Service.Get` permit/deny/store-failure,
          `internal/httpapi` handler tests covering updated-by-name
          resolution, empty-state placeholders, forbidden, unauthenticated,
          HX-Request partial), new `MetricsExportConfigRepository`/
          `AuditSettingsRepository` integration tests confirming the
          migrations' seeded rows against a real Postgres instance
          (including a full down/up migration round-trip), and a genuine
          end-to-end pass through the actual compiled `sparky-server`
          binary - a real Admin user, a real Developer (non-Admin) user,
          the break-glass SuperAdmin, and both config rows updated
          directly via SQL to non-default values (backend `nfs`,
          forwarding to a GELF host with TLS) to confirm the page reflects
          real data, not just the seeded defaults.
    - [x] Phase 7: the Metrics sidebar section as the eighth and final
          sidebar page - the first slice of "Phase 7 and beyond", scoped
          down the same way Phases 2-6 were (confirmed with the user via
          an explicit multi-option choice - see this date's Decisions Log
          entry). Unlike every prior read-only page, this one's floor is
          Read-only (CLAUDE.md Frontend Conventions, Metrics' sidebar
          tier), same as Dashboard/Nodes/Model profiles/Transfers - no
          RBAC check, only `RequireSession`. Complete when the compiled
          binary, driven with real HTTP requests against a real Postgres
          instance, serves both a per-node latest-reading table and a
          GPU-utilization chart backed by real telemetry rows. Done -
          `internal/db/metrics.go` (previously write-only, per its own
          prior doc comment - "nothing yet reads metrics back") gained
          `LatestByNode` (`SELECT DISTINCT ON (node_id) ... ORDER BY
          node_id, recorded_at DESC`, one row per node) and `Recent`
          (most recent `recentMetricsLimit` = 200 rows across every node
          combined, most-recently-recorded first - a recent-window chart
          data source, not full historical retention, which stays the
          separate v0.4.0 Historical metrics milestone).
          `internal/metrics.Service` gained `ListLatestByNode`/`ListRecent`,
          unguarded by RBAC like `nodes.Service.ListNodes`, and is
          constructed in `cmd/sparky-server/main.go` for the first time -
          it existed as a type before this phase but nothing had ever
          instantiated it in production. Chart.js 4.4.4 (MIT license)
          vendored at `web/static/js/chart.umd.min.js` per
          ARCHITECTURE.md's "no CDN" static-asset policy - loaded
          unconditionally in `base.html`'s `<head>` alongside a new small
          `web/static/js/metrics.js` (defines `initMetricsChart`,
          CLAUDE.md's "minimal vanilla JS" allowance), so both are already
          present by the time any page's content - including an htmx
          partial swap into Metrics from elsewhere - needs them, without
          depending on the relative execution order of dynamically
          swapped `<script src>` tags (verified by reading the vendored
          htmx source's `allowScriptTags:true` default, not assumed).
          `internal/httpapi/metrics.go` builds the chart's series data as
          JSON server-side (one series per node, points sorted
          chronologically) and embeds it via `template.JS` inside an
          inline `<script>` in `metrics.html`'s own content block -
          `encoding/json`'s default HTML-safe escaping is what makes that
          embedding safe without a separate sanitization step. Verified
          two ways: new unit tests (`internal/metrics.Service`'s two new
          read methods, `internal/httpapi` handler tests covering
          node-name resolution in both the table and the embedded chart
          JSON, empty state, both list-failure paths, unauthenticated,
          HX-Request partial), new `MetricsRepository.LatestByNode`/
          `Recent` integration tests against a real Postgres instance,
          and a genuine end-to-end pass through the actual compiled
          `sparky-server` binary - a real Developer (non-Admin) user (no
          Admin needed, confirming the Read-only floor), two real nodes,
          and nine real metrics rows across a 20-minute window inserted
          directly via SQL, confirming the table shows the correct
          latest-per-node values, the embedded chart JSON contains both
          nodes' points in chronological order with correctly resolved
          names, both vendored scripts serve with 200, an unauthenticated
          request gets 401, and the `HX-Request` partial correctly omits
          the sidebar. The chart's actual visual rendering in a real
          browser remains unverified, same accepted gap as every other
          Dashboard UI page's own CSS/layout/real-DOM behavior - see
          PLANNING.md Known Issues.
    - [x] Phase 8: the Users & permissions tier-change form - the
          Dashboard UI's first write/action form, the first slice of
          "Phase 8 and beyond", scoped down the same way Phases 2-7 were
          (confirmed with the user via an explicit multi-option choice -
          see this date's Decisions Log entry). Complete when the
          compiled binary, driven with real HTTP requests against a real
          Postgres instance, actually changes a real user's tier, writes
          a real audit record, and correctly refuses a transition the
          acting viewer isn't permitted to make. Done - no changes needed
          to `internal/rbac` at all: `rbac.CanElevate` and
          `rbac.Service.ElevateTier` were both already fully built and
          tested from before this Dashboard UI milestone even started;
          this phase only had to give `ElevateTier` its first HTTP
          caller. New `internal/httpapi`: `userElevator` interface
          (`ElevateTier`), a narrow interface distinct from `userRoster`
          even though the same `*rbac.Service` value satisfies both in
          production (`cmd/sparky-server/main.go` now passes `rbacService`
          twice to `httpapi.New`, once per interface); `reachableTiers`,
          a pure helper that asks `rbac.CanElevate` about every candidate
          tier rather than reimplementing its rules, so the dropdown
          offered can never drift from what the server-side check
          actually permits; `handleElevateUser` (`POST
          /users/{id}/tier`), which validates the posted tier against
          the four known values before calling `ElevateTier` (input
          validation happening ahead of the Service call, not relied on
          to reject bad input) and, on success, responds with
          `HX-Redirect: /users` - the same blunt full-navigation pattern
          `handleLogout` already established, chosen over a new
          per-row-partial-render path for the very first write action in
          the app. `users.html` gained a per-row `<select>` + submit
          button, shown only when `ReachableTiers` is non-empty for that
          row (an Admin viewing another Admin's row, or their own row,
          correctly gets no form at all, since `CanElevate` refuses both
          cases). This is also the first new state-changing endpoint
          added since the app-wide CSRF gap was documented (PLANNING.md
          Known Issues) - deliberately left unprotected consistent with
          that existing, already-accepted gap rather than retrofitted
          for one endpoint in isolation; the Known Issues row is extended
          to name it explicitly rather than left silently exposed to a
          reader of that row. Verified two ways: new unit tests
          (`reachableTiers`'s tier-matrix coverage mirroring
          `rbac.CanElevate`'s own exhaustive test, `isKnownTier`,
          `handleElevateUser`'s success/forbidden/not-found/invalid-tier/
          missing-tier/generic-failure/unauthenticated paths) and a
          genuine end-to-end pass through the actual compiled
          `sparky-server` binary against a real Postgres instance - a
          real Admin elevating a real Developer to PowerDev (confirmed
          in the database: new tier, `elevated_by`/`elevated_at` set,
          and a real `elevated_user` audit row with the correct
          `from_tier`/`to_tier` detail), that same Admin then correctly
          refused a skip-a-step promotion to Admin, a Developer session
          refused any elevation at all, an unknown target user ID
          getting 404, an invalid tier value getting 400, an
          unauthenticated request getting 401, and finally the
          break-glass SuperAdmin session successfully performing the
          exact skip-a-step promotion the regular Admin had just been
          refused, with `elevated_by` correctly left `NULL` and the
          audit row correctly marked `is_superadmin_action`.
    - [x] Phase 9: the node registration form - the second write/action
          form, the first slice of "Phase 9 and beyond", scoped down the
          same way Phases 2-8 were (confirmed with the user via an
          explicit multi-option choice - see this date's Decisions Log
          entry). Complete when the compiled binary, driven with real
          HTTP requests against a real Postgres instance, actually
          registers a real node, persists only its bearer token's hash
          (never the plaintext), writes a real audit record, and
          correctly refuses a non-Admin's attempt. Done - no changes
          needed to `internal/nodes` at all: `nodes.Service.RegisterNode`
          and `RegisterNodeParams.validate()` were both already fully
          built and tested well before this Dashboard UI milestone even
          started (this phase only had to give `RegisterNode` its first
          HTTP caller), same shape as Phase 8's `ElevateTier` discovery.
          New `internal/httpapi`: `nodeRegistrar` interface
          (`RegisterNode`), referencing `nodes.RegisterNodeParams`
          directly rather than a primitives-only method signature -
          unlike every other narrow interface in this package, this one
          genuinely can't avoid importing a domain package's exported
          type, since `*nodes.Service`'s real method takes that struct
          and a differently-shaped interface method wouldn't be
          structurally satisfied by it at all; `handleRegisterNodeForm`
          (`GET /nodes/register`) and `handleRegisterNode` (`POST
          /nodes/register`). Unlike Phase 8's tier-change form, this
          POST's successful response cannot use the blunt
          `HX-Redirect`-back-to-the-list pattern, since
          `RegisterNode`'s plaintext bearer token is returned exactly
          once and would be lost on an immediate redirect - success
          instead renders a dedicated confirmation page
          (`node_registered.html`) showing the token with an explicit
          warning it will never be shown again, and the whole flow (form
          submission included) deliberately uses a plain `<form
          method="post">` full-page navigation rather than an
          `hx-post` partial swap, avoiding any need to reason about
          htmx's swap semantics for a page whose entire point is
          something the user must actually read before navigating away.
          A validation failure (`nodes.ErrInvalidNode`) re-renders the
          form with the specific reason and every previously-typed field
          value preserved, rather than a blank form or a raw JSON error.
          The Nodes list page gained a `CanRegister`-gated "Register
          node" link, resolved the same non-security-boundary way as the
          Users page's per-row `ReachableTiers` - the real enforcement is
          `rbac.CanManageNodes` inside both new handlers, checked
          directly (not via a Service call, since a `GET` has no
          mutation to gate through one) for the form's visibility and
          indirectly via `RegisterNode` itself for the actual write.
          This is the second new state-changing endpoint added since the
          CSRF gap was documented, and joins it the same way Phase 8's
          did - see that phase's own Decisions Log entry, extended here
          rather than repeated. Verified two ways: new unit tests
          (`CanRegister` shown/hidden by tier, the registration form's
          own 403/401 gating, successful registration with correct
          `RegisterNodeParams` construction including the
          spark-vs-docker-gpu `ContainerRuntime` nil/non-nil cases,
          forbidden, invalid-node re-display with preserved field
          values, non-numeric memory input rejected before the Service
          call, unauthenticated) and a genuine end-to-end pass through
          the actual compiled `sparky-server` binary against a real
          Postgres instance - a real Admin registering a real spark
          node (confirmed in the database: only `bearer_token_hash`
          persisted, never the plaintext token; `registered_by` set to
          the Admin; a real `registered_node` audit row) with the
          real plaintext token rendered on the confirmation page, a
          Developer session correctly refused (403) both the form and
          the submission, a missing-hostname submission correctly
          re-shown with `"invalid node: hostname is required"`, a
          docker-gpu submission missing `container_runtime` correctly
          refused with the matching validation message, and an
          unauthenticated request to the form getting 401.
    - [x] Phase 10: the model profile create/edit form - the third
          write/action form, the first slice of "Phase 10 and beyond",
          scoped down the same way Phases 2-9 were (confirmed with the
          user via an explicit multi-option choice - see this date's
          Decisions Log entry). Complete when the compiled binary,
          driven with real HTTP requests against a real Postgres
          instance, actually creates and edits a real profile - with its
          `requires_full_gpu_residency` correctly derived from the
          selected engine adapter, its `engine_params` validated through
          that same adapter, and a real audit record - and correctly
          refuses a non-PowerDev's attempt. Done - `profiles.Service`
          needed one small, genuinely new addition unlike Phases 8/9's
          "no changes needed" discovery: `GetProfile` (wrapping the
          already-existing but previously Service-unexposed
          `ProfileRepository.FindByID`), added so the edit form has a
          single-record read to prefill itself from, unguarded by RBAC
          like `ListProfiles` for the same reason (a read, with the real
          authorization check on the write that follows). `CreateProfile`/
          `UpdateProfile` themselves needed no changes at all - both were
          already fully built and tested, same shape as `ElevateTier`/
          `RegisterNode` before them. New `internal/httpapi`:
          `profileEditor` interface, referencing
          `profiles.CreateParams`/`UpdateParams` directly - the second
          interface in this package (after `nodeRegistrar`) that can't
          avoid importing a domain package's exported type, for the same
          structural reason. `handleNewProfileForm`/`handleCreateProfile`
          (`GET`/`POST /profiles/new`) and
          `handleEditProfileForm`/`handleUpdateProfile`
          (`GET`/`POST /profiles/{id}/edit`) share one template
          (`profile_form.html`, an `IsEdit` flag picking the title,
          submit-button label, and POST target) and one field-parsing
          helper (`fieldsFromForm`) - the same shape of form either way,
          differing only in whether a `GetProfile` prefill happens first.
          Unlike node registration's confirmation page, a successful
          submission here has no one-time secret to protect, so it uses
          the standard Post/Redirect/Get pattern (a real
          `http.Redirect` to `/profiles`, not `HX-Redirect`, since
          this form - like node registration's - is a plain `<form
          method="post">` full-page navigation, not `hx-post`) rather
          than needing a dedicated confirmation view. A validation
          failure (`profiles.ErrInvalidProfile`, which already carries
          the specific engine-adapter-level reason inside its wrapped
          message - e.g. "invalid engine params: tensor_parallel_size
          must be positive") re-renders the same form with that message
          and every previously-typed value preserved, including the raw
          `engine_params` JSON text so a malformed submission never loses
          what was typed. A blank `engine_params` submission defaults to
          `"{}"` before reaching `Fields.validate`/the adapter, since
          every `internal/engines` adapter treats an empty params object
          as valid and typing `{}` by hand for every profile would be
          needless friction. The engine-type dropdown includes
          `aphrodite` even though it has no registered adapter until
          v0.3.0 - selecting it correctly fails through the same
          validation path as any other invalid submission, rather than
          the UI maintaining its own separate notion of which enum
          values are "really" available yet. The Model profiles list
          page gained a `CanManage`-gated "New profile" link and a
          per-row "Edit" link, resolved the same non-security-boundary
          way as the Nodes and Users pages' own gated links/forms - the
          real enforcement is `rbac.CanManageProfiles`, checked directly
          in the GET handlers and internally inside
          `CreateProfile`/`UpdateProfile` on every submission. This is
          the third new state-changing endpoint pair added since the
          CSRF gap was documented, and joins it the same way Phases 8
          and 9 did - see those phases' own Decisions Log entries,
          extended here rather than repeated. Verified two ways: new
          unit tests (`profiles.Service.GetProfile` found/not-found/
          store-failure, `CanManage` shown/hidden by tier, both forms'
          own 403/401/404 gating, the edit form's prefill including a
          real JSON `engine_params` round-trip, successful create with
          correct `CreateParams` construction and the blank-params-
          defaults-to-`{}` behavior, successful update targeting the
          right profile ID, forbidden, invalid-profile re-display with
          preserved field values including the raw JSON text,
          non-numeric port rejected before the Service call) and a
          genuine end-to-end pass through the actual compiled
          `sparky-server` binary against a real Postgres instance - a
          real PowerDev creating a real vLLM profile with real
          `engine_params` (confirmed in the database:
          `requires_full_gpu_residency = true`, the exact submitted
          params, a real `created_profile` audit row), the edit form
          correctly prefilling every field including the JSON textarea
          (HTML-escaped by `html/template`, confirmed safe and correct),
          a real edit changing the port and persisting `updated_by`, a
          Developer session correctly refused (403) both the new-profile
          form and its submission, a submission with an
          adapter-rejected `engine_params` value
          (`tensor_parallel_size: -1`) correctly re-shown with vLLM's
          own specific validation message, an unknown profile ID on the
          edit form getting 404, and an unauthenticated request getting
          401.
    - [x] Phase 11: the instance load/unload form, SSE wiring for live
          telemetry/transfer progress, and combining
          `internal/transfers.Service.HandleTransferProgress`/
          `internal/lifecycle.Service.HandleInstanceResult`/
          `internal/metrics.Service.HandleTelemetry` into the single
          `agentconn.OnMessageFunc` `cmd/sparky-server/main.go` previously
          passed `nil` for. Completes all four Dashboard UI write/action
          forms this milestone scoped.
          Done - `cmd/sparky-server/main.go` builds `transferService`/
          `lifecycleService`/`metricsService` before `agentConnHandler`
          now (previously the reverse) and passes a dispatch closure that
          switches on the envelope's own `Type` to route to the right
          handler, rather than calling all three unconditionally. New
          `internal/events` package: a non-persisted, in-process
          publish/subscribe `Broker` (`Publish` drops rather than blocks
          for a full subscriber buffer, so one slow browser tab can never
          back-pressure the agent's own WebSocket read loop, same
          reasoning as `agentconn.Registry.Send`'s own mutex scope) fed by
          the dispatch closure and consumed by new
          `internal/httpapi/events.go`'s `GET /events` (session-gated, no
          RBAC beyond that - the events carry nothing more than a type).
          New `web/static/js/sse.js` is a small vanilla-JS `EventSource`
          client that triggers a debounced htmx refetch of whatever page
          is visible on a relevant event, rather than patching individual
          DOM nodes or a server-only stub with no client - confirmed with
          the user as this phase's scope (see this date's Decisions Log
          entry). New `internal/httpapi/instances.go`:
          `handleLoadInstance`/`handleUnloadInstance`
          (`POST /profiles/{id}/load`, `POST /instances/{id}/unload`) -
          `lifecycle.Service.LoadInstance`/`UnloadInstance` needed no
          changes, same "already built, just needed an HTTP caller" shape
          as Phases 8/9/10's own discoveries. The controls live on the
          Model profiles page (not a new sidebar section), matching
          CLAUDE.md's existing "PowerDev create / Developer launch" tier
          note for that section directly;
          `internal/httpapi/model_profiles.go` now also lists running
          instances and folds them into a `profileID -> active instance`
          map (non-terminal statuses only, so a stopped/failed instance
          leaves its profile eligible to load again). Fourth write route
          pair added since the app-wide CSRF gap was documented - joins it
          the same way Phases 8, 9, and 10 did (Known Issues updated
          below). Verified with new unit tests (`internal/events`
          publish/subscribe/cancel/buffered-drop behavior;
          `internal/httpapi`'s RBAC/not-found/conflict/unauthenticated
          paths for both new routes; `handleEvents` against a real
          `httptest.Server`/`http.Client`, not `httptest.NewRecorder` - a
          streaming handler and a concurrently-read response body over a
          plain recorder's unsynchronized `bytes.Buffer` would race under
          `-race`, which a real connection doesn't have) and `go test
          -race` clean, plus a genuine end-to-end pass against the real
          compiled `sparky-server`/`sparky-agent` binaries, a real
          Postgres instance, and a real local Podman daemon: registered a
          real node, connected a real agent, created a real profile,
          Loaded it through the real form, and confirmed the
          `running_instances` row actually left `starting` (a real agent
          error - no `.gguf` file present, expected in this sandbox with
          no real model on disk - transitioned it to `failed`, proving
          OnMessage consolidation processes the agent's response instead
          of leaving the row stuck) while a real `curl -N /events`
          connection concurrently received the matching
          `event: instance_result` SSE frame; a subsequent Unload attempt
          against that non-running instance correctly got a real 409
          `NOT_RUNNING`, and the Model profiles page correctly re-offered
          Load once the instance reached its terminal `failed` state.

    All eleven phases done - checking off the top-level "Dashboard UI"
    item above.
  - [x] Bare-metal install script (apt + dnf) - agent-only; the central app has
        no packaged installer (see this date's Decisions Log entry and
        ARCHITECTURE.md Deployment Model, corrected in this same change).
        Complete when a `.deb`, an `.rpm`, and a tarball each independently
        produce the same end state (binary, systemd unit, `serviceloop`
        account, GPU-group membership, an unconfigured `secrets.env` ready to
        fill in), and remove/upgrade/purge all behave correctly. Done -
        packaged via `nfpm` (single static Go binary, one YAML config
        produces both `.deb` and `.rpm`, never touches `go.mod`) into
        `scripts/build_packages.sh`'s `dist/` output; binary at
        `/opt/sparky/bin/sparky-agent` for all three methods (the user's
        explicit choice, see this date's Decisions Log entry), with a
        `/usr/local/bin/sparky-agent` convenience symlink since `/opt` isn't
        on any distro's default `$PATH`. New `deploy/systemd/sparky-agent.service`
        and `deploy/secrets.env.template` - both long referenced by
        `docs/AGENT.md` but never actually committed until now.
        `scripts/packaging/lib/agent-common.sh` is the single implementation
        of the idempotent `serviceloop` account creation and `video`/`render`
        GPU-group detection, shared by all three methods (shipped as real
        package content at `/opt/sparky/share/sparky-agent/agent-common.sh`
        for the `.deb`/`.rpm` paths, since nfpm reads script files at build
        time and embeds them into the package - a raw path reference to a
        repo checkout that won't exist on the target host wouldn't work).
        Fresh installs are enabled but never auto-started (an unconfigured
        `secrets.env` would crash-loop); an already-running agent is safely
        restarted on upgrade, relying on nothing more than a
        `systemctl is-active` check rather than parsing either package
        manager's differing upgrade-vs-install argument conventions.
        `apt remove`/`dnf remove` stop and disable the service and remove the
        binary, but deliberately leave `serviceloop` and
        `/etc/sparky-agent/secrets.env` in place; `apt purge` removes both.
        RPM has no native purge concept at all - confirmed by reading its
        actual scriptlet argument semantics, not assumed - so
        `scripts/packaging/purge_rpm.sh` is copied out to
        `/usr/local/sbin/sparky-agent-purge.sh` by the `preremove` scriptlet
        just before removal deletes everything else the package owns, and
        `docs/AGENT.md` documents running it by hand as the RPM equivalent.
        No CI wiring and no signed package repository - both explicitly out
        of scope for this pass (see this date's Decisions Log entry).
        Verified two ways: `dpkg-deb`/`rpm -qlp` inspection of both packages'
        contents and embedded control scripts against what was actually
        written, byte-for-byte where checkable, plus a genuine end-to-end
        pass inside disposable Debian and Rocky Linux podman containers
        running real systemd (`--systemd=always /sbin/init`, not a faked
        init) - install, start with a syntactically valid but unreachable
        `secrets.env` (confirmed the agent retries the WebSocket dial with
        backoff and stays `active (running)` rather than crash-looping),
        upgrade a running install (confirmed the PID changed and
        `secrets.env` was byte-identical before and after), `remove`
        (confirmed `serviceloop`/`secrets.env` survived, everything else
        didn't), and `purge`/`purge_rpm.sh` (confirmed full cleanup, no
        stray files anywhere - this specific check caught a real bug during
        development: `purge_rpm.sh` was initially just ordinary package
        content, so a plain `dnf remove` deleted it before it could ever be
        run; fixed by having `preremove.sh` copy it out to a
        package-unowned path first). The Debian container naturally only has
        a `video` group and the Rocky container naturally has both `video`
        and `render`, so both branches of the group-detection loop were
        exercised without needing to fabricate either. Real GPU hardware,
        real Spark ARM64 execution, and true bare-metal PID 1 behavior
        (journald integration, udev device-permission timing, real boot
        ordering) remain unverified - a container's systemd is a reasonable
        proxy, not identical to real hardware, same honest gap already on
        record for CDI passthrough.

- [ ] **v0.2.0** - Bare-metal runtime backend, and Spark hardware validation
  - [x] Agent: bare-metal runtime backend (`serviceloop`, direct process exec) -
        for hosts where GPU passthrough isn't viable (e.g. a single-GPU
        workstation already using that GPU for its own host session), not
        specific to DGX Spark hardware. Validated against this project's own RTX
        4090 laptop, the concrete hardware in hand for this case - see
        PLANNING.md's 2026-08-13 Decisions Log entry
    - [x] The backend itself (`agent/runtime/baremetal`) - a shared
          `agent/runtime.Backend` interface (`Start`/`Stop`/`Shutdown`)
          introduced so `agent/connection` no longer branches on which
          concrete backend (containers or bare-metal) it holds, satisfied by
          both `agent/runtime/containers` (refactored onto it, no behavior
          change) and the new `agent/runtime/baremetal` (`exec.Command`,
          a mutex-guarded `instanceID -> process` map, SIGTERM-then-
          grace-period-then-SIGKILL stop, `Shutdown` stopping every tracked
          process concurrently on agent exit - see docs/AGENT.md Signal
          Handling, now updated from "statement of intent" to describe
          this). `cmd/sparky-agent` picks the concrete backend once at
          startup from `SPARKY_RUNTIME_BACKEND`, also closing a real,
          separately-discovered gap: that value was loaded but never
          validated against its enum or dispatched on anywhere before this.
          Since an engine adapter's `LaunchSpec.Image` is a Docker image
          reference with no bare-metal meaning, `agentproto.LoadInstance`
          gained an `EngineType` field (server already has
          `profile.EngineType` in scope where it builds this payload) and
          the agent resolves a local executable via one new optional env
          var per engine type (`SPARKY_LLAMACPP_BINARY_PATH` /
          `SPARKY_VLLM_BINARY_PATH` - only `vllm`/`llamacpp` have adapters
          today) - both design forks confirmed with the user before
          building, see the 2026-08-13 Decisions Log entries below.
          `SPARKY_MODEL_STORAGE_PATH`'s bare-metal default
          (`/opt/sparky/serviceloop/models` - corrected from an initial
          `/home/serviceloop/models`, see the dated Decisions Log entry
          below for why) is now actually implemented in `agent/config.Load`,
          closing the Known Issues row that named this exact gap. Verified via `go test -race` across every
          touched package, including new `agent/runtime/baremetal` tests
          exercising real process lifecycle (SIGTERM, SIGKILL escalation
          on a SIGTERM-ignoring process, `Shutdown` stopping multiple
          tracked processes concurrently) against harmless real binaries
          (`/bin/sh`) already present on any Linux box - no GPU or real
          engine binary needed for that.
    - [x] Real hardware validation against the RTX 4090 laptop - done, run
          directly on that machine (confirmed real GPU: an NVIDIA GeForce
          RTX 4090 with 16GB VRAM, the mobile/laptop variant, already
          driving the host's own Xorg session - exactly the "GPU already
          claimed by the host OS" case bare-metal exists for). Two real
          bugs found and fixed in the same pass, neither guessed at -
          both confirmed via direct reproduction before and after the fix:
          (1) the default model storage path (`/home/serviceloop/models`)
          was unreachable under `ProtectHome=true` and never created by
          `useradd --no-create-home` in the first place - see this date's
          separate Decisions Log entry for the `/opt/sparky/serviceloop`
          fix; (2) `systemctl stop` while an instance was loaded (skipping
          an explicit unload) crashed the real `llama-server` child instead
          of stopping it cleanly - the unit's default `KillMode` double-
          signals a tracked child (once from systemd's cgroup-wide kill,
          once from the agent's own per-child `Shutdown` logic), and
          llama.cpp's own signal handler aborts on a second interrupt
          arriving mid-shutdown; fixed with `KillMode=mixed`, see that
          date's own Decisions Log entry. With both fixed: a real
          `llamacpp` profile (a 3.6GB GGUF, `n_gpu_layers=99`) loaded
          through the real dashboard flow, confirmed as an actual child
          process of `sparky-agent` running as `serviceloop`, with
          `nvidia-smi` and `--query-compute-apps` both attributing real
          GPU memory (~4.5GB) directly to that PID - genuine offload, not
          a CPU fallback - and a real `/v1/chat/completions` request
          against it returned a real completion. Unload sent SIGTERM,
          confirmed via the process's own log line
          ("cleaning up before exit...") and freed GPU memory. The
          KillMode-fixed shutdown path was re-verified by reproducing the
          exact same load-then-abrupt-`systemctl stop` sequence and
          confirming the same clean exit. Live telemetry (`nvidia-smi`-
          derived) tracked the real VRAM climb (152 -> 3846 -> 4742 MiB)
          in the `metrics` table in real time during the load, closing the
          separate, previously-unverified `nvidia-smi` telemetry Known
          Issues row as a byproduct - visual chart rendering itself
          remains unverified, consistent with this project's existing,
          already-accepted no-browser-available gap for that page. One
          real gap surfaced and left deliberately unfixed, matching what
          the runbook expected going in: `running_instances` stays stale
          (`running`) in the database after an ungraceful agent stop, with
          no real process behind it and nothing to reconcile it on
          restart - a new Known Issues row records this precisely. The
          engine-adapter-argument-shape question is now fully closed for
          llama.cpp (`--model`/`--port`/`--host`/`--gpu-layers`/
          `--ctx-size`/`--threads` all confirmed directly against
          `llama-server --help` and a real successful load) - vLLM's half
          of that question was deliberately not attempted this pass
          (confirmed with the user) and remains open, tracked in Known
          Issues.
  - [x] Agent: compiled-engine provisioning from GitHub Releases - self-service
        download/deploy of maintainer-built x86_64/arm64 engine release tarballs
        (llama.cpp to start, matching this project's own amd64/arm64 agent-
        packaging convention), reusing the Transfer Executor's download/progress-
        reporting pattern rather than a new mechanism. Distributed like models
        are today - the agent pulls directly from GitHub over HTTPS, no new
        central-server-side hosting required. Hosted on github.com to start,
        migrating to a dedicated repo later if warranted - see the 2026-08-15
        Decisions Log entry for the full design discussion (why a prebuilt
        binary rather than an agent-side source build; why GitHub Releases first)

        Done - new `agent/enginetransfer` package (mirroring `agent/transfer`'s
        Executor pattern): downloads a release tarball plus its sibling
        `$ENGINE-$VERSION-$ARCH.tar.xz.sha256` from the main Sparky repo's own
        GitHub Releases, verifies the SHA256 (mandatory here, unlike the
        Hugging Face Transfer Executor's own downloads, which have none - the
        user confirmed these bundles are maintainer-built and always come with
        a checksum), extracts via a shelled-out system `tar -xJf` (no new Go
        dependency - stdlib `compress/*` has no xz support, and every
        bare-metal target already has `tar`), and installs into a versioned
        directory (`$SPARKY_ENGINE_INSTALL_PATH/<engine_type>/<version>/`),
        atomically repointing a `latest` symlink at each successful install.
        Multiple versions deliberately coexist on disk and in the new
        `node_engine_inventory` table (composite-keyed on `(node_id,
        engine_type, version)`, unlike `node_model_inventory`'s
        `(node_id, model_ref)`) - a real design constraint surfaced by the
        user during this pass: they want to eventually pin two otherwise-
        identical profiles to two different engine versions for direct
        output/timing comparison, so provisioning a new version must never
        delete an old one. That specific pinning mechanism (a new
        `model_profiles.engine_version` column and launch-time resolution) is
        intentionally deferred - see the new follow-up item below - but this
        pass's on-disk and schema layout is built to support it without
        rework.

        New `internal/agentproto` message types (`TypeStartEngineTransfer`/
        `TypeEngineTransferProgress`, distinct from the Model transfers ones -
        different semantics, no HF-style `resolve/{revision}` URL scheme,
        checksum verification instead of none). New migrations
        `000015_create_engine_transfers`/`000016_create_node_engine_inventory`,
        reusing `model_profiles`' own `profile_engine_type` enum for
        `engine_type` rather than minting a new one. New `internal/db`
        repositories (`EngineTransferRepository`, `NodeEngineInventoryRepository`)
        and `internal/engineprovision.Service`, directly mirroring
        `internal/transfers`' shape (`ProvisionEngine`/
        `HandleEngineTransferProgress`) but gated by `rbac.CanManageNodes` -
        Admin/SuperAdmin only, no PowerDev-override path, confirmed with the
        user: this is node-level infrastructure provisioning, not a
        per-user-grantable capability like `manage_model_store`.
        `agent/connection.Conn` gained a new dispatch case and its own
        `engineTransferWG`, separate from `transferWG` (same reasoning as the
        existing `instanceWG`/`transferWG` split - unrelated operations,
        no reason to block each other's shutdown wait). New optional
        `SPARKY_ENGINE_INSTALL_PATH` agent env var, defaulting to
        `/opt/sparky/serviceloop/engines` on bare-metal, mirroring
        `SPARKY_MODEL_STORAGE_PATH`'s own default-path convention -
        `SPARKY_LLAMACPP_BINARY_PATH`/`SPARKY_VLLM_BINARY_PATH` themselves are
        deliberately unchanged (still a static, operator-set path to one
        executable); the operator points that path at
        `.../latest/llama-server` once, and re-provisioning a new version
        just swaps what the symlink resolves to, requiring only the existing
        "config change needs a restart" step, not a fresh config edit.

        Matching this project's own repeated precedent (RBAC Phase B, the
        audit log, the node registry, and `internal/transfers.InitiateTransfer`
        itself all shipped their logic layer before any HTTP handler or UI
        existed), this pass deliberately stops at the logic and agent layer -
        `internal/engineprovision.Service` is constructed in
        `cmd/sparky-server/main.go` and wired into the `onMessage` dispatch
        (the progress-reporting direction is real infrastructure regardless of
        whether anything can trigger a run via HTTP yet) but is not yet passed
        to `httpapi.New` - no HTTP handler or dashboard form exists to call
        `ProvisionEngine` from. Verified with real `go test` coverage
        throughout (unit tests for `agent/enginetransfer` against an
        `httptest.Server` plus a faked `tar` shell-out, covering checksum-
        mismatch, extraction failure, and same-version re-provisioning/
        different-version-coexistence branches; `internal/engineprovision`
        against fakes, mirroring `internal/transfers/service_test.go`'s RBAC/
        offline-node/dispatch-failure coverage; `agent/connection` dispatch
        and shutdown-wait tests for the new message type) and integration
        tests for both new repositories against a real local Postgres
        instance, including that both migrations' `down` reverse cleanly.
        Not yet done, and explicitly out of scope for this pass: an actual
        published release tarball to provision against on real hardware (the
        `httptest.Server`-based tests stand in for that), and any HTTP/UI
        surface.
  - [x] `sparky-agent setup` subcommand - creates/verifies the `serviceloop` system
        account and its GPU-passthrough group membership (`video`/`render`,
        distro-dependent), idempotent, supports both environment-variable-driven and
        interactive invocation; `scripts/install.sh` is trimmed to placing the binary
        and systemd unit and delegates account provisioning to this subcommand rather
        than running `useradd`/`usermod` itself.
        Done - new `agent/provision` package (`Provisioner.EnsureServiceloopUser`/
        `EnsureModelStorageDir`/`EnsureGPUGroupMembership`), a direct 1:1
        translation of the three bash functions it replaces onto a fakeable
        `runner` seam (mirroring `agent/telemetry`'s `commandRunner` idiom) -
        real `go test` coverage this logic never had as bash, including branches
        (user exists/missing, each GPU group present/absent independently,
        command failures) the podman-based verification pass could only exercise
        in combination, not in isolation. `cmd/sparky-agent setup` is the sole
        caller: an explicit `os.Getuid() != 0` check (no existing Go code in
        either binary had this precedent, though `install_agent.sh` already
        checks this for itself) before calling into `agent/provision`, dispatched
        in `main()` *before* `config.Load()` - `setup` needs none of the normal
        agent env vars, and requiring them valid first would be circular, since
        an operator typically hasn't gotten a bearer token to put in
        `secrets.env` yet at the point they'd run this. `scripts/packaging/lib/agent-common.sh`
        trimmed to keep only `ensure_secrets_file` (a different concern -
        materializing a config file, never part of this migration);
        `postinstall.sh`/`install_agent.sh` now call `/opt/sparky/bin/sparky-agent
        setup` instead of the three removed bash functions. Verified two ways,
        not just reasoned about: first, ran the compiled subcommand directly
        against this project's own RTX 4090 laptop (no `serviceloop` account
        existed at the time, having been fully purged after the earlier
        hardware-validation pass) - confirmed it created the account, directory,
        and group membership correctly, and that a second run was a true no-op.
        Second, rebuilt the `.deb` with the wired-up packaging scripts and
        re-ran the same disposable-Debian-podman-plus-real-systemd pass the
        original packaging PR used, this time confirming `sparky-agent setup`'s
        own banner actually appears in the install log, both `video`-only and
        `video`+`render` group-detection branches still work (added a `render`
        group mid-test, reinstalled, confirmed it joined), a reinstall stays
        idempotent, and `apt purge` (untouched by this change) still behaves
        exactly as before - `serviceloop` removed, `/opt/sparky/serviceloop`
        deliberately left in place.
  - [x] Per-profile engine version pinning - a new `model_profiles.engine_version`
        column (nullable, null meaning "latest") plus launch-time resolution to a
        specific installed version under `SPARKY_ENGINE_INSTALL_PATH` instead of
        always the `latest` symlink, so two otherwise-identical profiles can each
        pin a different installed engine version for direct output/timing/tuning
        comparison. Deliberately scoped out of the engine-binary-provisioning item
        above rather than bundled into it (confirmed with the user) - the
        provisioning mechanism is a full-sized piece of work on its own, and this
        is a separate, cleanly-reviewable follow-up now that versioned installs
        actually exist to pin against (`node_engine_inventory`'s composite
        `(node_id, engine_type, version)` key was deliberately built to support
        this, not a v0.3.0-only precedent). No other blockers.

        Done - migration `000017_add_model_profiles_engine_version` adds a
        single nullable `engine_version text` column, no `CHECK` constraint
        (any non-empty string accepted). Threaded mechanically through the
        full existing chain: `internal/db.ProfileRepository`'s `Profile`
        struct/`Create`/`Update`; `internal/profiles.Fields`/`Service`
        (no new validation - a pin is accepted as-is and forwarded, the
        same "attempt and report failure" philosophy `required_memory_gb`'s
        own SCHEMA.md doc comment already states, confirmed with the user
        over validating against `node_engine_inventory` at save time);
        `internal/lifecycle.Service.LoadInstance`, which now sends
        `agentproto.LoadInstance.EngineVersion` (empty string for an
        unpinned profile - the wire field carries plain values, not
        pointers, same convention as `EngineType`); the profile create/edit
        form (`web/templates/pages/profile_form.html`) gained a plain
        optional free-text input next to `engine_type`, not a dropdown
        populated from `node_engine_inventory` (confirmed with the user -
        matches this item's own scope, defers a picker UX).

        The actual resolution happens agent-side in the new
        `agent/connection.resolveEngineBinaryPath`: an unpinned load
        returns exactly what today's flat `EngineBinaryPaths[engineType]`
        lookup already resolves to (zero behavior change for any profile
        that doesn't set `engine_version`); a pinned load reuses the
        *filename* of the operator's static `SPARKY_<ENGINE>_BINARY_PATH`
        config (already pointing through the `latest` symlink) under the
        pinned version's own directory instead -
        `SPARKY_ENGINE_INSTALL_PATH/<engine_type>/<version>/<same
        filename>` - so provisioning this feature needed zero new
        per-version agent configuration. `agent/runtime`/
        `agent/runtime/baremetal` needed no changes at all -
        `runtime.Spec.BinaryPath` was already opaque to `Start`, so a bad
        pin surfaces as the same `exec.Command` "no such file or
        directory" failure any other misconfigured binary path already
        produces, reported back as a failed `instance_result` - the
        confirmed "fail clearly at launch time, don't pre-validate"
        design.

        Verified with real `go test` coverage throughout: integration
        tests for the new column against a real local Postgres instance
        (set/unset round-trip on both `Create` and `Update`, including
        that clearing a pin back to `NULL` actually clears it, and that
        the migration's `down` reverses cleanly); unit tests for
        `internal/profiles.Service` confirming a pin passes straight
        through with no adapter/node-lookup involvement;
        `internal/lifecycle.Service` confirming a pinned profile's version
        reaches the dispatched envelope and an unpinned one sends empty;
        `internal/agentproto` round-trip coverage for the new field;
        `agent/connection.resolveEngineBinaryPath` unit tests (unpinned
        passthrough, pinned reconstruction, and two degraded-config cases -
        no binary configured, no install path configured) plus a
        dispatch-level test confirming a pinned `LoadInstance` produces a
        `runtime.Spec.BinaryPath` under the versioned directory; and unit
        tests for the two pure form-handling functions the field touches
        in `internal/httpapi` (`fieldsFromForm`/
        `profileFormValuesFromProfile`) - noting as a pre-existing, not
        newly-introduced gap that this package has no HTTP-level test
        coverage for the profile create/edit handlers themselves for any
        field, not just this one.
  - [ ] Validate the Docker/Podman runtime backend (real CDI GPU passthrough)
        against real DGX Spark hardware - the GB10 GPU supports passthrough to
        a container without affecting a display connected to it (NVIDIA's
        supported use case for that hardware), unlike the dev laptop, so
        expected to be the more straightforward CDI validation target once real
        hardware is available

- [ ] **v0.3.0** - Multi-Spark clustering
  - [ ] Fabric groups, physical linkage tracking
  - [ ] Clustered model profiles (Profile cluster nodes: head/worker/rank)
  - [ ] Node model inventory, Green/Blue/Red launch eligibility
  - [ ] Peer-to-peer rsync replication over the cluster link
  - [ ] Reduced-capacity launch prompt and relaunch-at-full-capacity flow
  - [ ] Running instance nodes (actual runtime topology, may differ from profile intent)
  - [ ] Aphrodite engine adapter
  - [ ] Agent: Python-engine provisioning (vLLM now, Aphrodite once its adapter
        above exists) - self-service clone-and-provision for engines that
        install via pip into a venv rather than a compiled binary: clone the
        engine's repo pinned to an exact commit (resolved from an operator-
        chosen tag/release/branch - never left floating on "latest"), then
        `pip install` its `requirements.txt` into a venv under the engine's own
        directory. A distinct trust/complexity profile from the compiled-engine
        track (v0.2.0): arbitrary third-party package install code runs on the
        node, not just a maintainer-built blob, and matching the installed CUDA
        wheel to the node's actual driver is the real hard problem, not the venv
        mechanics themselves - see the 2026-08-15 Decisions Log entry

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

The original bootstrap tasks this section once tracked (compiling `ARCHITECTURE.md`
and `CLAUDE.md` from the completed design, writing `docs/AGENT.md`/`README.md`/
`.env.example`) are all long done - `.clauderules` was also written as part of that
pass, then later retired and fully merged into `CLAUDE.md`. Active work now tracks
at the phase level in Milestones above, which is more precise than a separate list
here can stay in sync with; this section exists for a one-line pointer, not a
duplicate checklist.

- Dashboard UI is fully done as of Phase 11 (the instance load/unload
  form, the `OnMessage` dispatcher consolidation it depended on, and SSE
  wiring) - see that phase's own entry above for detail. Phases 1 (base
  layout, session-gated routing, Dashboard/Nodes/Model profiles read
  views), 2 (login page, logout control), 3 (Transfers read view), 4
  (Audit log read view, the first Admin-tier-gated page), 5 (Users &
  permissions read view), 6 (Settings read view, which also migrated the
  two singleton config tables SCHEMA.md had documented but never
  created), 7 (Metrics read view + chart, back at the Read-only floor,
  which also gave `internal/metrics.Service` its first production
  caller), 8 (the Users & permissions tier-change form, the Dashboard
  UI's first write/action form, giving `rbac.Service.ElevateTier` its
  first HTTP caller), 9 (the node registration form, giving
  `nodes.Service.RegisterNode` its first HTTP caller), and 10 (the model
  profile create/edit form, giving `profiles.Service.CreateProfile`/
  `UpdateProfile` their first HTTP callers, and adding the new
  `GetProfile` read method the edit form's own prefill needed) were
  already done.
- The bare-metal install script (apt + dnf) is done - see that item's own
  entry above. Agent-only; the central app has no packaged bare-metal
  installer (`ARCHITECTURE.md` Deployment Model and `README.md` Quick Start
  corrected to match, in the same change).
- v0.1.0's only remaining open item is "Agent: Docker/Podman runtime
  backend... CDI GPU passthrough," blocked on hardware/verification this
  sandbox doesn't have - see Dependencies and Blockers below, not something
  to pick up as "next up" in the usual sense.
- v0.2.0's bare-metal runtime backend (`agent/runtime/baremetal`) is fully
  done, including real-hardware validation against the RTX 4090 laptop -
  see that item's own entry above for the two real bugs found and fixed in
  the process (`/opt/sparky/serviceloop`, `KillMode=mixed`). The
  `sparky-agent setup` subcommand is also done (`agent/provision`, real
  `go test` coverage, verified both directly on that same laptop and via
  the disposable-podman-plus-real-systemd technique - see that item's own
  entry above). Engine-binary provisioning from GitHub Releases is also
  done (`agent/enginetransfer`, `internal/engineprovision` - see the
  2026-08-15 Decisions Log entries for the design and the concrete choices
  settled while building it), with real `go test` coverage throughout
  (including integration tests for the two new repositories against a real
  local Postgres instance) but no HTTP handler or dashboard form yet, and
  not yet exercised against a real published release tarball on real
  hardware - both explicitly out of scope for that pass. Per-profile
  engine version pinning - the follow-up deliberately scoped out of that
  provisioning work - is also done (`model_profiles.engine_version`,
  `agent/connection.resolveEngineBinaryPath` - see that item's own entry
  above and its dated Decisions Log entry). v0.2.0's only remaining open
  item is the Docker/Podman backend's own real-hardware CDI validation,
  which - like v0.1.0's identical remaining item - is blocked on real DGX
  Spark hardware, not something to pick up as "next up" in the usual
  sense; see Dependencies and Blockers below.

---

## Open Questions

| # | Question | Raised | Notes |
|---|----------|--------|-------|
| 1 | Exact default for the configurable downsampled-aggregate retention window (raw window is fixed at 6 months). | 2026-08-06 | No default chosen yet - needs a decision before v0.4.0 (Historical metrics), not v0.3.0 as originally noted here - Metrics retention is a v0.4.0 milestone item. |

Two questions originally tracked here have moved on, not been deleted outright:
- "What happens to an active session when a user is removed from the AD access
  group mid-session?" is now tracked in Known Issues and Technical Debt below,
  since the auth work it concerns is actually built now - that table is the more
  current record.
- "Does CDI GPU-passthrough behave identically on Podman across target
  distros/hardware?" has been substantially answered, not left open in its
  original vague form: see the 2026-08-10 Decisions Log entry and the CDI row in
  Known Issues and Technical Debt for the actual empirical finding (which
  Docker API mechanism fails, and how), which is far more specific than this
  question originally anticipated. What remains genuinely open is narrower -
  real target hardware/Podman version - and that's what the Known Issues row
  now tracks.

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
| 2026-08-11 | Manual fallback check: a real container serving a tiny CPU-only model (`ghcr.io/ggml-org/llama.cpp:server`, Qwen2.5-0.5B-Instruct GGUF, no GPU/CDI involved) produces genuine inference output through Podman on this dev machine | The 2nd laptop's GPU can't be assigned via CDI because its nvidia drivers aren't loaded, blocking a GPU-based sanity check there; this confirms the container engine itself (start, port-publish, serve, stop) is healthy independent of the still-open CDI gap above - a real `/v1/chat/completions` request returned a coherent completion at ~93 tok/s | Ad hoc via raw `podman run`, not through `agent/runtime/containers.Backend` - `Spec` has no `Cmd`/port-binding fields yet, since nothing has needed them before now; not added here since no code task was requested, just an infrastructure check |
| 2026-08-11 | Model profiles' `topology` enum declares both `single_node` and `clustered` from the start; a `CHECK` constraint, not a narrower enum, is what enforces v0.1.0's single-node-only scope | An enum value costs nothing and doesn't need `fabric_group_id` to exist first (unlike `nodes.agent_status` already shipping `unreachable` before anything produces it) - only `target_node_id`/`fabric_group_id` actually need the deferral nodes' `fabric_group_id` set the precedent for, since a clustered profile has nowhere to record its cluster target without that column | A `single_node`-only enum, extended via `ALTER TYPE ... ADD VALUE 'clustered'` once Fabric groups lands (rejected: enums are cheap to declare fully upfront; deferring the value too would just mean an extra migration later for something a `CHECK` constraint already prevents from being used) |
| 2026-08-11 | `internal/engines` (Phase 2 of Model profiles) validates `engine_params` and reports `requires_full_gpu_residency` only - it does not translate a profile into an actual launch command | ARCHITECTURE.md's Component Breakdown already splits this: Model Profile Management (CRUD + validation) is a distinct component from the Model Lifecycle Orchestrator (owns load/unload, translates into agent commands), which belongs to the separate "Running instances" v0.1.0 item | Building launch-command translation into the adapter now (rejected: nothing calls it yet - Running instances doesn't exist - so it would be exactly the kind of speculative code CLAUDE.md's "don't design for hypothetical future requirements" warns against) |
| 2026-08-11 | Model transfers' agent-side downloader (`agent/transfer`, Phase 3) talks to the Hugging Face Hub API directly over `net/http` - no `huggingface_hub`/Python, no `git`/`git-lfs` on the agent host | Matches the "own it, don't add dependencies you don't need" reasoning already on record for `chi`, `internal/session`, and `coder/websocket` - a system-level Python or git-lfs dependency on every compute node would be a real deviation from this project's single-Go-binary deployment story (CLAUDE.md, docs/AGENT.md Install). Verified against the real API before committing to this, not assumed: `GET https://huggingface.co/api/models/{repo}` lists files via `siblings` (no sizes); `GET https://huggingface.co/{repo}/resolve/{revision}/{file}` redirects to a signed CDN URL - Go's `net/http` follows this by default - and the final response carries a real `Content-Length` and `Accept-Ranges: bytes`, confirmed against `Qwen/Qwen2.5-0.5B-Instruct-GGUF` | Shelling out to `huggingface-cli download` or `git clone` a repo's git-lfs remote (rejected: both require installing and maintaining a second language runtime or tool on every agent host, for no capability a plain HTTP client with redirect support doesn't already provide) |
| 2026-08-11 | Model transfers downloads every file in a Hugging Face repo's default revision for v0.1.0, not just the one file an engine actually needs | Correct and necessary for a vLLM/full-residency profile, which needs the whole HF Transformers-format directory (config, tokenizer, every safetensors shard) anyway. Wasteful specifically for a multi-quantization GGUF repo, where only one `.gguf` file is actually needed (see the earlier CPU-only model test: `Qwen2.5-0.5B-Instruct-GGUF` alone has 8 different quantizations) - a known, deliberate v0.1.0 simplification, not silently assumed fine | Parsing a quantization/file selector out of `model_ref` now, mirroring `llama.cpp`'s own `-hf repo:QUANT` convention (rejected for v0.1.0: no engine adapter needs it yet to actually launch anything - Running instances, the feature that would consume a downloaded file, doesn't exist - revisit once it does) |
| 2026-08-11 | `model_transfers.source_type`/`source_node_id` gets a `CHECK` constraint pairing them (`source_node_id` required if and only if `source_type = 'peer_node'`), even though nothing produces `peer_node` until v0.3.0's rsync work | Same real data-integrity invariant as `nodes.container_runtime`/`node_type` - "populated only when source_type = peer_node" (SCHEMA.md Model transfers) is exactly the shape that precedent already covers, so it gets the same treatment: enforced at the database level regardless of caller discipline, not just documented as an application-level assumption | No constraint, relying on `internal/transfers.Service` alone to never construct an invalid pairing (rejected: the whole reason the nodes precedent exists is that relying on caller discipline alone was judged insufficient there, and nothing about this case is different) |
| 2026-08-12 | `agent/transfer.Executor`'s resume logic treats a `200` response to a Range-header'd request as "start this file over," never appending its body to the existing partial file | Re-verifying the real Hugging Face Hub API before writing Phase 3's resume path (not just trusting the 2026-08-11 entry's Content-Length/Accept-Ranges finding) surfaced a case that entry didn't cover: small, non-LFS files served directly from `huggingface.co` (e.g. `LICENSE`) advertise `Accept-Ranges: bytes` but silently ignore an actual `Range` header, returning `200` with the full body. Only large LFS-tracked files - redirected to a CDN (`us.aws.cdn.hf.co`) - honor it with a real `206`/`Content-Range`, confirmed against both a small file and a large one in `Qwen/Qwen2.5-0.5B-Instruct-GGUF`. Blindly appending a `200`'s body under an assumption of `206` would silently corrupt the file (partial content followed by the full file again) | Treating any non-error status as success and appending regardless (rejected: works for the CDN-redirected case observed in the 2026-08-11 entry, but silently corrupts a resumed small file - exactly the kind of untested assumption this project's empirical-verification discipline exists to catch) |
| 2026-08-12 | Running instances' `agentproto.LoadInstance` payload carries a server-built `Image`/`Args` (from `internal/engines.Adapter.BuildLaunchSpec`) plus a bare `ModelRef`/`Port`/`RequiresFullGPUResidency` - not a fully-formed `containers.Spec`. The agent resolves `ModelRef` to a local path itself and fills in `--model`/`--port`/`--host` before invoking `containers.Backend.StartContainer` | Engine-specific flag translation (`--tensor-parallel-size`, `--gpu-layers`, etc.) needs no agent-local state, so it belongs server-side in `internal/engines`, the same package that already validates those params - keeping the agent oblivious to per-engine CLI shape, exactly as `internal/agentconn`'s generic `OnMessage` design already keeps that layer oblivious to transfers. But the model's actual on-disk path is agent-local knowledge the central app does not have (`SPARKY_MODEL_STORAGE_PATH` is agent-only config) - same reasoning `agent/connection`'s `TypeStartTransfer` handling already established for download destinations. `RequiresFullGPUResidency` rides along on the wire so the agent can decide directory-vs.-single-file resolution using the same capability flag SCHEMA.md's Model profiles already defines, rather than needing to know engine type names itself | A fully agent-side translation, sending only `ModelRef`/`EngineType`/`EngineParams` and letting the agent import an engine-adapter equivalent (rejected: `internal/engines` depends on `internal/db`, and `cmd/sparky-agent` deliberately has zero database dependency - agentproto's own `TransferProgress.Status` design already draws this exact line); a fully server-side translation that also resolves the model path (rejected: the central app has no visibility into any given node's local storage layout, and hardcoding an assumed path would silently break the moment `SPARKY_MODEL_STORAGE_PATH` differs node to node) |
| 2026-08-12 | A Running instance's container is named deterministically (`containers.InstanceContainerName`, `sparky-instance-{instance_id}`) rather than the central app tracking a live container ID | `UnloadInstance`'s wire payload only needs `InstanceID` - the agent derives the exact same name it started the container under and the Docker Engine API accepts a name anywhere it accepts an ID (`ContainerStop`/`ContainerRemove` don't distinguish). Tracking a returned container ID server-side would need a new `running_instances` column with no other consumer, purely to round-trip a value the agent can already reconstruct on its own | Adding `running_instances.container_id`, populated from the agent's `instance_result` on a successful load, then echoed back on unload (rejected: extra schema and an extra round-trip for a value that's a pure function of `instance_id` - `SCHEMA.md` was left unchanged for exactly this reason) |
| 2026-08-12 | `vllm/vllm-openai:latest` chosen as the container image vLLM profiles launch, unverified against a live install | Vllm's own official OpenAI-compatible server image, well-documented and stable - but installing vLLM's CUDA/torch dependency chain wasn't practical in this environment, the same constraint `vllmParams`' own 2026-08-11-adjacent doc comment already disclosed for its recognized flags. `llamaCPPImage` (`ghcr.io/ggml-org/llama.cpp:server`), by contrast, is the same image already empirically verified serving real inference (2026-08-11 entry above) | Deferring the vLLM adapter's `BuildLaunchSpec` until a real install could be tested (rejected: `internal/lifecycle` needs both adapters to exist to build at all, and the codebase's own established pattern - see `vllmParams`' doc comment - is to state an honest confidence gap rather than block on hardware access that may not materialize soon) |
| 2026-08-12 | Running instances ships without a `running_instance_nodes` table, Green/Blue/Red launch eligibility, or reduced-capacity launch handling | All three are explicitly v0.3.0 clustering scope per this milestone file's own split (see the v0.3.0 section below) - `model_profiles_single_node_only` already guarantees every profile that exists today has exactly one `target_node_id`, so there is no multi-node topology yet for `running_instance_nodes` to record. Same precedent as `model_profiles.fabric_group_id` and `profile_cluster_nodes` not existing until Fabric groups lands | Building the table now with no populating code path (rejected: same "no consumer yet" reasoning already on record against `audit_settings` and an unconstrained `nodes.fabric_group_id`) |
| 2026-08-11 | `internal/agentconn`'s `Registry` gains a generic `Send(nodeID, envelope) error`, and `Handler` gains a generic `OnMessage` callback for message types it doesn't handle internally, rather than anything transfer-specific | ARCHITECTURE.md frames the Agent-Communication Layer as "the only component that speaks the agent protocol" that every other component goes through - it should not need to know what a transfer is. Running instances (a separate, later v0.1.0 item) will need the same send/receive capability for container-start commands, so building it generically now, at the size Model transfers actually needs, avoids either duplicating it later or guessing at a needs a future feature hasn't stated yet | A transfers-specific method on `Registry`/`Handler` (rejected: couples a cross-cutting protocol layer to one feature, and CLAUDE.md's Component Breakdown already documents this layer as feature-agnostic) |
| 2026-08-11 | `.clauderules` fully merged into `CLAUDE.md` (which now `@import`s `ARCHITECTURE.md`/`SCHEMA.md`/`docs/AGENT.md` too), rather than converting the pointer to `.clauderules` into an `@import`; `PLANNING.md` stays a prose-only reference, not imported and not moved into a path-scoped `.claude/rules/` file | `.clauderules` was never actually being read - CLAUDE.md only mentioned it in prose, and Claude Code has no mechanism that auto-loads an arbitrary filename, confirmed against actual documentation, not assumed. This caused a real compliance failure (AI-attribution text landed in commits/PRs across the whole session). The fix needed to be structural, not a stronger sentence: `@import` guarantees loading regardless of model behavior. Verified empirically (not assumed) that a `.claude/rules/` file whose content is only an `@import` pointing elsewhere resolves eagerly at session launch regardless of its `paths:` frontmatter - so it provides no conditional-loading benefit over a blanket import, only the downside of an unverified mechanism. Real conditional loading requires the referenced content to live directly inside the rule file, which means relocating `ARCHITECTURE.md`/`SCHEMA.md`/`docs/AGENT.md` out of their expected root-level locations - real surgery, disproportionate to a project this scoped (a handful of nodes, single team), given the failure mode that actually mattered (content that could never load without the model choosing to fetch it) is already fully closed by the blanket imports. `CLAUDE.md` now sits at 565 lines plus ~986 imported - well past Claude Code's own documented ~200-line guidance ("Bloated CLAUDE.md files cause Claude to ignore your actual instructions") - a known, accepted tradeoff, not an oversight. `PLANNING.md` (725 lines, growing) stays unimported for the same bloat reason and because it doesn't map to a narrow file-path pattern the way `docs/AGENT.md` does; the mitigation is behavioral (reading it at the start of every substantive task, maintained without a miss for this entire session) rather than structural | Doing the full `.claude/rules/` relocation now (rejected: disproportionate surgery - moving canonical docs out of their expected locations, re-deriving path-scoping, re-testing - for a project this size, when the actually damaging failure mode is already closed); importing `PLANNING.md` too (rejected: makes the already-past-guidance bloat strictly worse for a file that keeps growing by design). Revisit `.claude/rules/`-based organization as part of a future, separate examination of the project boilerplate this repo was based on, where it can be designed in from a project's first commit rather than retrofitted mid-project |
| 2026-08-12 | `agent/telemetry.Collector` aggregates across every GPU `nvidia-smi` reports into one reading (utilization averaged, memory summed) rather than one row per GPU | Matches Nodes' existing single `gpu_memory_gb` scalar per node (SCHEMA.md Nodes) and Metrics' own single `gpu_utilization_pct`/`gpu_memory_used_mb` columns (SCHEMA.md Metrics) - neither table has a per-GPU-index concept to preserve. Every node this project actually develops against has exactly one GPU (the laptop RTX 4090 and Dell Precision RTX 3080Ti), so this aggregation choice is unverified against a real multi-GPU node - an honest gap, not silently assumed correct, same standard already applied to the vLLM adapter's launch spec | A schema change to record per-GPU rows (rejected: no consumer asks for per-GPU granularity anywhere in SCHEMA.md/ARCHITECTURE.md, and Nodes' own capacity fields already model a node as having one GPU-memory pool) |
| 2026-08-12 | CPU utilization is computed as a stateful delta between successive `agent/telemetry.Collector.Read` calls, not a single instantaneous read; the very first reading after agent startup is always 0 | `/proc/stat` exposes cumulative tick counters since boot, not a point-in-time percentage - a single sample cannot yield a utilization figure, only a rate computed from two samples separated by known time. Keeping the previous sample as `Collector` state (rather than sleeping between two reads inside one `Read` call) avoids blocking the telemetry goroutine's tick for the poll interval's duration just to answer one question | Sleeping briefly inside `Read` to take two samples per call (rejected: blocks the telemetry goroutine, and duplicates state `Collector` can just keep between ticks instead - the poll interval itself is already the natural sampling window) |
| 2026-08-12 | `agentproto.Telemetry` carries no node or Running-instance identity - `internal/metrics.Service.HandleTelemetry` resolves `running_instance_id` server-side via the new `RunningInstanceRepository.FindActiveByNode`, using the connection's own authenticated node identity, not a value in the payload | Same trust boundary already established for `HandleTransferProgress`/`HandleInstanceResult`: the agent-communication layer's authenticated `nodeID` is the source of truth for which node sent a message, never a client-supplied field. The agent has no reason to track its own Running-instance state - only the central app's `running_instances` table knows what's currently loaded where, so asking the agent to duplicate that bookkeeping just to echo an ID back would be redundant, error-prone state to keep in sync | Sending `running_instance_id` from the agent, tracked locally in `agent/connection.Conn` from the `load_instance`/`unload_instance` commands it has already handled (rejected: duplicates state the central app already has authoritatively, and diverges the moment a reconnect or restart loses the agent's own copy) |
| 2026-08-12 | `internal/metrics.Service.HandleTelemetry` has no RBAC check and writes no audit record, unlike `internal/transfers.Service`/`internal/lifecycle.Service` | A telemetry push is agent-initiated observational data collection, not a human actor's state-changing action - SCHEMA.md Audit log's own action examples (`loaded_model`, `elevated_user`, `deleted_model_copy`) are all administrative actions on domain objects, the same reasoning PLANNING.md's audit-log entry already used to exclude authentication/session bookkeeping from the audit trail. There is also no actor to RBAC-check against - the caller is the node itself over an already-authenticated connection, the same situation `nodes.AuthService.Authenticate` (not RBAC-gated either) is already in | Auditing every telemetry write anyway for consistency with other services (rejected: would flood the audit log with a few-second-interval, non-actor-attributable event stream that SCHEMA.md's own audit log framing was never meant to hold) |
| 2026-08-12 | Dashboard UI Phase 1 scoped to a base layout/sidebar shell plus three read-only pages (Dashboard, Nodes, Model profiles) - confirmed with the user rather than assumed, given how much larger this milestone item is than any prior one (the first real frontend code in the repo, and the first HTTP wiring for four previously-unwired services at once) | Explicit user choice between "full 8-section dashboard in one pass" and "shell + core read views first, remaining sections as later phases" - the latter, matching the phased-delivery precedent Model transfers/Model profiles already established for large items, and keeping each phase's own verification bar (real end-to-end run against the compiled binary) achievable in one sitting | Building all 8 sidebar sections and every write/action form in one pass (user declined - too large to verify thoroughly in one sitting, and several genuinely open design questions - login page UX, SSE wiring shape - would have been guessed rather than confirmed) |
| 2026-08-12 | UI verification for Dashboard UI Phase 1 used the real compiled `sparky-server` binary driven by `curl`+cookiejar against a real Postgres instance (page markup inspected directly), not a browser - confirmed with the user, since this sandboxed environment has no display | CLAUDE.md's "start the dev server and use the feature in a browser" bar cannot be literally met here - no headless browser tooling is available either. `curl` against the real running binary (real session cookie from a real `POST /login/break-glass`, real DB-backed data) verifies the actual HTTP/template/data pipeline end to end; only visual rendering (CSS layout, real browser DOM behavior) is unverified | Claiming a browser check happened anyway (rejected outright - misrepresenting verification that didn't occur); skipping end-to-end verification entirely and relying on `httptest` unit tests alone (rejected: unit tests use fakes, never proving the real binary/real Postgres/real template files actually work together) |
| 2026-08-12 | Each Dashboard UI page is parsed as base-layout-plus-that-one-page into its own isolated `*template.Template` (`internal/httpapi.loadPageTemplates`), not one `template.ParseGlob` across every page | Every page template defines a block named `"content"` (the htmx-swappable inner region) - `html/template` keeps one flat namespace per `*template.Template`, not a per-source-file one, so combining all pages into one parsed set would make every page's `"content"` block collide, with whichever was parsed last silently winning for every page. This was caught by reasoning through the mechanism before writing it, not discovered as a bug afterward | One shared `*template.Template` with a per-page-uniquely-named content block (e.g. `"dashboard-content"`, `"nodes-content"`) referenced dynamically from `base.html` (rejected: `html/template` has no clean way to parameterize *which* named template `{{template "X" .}}` invokes from within the template language itself - it would need Go-side string-building of template source, uglier than just parsing per-page) |
| 2026-08-12 | The sidebar (`web/templates/layouts/base.html`) lists only the three sections Phase 1 actually built (Dashboard, Nodes, Model profiles), not all eight CLAUDE.md's Frontend Conventions table names | A static nav link to a route that doesn't exist yet is a dead 404 link - real UX regression for a real user, not a cosmetic gap. Trivial to extend: each later phase adds its own `<li>` alongside its new route, in the same commit that adds the route | Rendering all eight now, with five pointing nowhere (rejected: shipping known-broken links to "look complete" is worse than an honest, incrementally-growing nav) |
| 2026-08-12 | `internal/nodes.Service`/`internal/profiles.Service`/`internal/lifecycle.Service` each gained a `List*` read method directly, rather than a separate new query-only service/package | These are the same `Service` types that already own each domain's RBAC-gated mutations (`RegisterNode`, `CreateProfile`, `LoadInstance`, ...) - adding an unguarded read method alongside them (no RBAC check needed, since viewing is available at the lowest tier) reuses the exact repository dependency each `Service` already holds, rather than standing up a second orchestration layer with its own copy of the same `nodeStore`/`profileStore`/`instanceStore` interface. `internal/db`'s own doc comments on `NodeRepository.List`/`ProfileRepository.List` already anticipated this exact call site ("for the future Nodes dashboard page") | A dedicated read-only query package per domain (rejected: doubles the interface surface for a one-method pass-through, and CLAUDE.md's Handler -> Service Layer -> Repository pattern doesn't distinguish "read service" from "write service" - it's one Service per domain) |
| 2026-08-12 | Dashboard UI's "Phase 2 and beyond" bundle (five sidebar sections, write forms, a login page, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the login page + logout control was built as Phase 2 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the 2026-08-12 entry scoping Phase 1 down from the full 8-section dashboard: the bundle as PLANNING.md originally described it was several genuinely distinct pieces of work, too large to verify thoroughly in one sitting | Building the whole "Phase 2 and beyond" bundle in one pass (not offered as the recommended option, given the Phase 1 precedent); building one more read-only section (Transfers) or all five remaining read-only sections instead (both offered, user chose the login page) |
| 2026-08-12 | `POST /login` serves both the existing JSON API contract and the new login page's `application/x-www-form-urlencoded` form submission, branching on `Content-Type` inside one handler (`isFormRequest`), rather than a second endpoint the way `POST /login/break-glass` is separate from `POST /login` | The break-glass precedent's separation is about identity source (AD vs. a credential that isn't a Users row at all) - a real semantic difference worth a distinct URL. A browser form and a JSON client logging in via AD are the same identity source and the same `LoginService.Login` call, differing only in response format (redirect vs. JSON) - ordinary HTTP content negotiation, not the kind of conflation the break-glass precedent guards against. Chi also has no method+path dispatch finer than exact (method, path) pairs, so serving both at the same bookmarkable `/login` URL requires branching inside one handler regardless | A second path (e.g. `/login/form`) mirroring break-glass's shape (rejected: manufactures a URL distinction where none of the underlying semantics differ, and loses `/login` as a single bookmarkable/memorable URL for both a browser and an API client) |
| 2026-08-12 | A failed login-page form submission re-renders `login.html` in place with an error message, rather than redirecting back to `GET /login` with the error carried some other way | No flash-message or session-backed mechanism exists anywhere in this codebase to carry a one-time message across a redirect, and building one is a real, separate feature. Re-rendering in place is the standard pattern for a server-rendered form with no such infrastructure, and keeps the failure path a single request/response, easier to test | A query-parameter error code on a redirect to `GET /login?error=...` (rejected: leaks the failure reason into browser history/referrer headers, and `GET /login`'s existing already-authenticated-redirect check would need to special-case it) |
| 2026-08-12 | `RequireSession` keeps its existing JSON 401 for an unauthenticated request to `/dashboard`/`/nodes`/`/profiles`, not a redirect to the now-real `/login` page | An htmx partial (`HX-Request` header set) fetch that received a redirect would have it followed transparently by the browser's `fetch` implementation, landing `login.html`'s full standalone `<html>` document nested inside `#main-content` - broken markup, not a real fix. Correctly redirecting only a full-page navigation (not an htmx partial) needs to inspect `HX-Request` before deciding, which is additional, untested logic outside this phase's confirmed scope (login page + logout control) | Redirecting unconditionally regardless of `HX-Request` (rejected: verified against the real htmx source this session - the vendored `htmx.min.js` follows a redirect via the browser's normal fetch redirect-following, with no awareness that the origin request was a partial fetch - so this would visibly break navigation away from an expired session mid-page) |
| 2026-08-12 | Neither CSRF protection nor authentication-endpoint rate limiting was added as part of the login page, despite CLAUDE.md Security Considerations calling for both | Both are pre-existing, app-wide gaps that predate this task and span every POST route already built (profile create/edit, node registration, transfers, instance load/unload, not just login) - fixing either one properly means a coordinated pass across every existing write route, not something to bolt onto one new form as a side effect. Flagged explicitly in Known Issues rather than silently left unaddressed | Adding CSRF protection to just the new login form (rejected: inconsistent - every other existing POST route stays unprotected, giving false confidence about this one); adding a rate limiter now (rejected: CLAUDE.md requires discussing a new Go module dependency first, and a hand-rolled version is a real design decision on its own, not a login-page side effect) |
| 2026-08-12 | Dashboard UI's "Phase 3 and beyond" bundle (four remaining sidebar sections, write forms, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the Transfers section was built as Phase 3 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the two prior phase-scoping entries (2026-08-12, twice): the bundle as PLANNING.md described it was several genuinely distinct pieces of work. Transfers specifically was chosen as "smallest, most direct continuation of what's already proven to work" - it's the same read-only-list shape Phase 1 already established for three other domains | Building all five remaining sections at once, write/action forms for the three Phase 1 pages, or the `OnMessage` dispatcher consolidation instead (all three offered as explicit alternatives; user chose Transfers) |
| 2026-08-12 | `internal/transfers.Service` is constructed in `cmd/sparky-server/main.go` for the first time by this phase, but `HandleTransferProgress` is still not wired as `agentconn`'s `onMessage` callback | The Transfers page's `ListTransfers` is a pure read from `ModelTransferRepository.List` - it has no dependency on live agent dispatch or progress reporting at all. Wiring `onMessage` is a separate, already-identified unit of work (the OnMessage dispatcher consolidation option this phase's own scoping choice explicitly declined) with its own three-way combination logic (`HandleTransferProgress`/`HandleInstanceResult`/`HandleTelemetry`) - conflating it with a read-only page would blur two unrelated decisions into one commit | Wiring `onMessage` now since a real `Service` instance exists anyway (rejected: `Service` existing and `onMessage` needing it are unrelated - the constructor call was already going to happen the moment any HTTP-facing caller needed the `Service`, read or write) |
| 2026-08-12 | Dashboard UI's "Phase 4 and beyond" bundle (four remaining sidebar sections, write forms, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the Audit log section was built as Phase 4 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the three prior phase-scoping entries (2026-08-12, three times now): the bundle was several genuinely distinct pieces of work. Audit log specifically was offered as "of the four remaining sections, most similar in shape to what's already built - a straightforward list, no aggregation or charting" (unlike Metrics or Settings) | Building all four remaining sections at once, write/action forms for the three Phase 1 pages, or the `OnMessage` dispatcher consolidation instead (all three offered as explicit alternatives; user chose Audit log) |
| 2026-08-12 | Audit log's RBAC check (`rbac.CanViewAuditLog`) lives inside `audit.Recorder.List` itself, not in a separate HTTP middleware (`RequireAdmin` was considered and not built) | Matches this codebase's existing precedent for every write action already gated (`transfers.Service.InitiateTransfer`, `rbac.Service.ElevateTier`, etc.): the permission check happens inside the Service-layer method, which takes `rbac.Actor` as a real parameter and returns `rbac.ErrNotPermitted` on refusal, so the guarantee travels with the method regardless of caller - a CLI tool or a different code path calling `Recorder.List` directly gets the same protection an HTTP middleware would only enforce for requests that happen to pass through it | A dedicated `RequireAdmin` middleware chained after `RequireSession` (rejected: would duplicate the authorization decision in two places - the middleware and, eventually, any other caller of `Recorder.List` - for no benefit over checking once, in the one place that's always on the path) |
| 2026-08-12 | `internal/audit`'s `writer` interface (and the `Recorder` field backing it) was renamed to `store`, widened to require both `Write` and `List` | `Recorder` is `audit_log`'s single sanctioned access point for both directions now, not writer-only - its own doc comment already framed it that way ("the only sanctioned path to the audit log," not "the only sanctioned way to write to it"). One interface matches that framing; `*db.AuditRepository` (the only real implementation) already satisfies both methods naturally, so no production call site changed, only the package's own test fake gained a `List` method | Two separate interfaces (a `writer` for `Record` and a distinct `reader` for `List`), composed via two fields on `Recorder` (rejected: `Recorder` has exactly one backing store either way - `*db.AuditRepository` - so two fields pointing at the same real value in production adds indirection with no actual decoupling benefit) |
| 2026-08-12 | Dashboard UI's "Phase 5 and beyond" bundle (remaining three sidebar sections, write forms, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the Users & permissions section was built as Phase 5 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the four prior phase-scoping entries (2026-08-12, four times now): the bundle was several genuinely distinct pieces of work. Users & permissions specifically was offered as "a straightforward list, same shape as every prior phase, smallest continuation of what's proven to work" - it also reuses `UserRepository.List`, already built for Phase 4's actor-name resolution | Building all three remaining sections at once, the Metrics section (rejected as an option too, since it needs Chart.js/aggregation - a new UI shape, not a continuation), write/action forms, or the `OnMessage` dispatcher consolidation instead (all offered as explicit alternatives; user chose Users & permissions) |
| 2026-08-12 | Users & permissions' RBAC check (`rbac.CanViewUsers`) lives in `rbac.Service.ListUsers`, not `internal/audit`-style inside a new dedicated Service, and not reusing `audit.Recorder.List`'s own check | `rbac.Service` already exists and already wraps `UserRepository` via its `userStore` interface (for `ElevateTier`) - widening `userStore` with `List` and adding `ListUsers` alongside it keeps every RBAC-gated action on `Users` in one place, rather than splitting Users-related authorization across `rbac.Service` (writes) and a new package (reads). This also means `cmd/sparky-server/main.go` constructs a real `rbac.Service` for the first time, ahead of `ElevateTier` having any HTTP caller | A new `internal/users` package mirroring `internal/audit`'s shape (rejected: `rbac.Service` was already the RBAC-gated home for `Users` table access via `ElevateTier` - a second package covering the read side of the exact same table would split one concern in two for no benefit); reusing `rbac.CanViewAuditLog` for this page too (rejected: same tier floor today, but a distinct capability conceptually - Audit log and the user roster could diverge in who may view them later, and `CanViewAuditLog`'s own doc comment is specific to the Audit log) |
| 2026-08-12 | Dashboard UI's "Phase 6 and beyond" bundle (remaining two sidebar sections, write forms, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the Settings section was built as Phase 6 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the four prior phase-scoping entries (2026-08-12, five times now): the bundle was several genuinely distinct pieces of work. Settings specifically was offered as "no aggregation or charting, closest in shape to the list/detail pages already built" - it turned out to need two new migrations (see the row below), which the scoping discussion did not anticipate, but the page shape itself was still the smallest continuation among the offered options | Building both remaining sections at once, the Metrics section (rejected as an option too, since it needs Chart.js/aggregation - a new UI shape, not a continuation, same reasoning Phase 5's own scoping already used to decline it), write/action forms, or the `OnMessage` dispatcher consolidation instead (all offered as explicit alternatives; user chose Settings) |
| 2026-08-12 | `audit_settings.retention_months`'s seeded default is 12 months | SCHEMA.md had never stated a default for this column (unlike `forwarding_protocol`, whose `syslog` default it already documented) - discovered only once the migration was actually being written, since building the Settings page's read view required creating the table for the first time. Confirmed with the user rather than guessed: a middle value between the Metrics table's own 6-month raw-resolution retention window and this column's stated 24-month ceiling | 6 months, matching the Metrics table's window exactly (offered, not chosen - the two retention policies govern unrelated tables, so matching them isn't obviously more correct than a middle value); 24 months, the maximum (offered, not chosen - would mean nothing is ever pruned by default) |
| 2026-08-12 | `metrics_export_config` and `audit_settings` (SCHEMA.md-documented tables that had never actually been migrated) are seeded with exactly one default row at migration time, using the same `id boolean PRIMARY KEY DEFAULT true` plus `CHECK (id)` singleton pattern as `break_glass_credential` - but, unlike that table, with an `INSERT` in the same migration rather than left absent until first configured | Break-glass credential legitimately has a "not set up yet" state before `sparky set-superadmin-password` ever runs - `BreakGlassRepository.Get` returning `ErrBreakGlassNotSet` is a real, meaningful signal. Metrics export config and Audit settings have no equivalent real-world unset state: the app (today, this page's read path; later, an actual export/forwarding job) needs an effective setting on every read, and "not configured" is indistinguishable in practice from "configured to do nothing" (`backend_type = 'none'`, `forwarding_enabled = false`) - so the schema states that explicitly as the seeded default rather than making every future caller handle a third, redundant empty state | Leaving both tables empty until an Admin visits a future write form and saves once (rejected: would make this phase's own read-only Settings page's `Get` calls handle a not-yet-configured error state that has no real meaning - the singleton is either configured to do something or configured to do nothing, never absent) |
| 2026-08-12 | Neither `internal/metrics.Service` nor `internal/audit.Recorder` gained the Settings page's RBAC-gated read; a new `internal/settings` package was created instead, wrapping both `MetricsExportConfigRepository` and `AuditSettingsRepository` behind one `Service.Get(ctx, actor)` | `internal/metrics.Service`'s own doc comment already scopes it to telemetry ingestion only, explicitly deferring NFS/S3 export itself to the v0.4.0 Historical metrics milestone - bolting a config *read* onto it now would blur ingestion-only scope with a v0.4.0-scoped concern before that milestone exists. `internal/audit.Recorder` is scoped to the `audit_log` table specifically, not the separate `audit_settings` table that configures its optional forwarding - the two tables are related but distinct. Rather than split the one RBAC decision behind a single page's data across two unrelated packages, one new package owns it, matching the `internal/audit`/`rbac.Service` precedent of a Service-layer method taking `rbac.Actor` and returning `rbac.ErrNotPermitted` | Extending `internal/metrics.Service` and `internal/audit.Recorder` each with their own gated read, and having `handleSettings` call both separately (rejected: two RBAC checks for one page's one "may view Settings" decision, and neither package's existing scope actually fits the config-read concern being added) |
| 2026-08-12 | Dashboard UI's "Phase 7 and beyond" bundle (the Metrics section, write forms, SSE wiring, `OnMessage` dispatcher consolidation) was split, and only the Metrics section was built as Phase 7 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the five prior phase-scoping entries (2026-08-12, six times now): the bundle was several genuinely distinct pieces of work. Metrics was offered as "the eighth and final sidebar page - completes the full read-only Dashboard UI," the first page needing aggregation/charting rather than a plain table, explicitly flagged as a new UI shape rather than a continuation (the same framing that led to it being declined as an option in Phases 5 and 6's own scoping) | Write/action forms or the SSE-wiring-plus-`OnMessage`-consolidation slice instead (both offered as explicit alternatives; user chose Metrics) |
| 2026-08-12 | Chart.js 4.4.4 was vendored at `web/static/js/chart.umd.min.js` (fetched once from a CDN during development, then committed as a static asset) rather than loaded from a CDN at runtime | ARCHITECTURE.md's Deployment Topology already settled this before this phase: "Static assets: embedded in the binary via `embed.FS`, served directly - no CDN." CLAUDE.md's Tech Stack table left "CDN or vendored" open for Chart.js specifically, but the more specific ARCHITECTURE.md policy governs runtime behavior; `htmx.min.js` was already vendored the same way, so this keeps every third-party frontend asset in the binary consistently. Chart.js 4.4.4 is MIT-licensed (confirmed via the npm registry before vendoring), compatible with this AGPLv3-or-later project | Loading it from `cdn.jsdelivr.net` at runtime (the option CLAUDE.md's own wording would have permitted) - not seriously considered once ARCHITECTURE.md's existing "no CDN" policy was checked, since the whole point of embedding every other asset is a single self-contained binary with no runtime dependency on external network reachability |
| 2026-08-12 | `chart.umd.min.js` and `metrics.js` are loaded unconditionally in `base.html`'s `<head>`, on every page, not only when navigating to Metrics | The alternative - loading them only from inside `metrics.html`'s own content block, the same place its data script lives - depends on precisely how htmx executes multiple sequentially ordered `<script src>` tags injected into swapped content, a behavior this sandboxed environment has no browser to verify empirically (this project's own stated discipline: never assert third-party library behavior without testing it directly). Loading both scripts unconditionally in `<head>` sidesteps that uncertainty entirely: ordinary synchronous `<head>` scripts are guaranteed by basic HTML parsing (not htmx-specific behavior) to finish loading and executing before `<body>` is parsed, so `Chart` and `initMetricsChart` are always defined by the time any page's content - including an htmx-swapped Metrics page reached from elsewhere - needs them. The cost is an unconditional ~200KB load on every page view, accepted as a reasonable trade-off for a "handful of nodes, small internal team" scale application (CLAUDE.md Project Overview) | Loading both scripts only inside `metrics.html`'s content block, re-fetched (browser-cached, so cheap) and re-executed on every htmx navigation to that page (rejected: correctness would rest on an unverified assumption about htmx's script-execution ordering for dynamically inserted external scripts, not empirically confirmed given no browser is available here); a `{{if eq .ActiveSection "metrics"}}` conditional in `<head>` (rejected: `<head>` is never touched by an htmx partial swap, so a conditional there would only take effect on a full page load, not when navigating to Metrics via htmx from another page - it would work by accident on this phase's own manual curl-based verification, which always does a fresh full-page request, and only fail in the one case that actually matters, an in-app htmx navigation) |
| 2026-08-13 | Dashboard UI's "Phase 8 and beyond" bundle (four write/action forms plus SSE wiring plus `OnMessage` dispatcher consolidation) was split, and only the Users & permissions tier-change form was built as Phase 8 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the six prior phase-scoping entries (now seven times): the bundle was several genuinely distinct pieces of work. The elevation form was offered as "smallest, most self-contained option... pure database write, no agent/WebSocket dependency at all" - `rbac.Service.ElevateTier` was already fully built and tested before this milestone even started, so this phase only had to wire an HTTP caller for it | Node registration or Model profile create/edit instead (both offered as CRUD alternatives with no agent dependency, but each with its own real wrinkle - node registration's once-only bearer token display, profile create/edit's free-form `engine_params` JSON); the `OnMessage` consolidation plus SSE wiring instead (offered too, explicitly flagged as a real prerequisite for the instance load/unload form specifically, not for this one; user chose the elevation form) |
| 2026-08-13 | `reachableTiers` (a new `internal/httpapi` helper) computes the tier-change dropdown's offered options by calling `rbac.CanElevate` once per candidate tier, rather than any simpler rule like "always offer the two adjacent tiers" | The dropdown must never offer an option the server-side check would then refuse - deriving it from the exact same function `handleElevateUser`'s downstream `rbac.Service.ElevateTier` call ultimately depends on (rather than a hand-written approximation of `CanElevate`'s rules) makes that guarantee structural, not just a matter of the two staying in sync by convention. The current tier itself is excluded from the offered set - `CanElevate` doesn't special-case a same-tier transition (a `SuperAdmin` actor's unconditional `true` covers it too), so `ElevateTier` would accept and audit a semantically-empty change, but offering it in the UI serves no purpose | Precomputing a static allow-list per (actor tier, target tier) pair as a lookup table (rejected: would need to be manually kept in sync with `rbac.CanElevate`'s own logic instead of calling it directly - the same duplication risk `reachableTiers`'s actual design avoids) |
| 2026-08-13 | The Users & permissions tier-change form (`POST /users/{id}/tier`) was added without any CSRF protection, same as every other write route in the app | This is the first new state-changing endpoint added since the app-wide CSRF gap was documented (PLANNING.md Known Issues, Dashboard UI Phase 2), which already reasoned that retrofitting CSRF protection needs a coordinated pass across every existing write route at once, not something bolted onto one form as a side effect of an unrelated task. That reasoning applies identically to a *new* write route: protecting this one form in isolation while every other POST endpoint (login, logout) stays unprotected would be an inconsistent half-measure, not a real fix. The existing Known Issues row is extended to name this endpoint explicitly rather than left silently exposed to a future reader of that row | Adding CSRF protection to just this one new endpoint (rejected: solves nothing security-wise while creating an inconsistent app where some POST routes are protected and others aren't, and still requires the same coordinated-pass work eventually) |
| 2026-08-13 | Dashboard UI's "Phase 9 and beyond" bundle (two more write/action forms plus SSE wiring plus `OnMessage` dispatcher consolidation) was split, and only the node registration form was built as Phase 9 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the seven prior phase-scoping entries (now eight times): the bundle was several genuinely distinct pieces of work. Node registration was offered alongside profile create/edit and the `OnMessage`-plus-SSE slice, each with its own explicitly-named wrinkle (this one's once-only bearer token display; profile create/edit's free-form `engine_params` JSON; the `OnMessage` slice's real dependency for the *next* form, instance load/unload) rather than presented as uniformly simple options - `nodes.Service.RegisterNode` was, like `ElevateTier` before it, already fully built and tested before this milestone started | Model profile create/edit or the `OnMessage` consolidation plus SSE wiring instead (both offered as explicit alternatives; user chose node registration) |
| 2026-08-13 | The node registration form's successful submission renders a dedicated confirmation page showing the plaintext bearer token, rather than reusing Phase 8's `HX-Redirect`-back-to-the-list pattern | `nodes.Service.RegisterNode`'s own doc comment is explicit: the token is "shown here only once" and never persisted anywhere but its hash. An `HX-Redirect` back to `/nodes` would discard it the instant the browser navigated - there would be no second chance to retrieve it, unlike every other write action in this app so far where redirecting to the updated list is harmless because the list itself reflects the new state. The whole registration flow (including the form submission) also deliberately uses a plain `<form method="post">` full-page navigation rather than an `hx-post` partial swap, sidestepping any need to reason about htmx's swap semantics for a page whose entire purpose is a value the Admin must actually read before navigating anywhere else | Showing the token in a toast/banner over the redirected `/nodes` page via query parameter or flash-message mechanism (rejected: no such mechanism exists in this app yet, and building one for a single one-time-secret display would be more machinery than the plain-confirmation-page alternative, for a form that isn't performance- or frequency-sensitive enough to need `hx-post`'s smoother partial swap) |
| 2026-08-13 | Dashboard UI's "Phase 10 and beyond" bundle (profile create/edit form plus SSE wiring plus `OnMessage` dispatcher consolidation) was split, and only the profile create/edit form was built as Phase 10 - confirmed with the user rather than assumed, presented as an explicit multi-option choice | Same reasoning as the eight prior phase-scoping entries (now nine times): the bundle was still two genuinely distinct pieces of work even after node registration's own scoping (Phase 9) narrowed it. Profile create/edit was offered as completing all four originally-planned write/action forms except instance load/unload, with its own explicitly-named wrinkle (free-form `engine_params` JSON) rather than presented as uniformly simple; the `OnMessage`-plus-SSE option was offered too, explicitly flagged as unblocking instance load/unload as the *next* slice rather than being that form itself | The `OnMessage` consolidation plus SSE wiring instead (offered as the only other option; user chose the profile form) |
| 2026-08-13 | `profiles.Service` gained a new `GetProfile` method (wrapping `ProfileRepository.FindByID`, already exported at the repository layer but previously unexposed through the Service) rather than the edit form's prefill reusing `ListProfiles` and filtering client-side for the matching ID | `ListProfiles` already exists specifically for the read-only list page and returns every profile; filtering a full list down to one record in the HTTP handler to prefill a form is presentation-layer plumbing standing in for what should be a real single-record Service read, the same category of thing `nodes.Service`/`rbac.Service` already expose for their own single-record needs (`FindByID`/`FindByADSID` equivalents). Unlike Phases 8 and 9, which needed zero domain-package changes because the write methods pre-existed complete, this phase's read-side gap was real and worth closing properly rather than working around in the wrong layer | Filtering `ListProfiles`'s full result in `handleEditProfileForm` (rejected: technically works for a "handful of nodes" scale roster, per CLAUDE.md Project Overview, but is exactly the kind of misplaced business-logic-in-a-handler CLAUDE.md's Handler -> Service Layer -> Repository pattern exists to prevent, and leaves `ProfileRepository.FindByID` sitting unused at the Service layer for no reason) |
| 2026-08-13 | Dashboard UI Phase 11's SSE wiring builds the full server-side broker (`internal/events`) and `GET /events` endpoint, plus a minimal client (`web/static/js/sse.js`) that triggers a debounced htmx refetch of the current page on a relevant event - not fine-grained live DOM patching and not a server-only stub with no client wiring - confirmed with the user as an explicit multi-option choice | Unlike every prior "Phase N and beyond" scoping decision in this log (nine times now), this was not a question of which independent slice to build next - PLANNING.md's own Phase 11 entry already named `OnMessage` consolidation, the load/unload form, and SSE wiring as one hard dependency chain, no longer splittable. The remaining choice was how much of SSE's own scope to build: a server-only stub would leave this phase's stated goal (seeing a load/unload/transfer/telemetry change without a manual reload) undelivered; fine-grained live DOM patching (a live-appended Chart.js point, an in-place progress bar/status badge update) was rejected as substantially more frontend JS to write and reason about with no browser available in this sandboxed environment to visually verify it, and a full-page-content refetch already fully solves what this phase needs without it | Fine-grained live DOM patching per event type (rejected: more JS, unverifiable here); broker + endpoint with no client consumption yet, deferred to a later phase (rejected: would leave the phase's own live-update goal undelivered) |
| 2026-08-13 | `nodes.node_type` (`spark` / `docker-gpu`) + `container_runtime` (`docker` / `podman`, CHECK-paired) collapsed into a single `runtime_backend` enum (`docker` / `podman` / `bare-metal`, migration `000014_nodes_collapse_runtime_backend`); "spark" retired as a schema value entirely | The 2026-08-06 design (see that date's entry below) conflated a hardware label with a runtime-mechanism choice, backwards: it assumed Spark needs the bare-metal (no-container, direct-exec) backend and non-Spark hosts use Docker/Podman. A DGX Spark's GB10 GPU supports passthrough to a container without affecting a display connected to it - NVIDIA's supported use case for that hardware, confirmed with the user rather than assumed, though neither of us has independently verified the underlying mechanism - so CDI GPU passthrough into a container should work fine there; the RTX 4090 laptop - this project's own primary v0.1.0 dev/test hardware - is the case that actually needs bare-metal, since its GPU is already claimed by the host OS's own session and can't be handed to a container at all, independent of whether the hardware happens to be a Spark. Separately, the two-column CHECK-constrained shape only ever expressed one 3-way choice (`docker-gpu` was never meaningful without a paired `container_runtime`) behind a constraint rejecting the other five combinations - collapsing to one column makes those states unrepresentable instead of merely rejected, and matches ARCHITECTURE.md's own pre-existing "Runtime Backends: Bare-metal, Docker/Podman" vocabulary exactly. Nothing else in the system (GPU/CPU memory equality, ARM64 cross-compilation, v0.3.0 fabric-group clustering) needs a discrete "is-this-hardware-a-Spark" flag - a descriptive `name` (already free text, e.g. `spark-1`) covers the human-labeling need with zero special-casing, same as before. `SPARKY_NODE_TYPE`/`SPARKY_CONTAINER_RUNTIME` (agent env vars) collapsed to `SPARKY_RUNTIME_BACKEND` the same way, though the agent doesn't yet branch on this value for backend selection either way - that's still real v0.2.0 work. `SPARKY_MODEL_STORAGE_PATH`'s documented default (`/home/serviceloop/models`) is kept as-is, only its stated reasoning changes (a bare-metal host, not "on Spark") - confirmed with the user, since nothing implements that default yet regardless (PLANNING.md Known Issues). PLANNING.md's Milestones v0.2.0 entry and Dependencies and Blockers are corrected to match (also confirmed with the user); already-completed Phase write-ups describing the old schema as it was actually built (Node registry, Model transfers Phase 1's CHECK-pairing mention, Dashboard UI Phase 9's registration-form entry) are deliberately left unedited - they're an accurate historical record of what those PRs built at the time, matching this project's existing never-rewrite-history discipline (migrations are never edited after creation; CHANGELOG corrections get a new entry, not a rewrite of the old one) | A minimal rename only (`node_type`'s `spark` value renamed to `bare-metal` via `ALTER TYPE ... RENAME VALUE`, `container_runtime` left as a second column) - considered and explicitly offered to the user as the smaller, lower-risk option; rejected in favor of the collapse once raised, since it would have kept the CHECK-constrained two-column pattern in place for something that's really one concept, and the collapse's own diff (while larger) is still fully contained to the `nodes` table and its direct Go/HTTP/template callers |
| 2026-08-13 | The new break-glass IP allowlist (`BREAKGLASS_ALLOWED_IPS`, gating the new `/login/break-glass` GUI page and its existing JSON API contract) determines the client's IP from `r.RemoteAddr` (the direct TCP peer), not `X-Forwarded-For`/`X-Real-IP` - confirmed with the user rather than assumed | Not spoofable by the client, unlike a header the client itself can set; correct for this control's actual motivating use case (direct-connection local/break-glass testing without AD). The reverse-proxy-in-front topology ARCHITECTURE.md's Request Lifecycle documents means `RemoteAddr` is the proxy's own address there, not the real client's - an accepted, documented tradeoff, not solved here; no trusted-proxy-config concept is introduced | Trusting `X-Forwarded-For` (rejected: client-controllable unless a trusted-proxy concept is introduced to strip/validate it, which this codebase has no precedent for anywhere - defeats the whitelist's purpose entirely for anyone who can reach the app directly or add an arbitrary header) |
| 2026-08-13 | `BREAKGLASS_ALLOWED_IPS` unset/empty allows from anywhere (the whitelist becomes a no-op) rather than defaulting to deny-all - confirmed with the user rather than assumed | Matches this project's existing "optional security control, off by default" precedent (`AUDIT_FORWARD_ENABLED`) - an unset value on upgrade must not silently lock out every existing break-glass caller (JSON API scripts, ops runbooks already depending on `/login/break-glass` with no IP restriction) | Deny-by-default when unset (rejected: a breaking change for every existing deployment on upgrade, with no migration path other than requiring every operator to set this before updating) |
| 2026-08-13 | The bare-metal install script is agent-only (`sparky-agent`); the central app has no packaged bare-metal installer at all - `go run ./cmd/sparky-server`/a built binary, no `scripts/install.sh`-equivalent. `ARCHITECTURE.md`'s Deployment Model bullet and `README.md`'s Quick Start, which both implied a single unified installer for both binaries, are corrected in this same change | `CLAUDE.md`'s own detailed Build and Run section never described a server installer anywhere, and the user's own framing of this task was exclusively agent-scoped ("the bare metal **agent** will be distributed in 3 ways") - the two higher-level docs had simply drifted out of sync with that reality. Left uncorrected, they'd reference a script name (`scripts/install.sh`) that doesn't exist under that name after this change at all - the real script is `install_agent.sh`, and it only ever installs the agent | Building a matching packaged installer for the central app too (not raised as a real option - out of scope for what was asked, and no prior detailed doc ever described one existing) |
| 2026-08-13 | `sparky-agent`'s binary installs to `/opt/sparky/bin/sparky-agent` for all three distribution methods (`.deb`, `.rpm`, tarball) | User's explicit choice, overriding an initial `/usr/bin` (packaged) / `/usr/local/bin` (tarball) recommendation - FHS reserves `/usr` for package-manager-owned files and `/usr/local` for locally-installed software, which would have meant two different binary paths depending on install method; the user preferred one consistent path for all three instead. Since `/opt/sparky/bin` isn't on any distro's default `$PATH`, a `/usr/local/bin/sparky-agent` symlink is shipped alongside it (package-tracked for `.deb`/`.rpm`, created/removed explicitly by `install_agent.sh`/`uninstall_agent.sh` for the tarball) so the binary is still invokable by name for ad hoc use (diagnostics; the future v0.2.0 `sparky-agent setup` subcommand) | `/usr/bin` (packaged) + `/usr/local/bin` (tarball) as originally recommended (user declined in favor of one path for all three) |
| 2026-08-13 | Packaging built via `nfpm` (a single static Go binary, one YAML config producing both `.deb` and `.rpm`) rather than hand-rolled `dpkg-deb`/`rpmbuild` control files or GoReleaser | Matches this project's own "own it, don't add dependencies you don't need" pattern already on record for `chi`/`golang-migrate`: nfpm is a build-time-only CLI tool (`go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`, same install shape as the already-documented `golang-migrate`), never imported into `go.mod`, and avoids maintaining two per-distro toolchains (`rpmbuild` specifically needs a real build environment) in sync by hand. Its actual config semantics were verified empirically against a real installed `nfpm`, not assumed from documentation alone - two real surprises found this way and designed around: `contents[].src`/`scripts.*` paths resolve relative to nfpm's own working directory at invocation time, not relative to the config file's location, and `${VAR}`-style env expansion only applies to certain top-level fields (`arch`/`version`/`maintainer`/etc.), not to `contents[].src` - `scripts/build_packages.sh` always invokes `nfpm` from the repo root and stages the per-arch binary at a fixed non-arch-suffixed path (`dist/build/sparky-agent`) before each invocation to work within both constraints | GoReleaser (rejected: does everything nfpm does via nfpm internally, plus GitHub-release/changelog automation this task never asked for - meaningfully heavier than needed); hand-rolled `dpkg-deb`/`rpmbuild` (rejected: real duplication keeping two distro-native toolchains' control files in sync by hand, and `rpmbuild` specifically needs a full build environment this project's own minimal-infrastructure philosophy would otherwise avoid) |
| 2026-08-13 | Distribution is raw artifacts only for now (`.deb`/`.rpm`/`.tar.gz` files, e.g. attached to releases, installed via `dpkg -i`/`rpm -i`/tarball extraction) - no signed apt/dnf repository, no GPG key infrastructure | User's explicit call: revisit a real repository "if the project's popularity shows there's a need." `scripts/build_packages.sh`'s output is kept flat and SemVer-clean specifically so a future `reprepro`/`createrepo`-based repo-publish step, if it's ever needed, is an addition on top of this build, not a restructuring of it | Building signed repository infrastructure now (rejected: real, ongoing maintenance overhead - key management, repo metadata generation, hosting - unlikely to be worth it yet at CLAUDE.md's own stated "handful of nodes, single internal team" scale) |
| 2026-08-13 | `apt remove`/`dnf remove` stop and disable the service and remove the binary, but leave `serviceloop` and `/etc/sparky-agent/secrets.env` in place; only `apt purge` (native dpkg semantics) removes both. RPM has no equivalent purge concept at all - researched directly (`%preun`/`%postun` scriptlets only ever receive a numeric install-count, 0 or 1+, with no separate "and delete everything" signal `dnf`/`rpm` can produce, unlike dpkg's own `purge` argument) rather than assumed - so `scripts/packaging/purge_rpm.sh` is copied to a package-unowned path (`/usr/local/sbin/sparky-agent-purge.sh`) by the `preremove` scriptlet just before removal deletes everything else the package owns, specifically so a real cleanup command still exists on the box afterward, and `docs/AGENT.md` states the RPM gap plainly rather than pretending purge works the same way there | Standard Debian/RPM packaging convention for the deb side (avoids orphaned file ownership and accidental credential loss on a routine `remove`); the RPM half is an honest answer to a real platform limitation, not a workaround - discovered mid-implementation when a first attempt at `purge_rpm.sh` was ordinary package content and got deleted by a plain `dnf remove` before it could ever run, caught by the container-based verification pass (see this milestone's own "Done" writeup) and fixed by copying it out before removal rather than by loosening what plain `remove` does | A plain `remove` that also deletes `serviceloop`/`secrets.env` (rejected: diverges from standard packaging conventions and risks silently destroying a real bearer token on a routine removal someone expected to be reversible) |
| 2026-08-13 | The bare-metal runtime backend (`agent/runtime/baremetal`) and the existing Docker/Podman backend now share one `agent/runtime.Backend` interface (`Start`/`Stop`/`Shutdown`, plus a generalized `runtime.Spec` superseding the old `containers.Spec`), rather than `agent/connection` branching on `cfg.RuntimeBackend` between two differently-shaped interfaces - confirmed with the user as an explicit two-option choice | Matches the one-interface/swap-the-concrete-implementation pattern already used for `transferExecutor`/`telemetryCollector` in the same package - `agent/connection` stays backend-agnostic, and `cmd/sparky-agent` picks the concrete implementation once at startup from `cfg.RuntimeBackend`. `containers.Backend` was refactored onto the shared interface with no behavior change (`Start`/`Stop` derive the container name internally via the existing `InstanceContainerName`, same as before; `Shutdown` is a no-op, matching the already-documented "containers survive an agent restart" design) | Two separate interfaces with `connection.go` branching between them (offered as the lower-upfront-restructuring alternative; rejected in favor of keeping backend-type awareness out of `connection.go` entirely) |
| 2026-08-13 | `agentproto.LoadInstance` gained an `EngineType` field (plain string, e.g. `"vllm"`/`"llamacpp"` - same values as `db.ProfileEngineType`, carried without an `internal/db` import per this package's existing `TransferProgress.Status`/`InstanceResult.Status` convention), and the bare-metal backend resolves which local executable to exec via one new optional env var per engine type (`SPARKY_LLAMACPP_BINARY_PATH`/`SPARKY_VLLM_BINARY_PATH`, only `vllm`/`llamacpp` having adapters today) - confirmed with the user as an explicit two-option choice | `engines.LaunchSpec.Image` is a Docker image reference with no bare-metal meaning at all - there was no field anywhere telling the agent which local binary to run for a given engine, a real protocol gap this milestone's research surfaced rather than something `agent/connection` could work around on its own. `internal/lifecycle.Service.LoadInstance` already has `profile.EngineType` in scope where it builds this payload, so populating the new field cost one line. A binary's on-disk location is inherently host-specific (venv paths, install locations vary per machine), so the agent - not the central app - is the right place to resolve it; a load request for an engine type with no configured path on that node fails clearly through the existing `InstanceResult` `status=failed` path, the same mechanism already used for every other real launch failure | A single `SPARKY_ENGINE_BIN_DIR` directory convention with fixed expected binary names inside it (e.g. `llama-server`, `vllm`) instead of one env var per engine type - offered as the lower-config-sprawl alternative as more engines are added later; rejected in favor of an explicit named path per engine type, consistent with every other environment variable in this project being explicitly named rather than convention-based |
| 2026-08-14 | `serviceloop`'s home directory (and, with it, the bare-metal runtime backend's default `SPARKY_MODEL_STORAGE_PATH`) moved from `/home/serviceloop`/`/home/serviceloop/models` to `/opt/sparky/serviceloop`/`/opt/sparky/serviceloop/models` - `scripts/packaging/lib/agent-common.sh`'s `useradd` gained `--home-dir`, and a new `ensure_model_storage_dir` function (`install -d`, run after `ensure_serviceloop_user` specifically because `install -o serviceloop -g serviceloop` needs that account to already exist to resolve the ownership) creates it on every install and upgrade | Found by inspection, before any GPU hardware was touched, while preparing the real-hardware validation runbook: `deploy/systemd/sparky-agent.service`'s `ProtectHome=true` makes all of `/home/*` inaccessible to the running process, and `useradd --no-create-home` never created `/home/serviceloop` in the first place - the documented bare-metal default path would have failed outright under the real packaged install, the very first thing hardware validation would have hit. `/opt/sparky` was chosen over the alternatives (a `/var/lib/sparky-agent/models`-style FHS location, or dropping `ProtectHome=true`/switching to `--create-home`) because it keeps every sparky-agent-owned path under the one tree the binary and packaged share files already live in, and sidesteps `ProtectHome=true` entirely rather than needing an exception carved out of it - `/opt` isn't a path that directive touches at all. `agent/transfer.Executor.Download` already `MkdirAll`s everything below the model storage root at write time, so the fix only needed to ensure the home directory itself exists and is `serviceloop`-owned. `userdel serviceloop` (no `-r`, unchanged) still doesn't touch directory contents on purge, so real downloaded model data survives a purge today exactly as it already would have under the old path - no new deletion behavior added. Confirmed fixed, not just reasoned about, by building the real `.deb`, installing it inside a disposable Debian container running real systemd (the same `--systemd=always`-plus-baked-in-systemd-image technique the original packaging PR used), and running a transient systemd unit as `serviceloop` with `ProtectHome=true` in effect: it could write to the new path and got `Permission denied` on the old one, reproducing and closing the bug in the same pass. Reinstalling on top of the existing account confirmed the fix is idempotent and leaves prior model data in place | A `/var/lib/sparky-agent/models` FHS-conventional location (considered; rejected in favor of consolidating under the already-established `/opt/sparky` tree instead of introducing a second top-level path); dropping `ProtectHome=true` and switching to `useradd --create-home` (considered; rejected as weakening the unit's hardening for no reason, when sidestepping the directive entirely was available instead) |
| 2026-08-14 | `deploy/systemd/sparky-agent.service` gained `KillMode=mixed` | Found on the RTX 4090 laptop during real-hardware validation of `agent/runtime/baremetal`, not by inspection this time: loading a real `llamacpp` profile then `systemctl stop sparky-agent` (skipping an explicit unload first) produced a C++ `terminate()`/abort crash in `llama-server`'s own shutdown path, instead of the clean exit the same backend's explicit-unload path already produced minutes earlier. Root cause confirmed via `journalctl`, not guessed: the unit's default `KillMode` (`control-group`, since nothing set it explicitly) makes systemd send `SIGTERM` to every process in the cgroup on stop - so the tracked `llama-server` child got one `SIGTERM` from systemd directly and a second one moments later from `agent/runtime/baremetal.Backend.Shutdown`'s own per-child signaling, and llama.cpp's own signal handler treats a second interrupt arriving mid-graceful-shutdown as a request to abort immediately rather than finish cleanly (its own log line said exactly that: "Received second interrupt, terminating immediately"). `mixed` sends the stop signal to the main process only, leaving the agent's own SIGTERM-then-grace-period-then-SIGKILL logic (already built, already unit-tested) as the sole signal path during a normal stop; systemd's cgroup-wide `SIGKILL` after `TimeoutStopSec` still applies as a backstop if the agent process itself hangs, so this loses no safety net. Confirmed fixed by reproducing the exact same load-then-abrupt-stop sequence against the rebuilt, reinstalled package on the same real hardware: clean "cleaning up before exit" with no crash, same as the explicit-unload path | `KillMode=process` (considered; rejected in favor of `mixed` since `process` drops the cgroup-wide `SIGKILL` backstop entirely rather than just reordering when it applies, trading away real defense-in-depth for no additional benefit here); leaving the double-signal as-is on the theory that it's cosmetic (rejected - an engine crashing on stop, even if Sparky's own bookkeeping is unaffected, is a real operator-visible symptom worth not shipping when the fix is one well-understood line) |
| 2026-08-15 | Engine software provisioning on bare-metal nodes (as distinct from model-weight downloads, which already exist) will be maintainer-provided artifacts distributed like models are, split into two v0.2.0/v0.3.0 work items by engine shape, rather than either agent-side source builds or leaving it fully manual forever | Prompted directly by today's own manual llama.cpp deployment during hardware validation - real toil (a hardcoded, non-relocatable `RUNPATH` needing an `LD_LIBRARY_PATH` workaround; copying a binary and its `.so` files past a `0700` home directory into `serviceloop`'s reach by hand). Agent-side source builds were considered and rejected first: compiling on the node itself means per-node toolchain/CUDA-version variability, real build time, and running arbitrary fetched source with no controlled artifact - a much bigger trust and maintenance surface than anything else this codebase takes on (nothing else here compiles third-party code at runtime). Compiled engines (llama.cpp) get prebuilt x86_64/arm64 release tarballs hosted on GitHub Releases to start (matching this project's own already-established "raw artifacts now, dedicated repo later if popularity warrants it" precedent from agent packaging - see the earlier 2026-08-13 entry), pulled by the agent directly over HTTPS the same shape as today's Hugging Face model downloads - no new central-server-side hosting needed. Python-based engines (vLLM, and Aphrodite once its adapter exists) don't get a frozen venv tarball - PyTorch/CUDA wheels are large and version-fragile enough that a frozen snapshot would be as brittle as the problem it's solving - instead a clone-and-provision flow: clone the engine's repo pinned to an exact commit SHA resolved from an operator-chosen tag/release/branch (never a floating "latest", which would let fleet nodes silently drift onto different, untested builds over time - the same reproducibility reasoning `model_ref`'s own "ideally with a pinned revision" convention already reflects), then `pip install` its `requirements.txt` into a venv under the engine's own directory. The two tracks are recorded as separate work items in different milestones specifically because their dependencies differ: the compiled-engine track has no blockers and continues directly from the now-complete bare-metal backend (v0.2.0); the Python-engine track's Aphrodite half is already gated behind the Aphrodite adapter (v0.3.0), so splitting avoids bundling a currently-blocked half in with an immediately-actionable one | Leaving engine installation fully manual indefinitely (rejected: real, confirmed toil, and this app's whole premise is self-service control over inference infrastructure - leaving engine provisioning as the one manual SSH-required step undercuts that); a single work item covering both tracks in one milestone (rejected: would either block the compiled-engine track on the Aphrodite adapter it doesn't actually depend on, or misrepresent the Python-engine track as unblocked when its Aphrodite half isn't) |
| 2026-08-15 | Implemented the compiled-engine provisioning item above with four concrete design choices settled during the build, not just the earlier same-day design entry: (1) activation via a symlink, not a config rewrite; (2) Admin/SuperAdmin-only RBAC; (3) mandatory SHA256 verification; (4) versioned installs that deliberately coexist rather than overwrite | (1) The freshly-provisioned binary becomes "the" one the bare-metal backend launches via an atomically-repointed `latest` symlink under `SPARKY_ENGINE_INSTALL_PATH/<engine_type>/`, not by having the agent (which already owns `secrets.env` at 0600 as `serviceloop`, so it technically could) rewrite `SPARKY_LLAMACPP_BINARY_PATH` in place - rewriting a config file the operator otherwise fully authors was judged to cut against this codebase's existing "config changes require a restart, no hot-reload" philosophy, and the symlink approach needs zero code changes in `agent/config`/`agent/runtime/baremetal` at all. (2) `internal/engineprovision.Service.ProvisionEngine` is gated by `rbac.CanManageNodes`, confirmed with the user over the alternative of reusing `manage_model_store` (the model-transfer capability) - this is node-level infrastructure provisioning (what software runs on a shared host), a different trust boundary from "manage my own model files," so no PowerDev-override path exists for it, matching `CanManageNodes`' own no-override precedent for node registration. (3) Confirmed with the user that these tarballs are maintainer-built specifically for Sparky (not raw upstream llama.cpp releases), so a sibling `.sha256` file is guaranteed to exist and is verified unconditionally before extraction - deliberately inconsistent with the Hugging Face Transfer Executor's own zero-checksum precedent, a difference recorded explicitly rather than glossed over, since the two sources have genuinely different trust/control characteristics. (4) `node_engine_inventory` is composite-keyed on `(node_id, engine_type, version)`, not `(node_id, engine_type)` the way `node_model_inventory` is keyed on `(node_id, model_ref)` - raised directly by the user mid-design: they want to eventually pin two otherwise-identical Model profiles to two different installed engine versions and compare outputs/timings/tuning side by side, which requires provisioning a new version to never delete an older one. That pinning mechanism itself is out of scope here (see the new deferred v0.2.0 follow-up item above) but the on-disk layout (versioned directories plus a `latest` symlink) and this schema key were built specifically so that follow-up needs no rework of what shipped today. Extraction shells out to the system `tar -xJf` via a fakeable `runner` seam (mirroring `agent/provision`'s own idiom) rather than adding a Go xz-decompression dependency - `compress/*` has no xz support, and CLAUDE.md requires discussing any new Go module dependency first, which a zero-dependency shell-out avoids needing to raise at all | Rewriting `secrets.env` directly from the agent process (rejected - see (1)); reusing `manage_model_store`/PowerDev-override RBAC (rejected - see (2)); skipping checksum verification to match the Hugging Face precedent (rejected - see (3)); a single-version-per-engine inventory key with old versions deleted on reprovision (rejected - see (4), would foreclose the version-pinning follow-up this was explicitly designed to support); a pure-Go xz library (rejected - see CLAUDE.md's dependency-approval rule, avoided entirely by shelling out instead) |
| 2026-08-15 | Implemented the deferred per-profile engine version pinning follow-up with two confirmed choices: a plain free-text form field rather than a dropdown populated from `node_engine_inventory`; no create/update-time validation that a pin actually exists on the target node | The provisioning work above deliberately left this for a separate pass once versioned installs existed to pin against. Two design questions were resolved directly with the user before implementing: whether the profile form should offer a live-populated dropdown of a node's actually-installed versions (an htmx-driven partial endpoint) versus a plain optional text input - the user chose the plain input, matching this item's own stated scope (schema plus launch-time resolution only) and the existing `required_memory_gb` optional-field pattern, deferring a picker UX rather than expanding scope for it now; and whether a pin should be validated against `node_engine_inventory` at save time versus accepted as-is and left to fail at launch - the user chose launch-time-only, matching `required_memory_gb`'s own SCHEMA.md-documented "attempt the launch and report failure" philosophy and avoiding a new coupling between `internal/profiles` and inventory state that no other profile field has (and that could go stale anyway, since a version can be removed after a profile is saved). The actual launch-time resolution reuses the *filename* of the already-configured `SPARKY_<ENGINE>_BINARY_PATH` (which already points through `latest`) under the pinned version's own directory instead, so no new per-version agent configuration was needed - an unpinned profile's resolution is byte-for-byte unchanged from before this feature | A dropdown populated from `node_engine_inventory` via a new htmx endpoint (rejected - see above, a real scope increase not currently justified); validating a pin against `node_engine_inventory` at profile save time (rejected - see above, new coupling plus staleness risk, inconsistent with `required_memory_gb`'s own precedent); sending an already-resolved absolute path from the central app instead of just the version string (rejected - the central app doesn't reliably know the binary's filename within a version's directory, only the node-local `SPARKY_<ENGINE>_BINARY_PATH` config does, so resolution has to stay agent-side, consistent with `LoadInstance`'s existing doc comment that host-local binary layout "is not something the central app can know") |
| 2026-08-16 | Added CSRF protection to every state-changing endpoint (`/login`, `/login/break-glass`, `/logout`, `/nodes/register`, `/profiles/new`, `/profiles/{id}/edit`, `/profiles/{id}/load`, `/instances/{id}/unload`, `/users/{id}/tier`), closing the app-wide Known Issues gap on record since Dashboard UI Phase 2 - hand-rolled double-submit-cookie token, wired explicitly per route | Confirmed with the user before implementing: hand-rolled (new `internal/httpapi/csrf.go`, mirroring `setup_gate.go`/`breakglass_ip_whitelist.go`'s shape) over a third-party library like `gorilla/csrf` - matches `internal/session`'s own "own it, don't add a dependency" precedent, and CLAUDE.md requires discussing any new Go module dependency first anyway; and explicit per-route wiring (`r.With(a.RequireSession, a.RequireCSRF)`, added to each of the nine write routes' existing `r.With(...)` chain) over an implicit global middleware, matching how `RequireSession` and the break-glass IP whitelist are already wired today rather than introducing a new implicit-global pattern this router doesn't otherwise use. Double-submit cookie (not an HMAC token embedded in the session payload) specifically because `internal/session` is fully stateless - no server-side session ID to hang a per-session CSRF secret off of - and two of the nine routes (`/login`, `/login/break-glass`) run before any session exists at all, so a double-submit cookie is the one mechanism that covers both pre-auth and authenticated writes uniformly. `RequireCSRF` skips enforcement entirely for a non-form-encoded request (reusing the existing `isFormRequest` helper `/login`/`/login/break-glass`'s dual-mode branching already established) - a JSON API caller can't be triggered by a naive cross-site HTML form in the first place (a plain form can't set `Content-Type: application/json` without JavaScript and a CORS preflight, itself a standard mitigation), so there's nothing for a forged submission to exploit there; confirmed via the vendored htmx.min.js's own source that htmx sets `Content-Type: application/x-www-form-urlencoded` by default on every non-GET request, so this same skip-check does *not* accidentally exempt the four htmx-driven writes (logout, tier-change, load, unload) the way it was designed to only exempt the two JSON branches. Token delivery splits by submission style: the four classic `<form method="post">` writes (login, break-glass login, node registration, profile create/edit) each get a hidden `csrf_token` input; the four htmx `hx-post` writes all render inside `base.html`'s shell, so a single `hx-headers` attribute on `<body>` covers all four at once rather than needing per-form wiring - confirmed via a real compiled-binary smoke test (a real GET-then-POST round trip against a running `sparky-server`, real Postgres) that the token flows correctly cookie-to-hidden-field and cookie-to-header, and that a missing or mismatched token is rejected with `403 CSRF_INVALID` while a correct one succeeds, for both submission styles | `gorilla/csrf` or a similar library (rejected - see above, new dependency inconsistent with existing precedent); an HMAC token embedded in the session payload (rejected - doesn't cover the two pre-session write routes without a second, separate mechanism); implicit global CSRF middleware skipping only GET/HEAD/OPTIONS (rejected - deviates from this router's existing explicit-per-route wiring convention, the same trade-off already accepted for `RequireSession` itself) |
| 2026-08-16 | Fixed `running_instances` staying stale at `status = running` after an ungraceful agent stop - a synthesis of the two options the Known Issues row itself named ("an agent-side 'report what's actually running on reconnect' handshake or a server-side liveness sweep"), not a choice between them: the server decides *when* to check (a fresh agent connection), the agent answers the *specific* question asked using state it already tracks for its own purposes | `agent/runtime.Backend` gained `IsRunning(ctx, instanceID) (bool, error)`; `containers.Backend`'s already-existing-but-unused `IsRunning` was changed from a raw `containerID` parameter to `instanceID` (resolving `InstanceContainerName` internally, matching `Stop`'s own pattern - a safe signature change since nothing called it yet); `baremetal.Backend.IsRunning` is new, answering from its own `processes` map via a non-blocking check of the tracked process's `done` channel (closed means the reaper already observed the process exit, even though only `Stop`/`Shutdown` ever removed the map entry before now - also fixed as a directly-motivated side effect: an already-exited entry is now cleaned up when `IsRunning` observes it, closing a small pre-existing leak nothing removed otherwise). This design works correctly for both backends without any special-casing: a container survives an agent restart by design, so `IsRunning` querying the daemon directly correctly confirms it's still running even after the agent process itself restarted; a bare-metal process's tracking is pure in-memory state scoped to one agent process's lifetime, so a genuine crash-and-restart wiping the map is itself the correct signal - "not tracked" honestly means "not as far as I can tell," even if the old OS process technically lingers as an unmanaged orphan (an already-accepted, separate gap). One new protocol message, `TypeCheckInstance`/`CheckInstance{InstanceID}` - deliberately no new response type, since the agent answers via the existing `TypeInstanceResult`, which `HandleInstanceResult` already maps to `SetStatus` correctly with no changes needed there at all. `internal/agentconn.Handler` gained `OnConnectFunc`, mirroring the existing `OnMessageFunc` exactly (same genericity - the package has no idea what reconciliation is, only that a connection just became usable), fired in its own goroutine so it never delays the connection's transition into its read loop. New `internal/lifecycle.Service.ReconcileNode`, wired as that hook: queries the new `ListRunningByNode` (deliberately plural and scoped to exactly `status = 'running'`, distinct from the existing singular `FindActiveByNode` `internal/metrics` already depends on for a different purpose, and deliberately not touching `starting`/`stopping` rows either, a different already-tracked Known Issues gap this pass leaves alone) and dispatches one `check_instance` per row found. An `IsRunning` error (e.g. a transient Docker daemon hiccup) deliberately reports nothing back rather than guessing - "I don't know" and "it's stopped" are different things, and a transient infrastructure error must not falsely stop-mark a row that might still be fine. Verified with real `go test -race` coverage throughout, plus a full real-binary end-to-end pass (no GPU hardware needed) standing in for real-hardware validation: a real compiled `sparky-agent` (bare-metal backend, a harmless long-running placeholder in place of a real engine) loaded through a real compiled `sparky-server` and real local Postgres, hard-killed with `SIGKILL` to simulate a crash (confirmed the DB still said `running` immediately after, reproducing the bug), then restarted - confirmed the row flipped to `stopped` (with `stopped_at` set) within about a second of reconnecting, with zero operator action taken | An agent-side self-reported "here's everything I have running" list sent unprompted on every reconnect (rejected - this is closer to the literal "agent-side handshake" framing the Known Issues row itself was skeptical of by citing the 2026-08-12 entry's concern about the agent becoming a second source of truth for what's running, duplicating bookkeeping the database already owns; asking a targeted, per-instance question instead avoids that); a pure server-side timer-based sweep independent of connection events (rejected - reconnection is the one moment a real answer becomes obtainable at all, a periodic sweep would either fire uselessly while offline or still need to wait for reconnection anyway); marking every row `failed` unconditionally the moment a node's connection drops (rejected - a mere network blip doesn't mean anything actually stopped, especially for the containers backend where the whole point is that a container outlives the agent; the fix only ever asks a real question once reconnected, never guesses eagerly on disconnect) |

---

## Known Issues and Technical Debt

| Issue | Severity | Deferred Because |
|-------|----------|-------------------|
| Mid-session behavior when a user loses AD access-group membership is undefined | Low | Not a stated priority during auth design; existing session likely persists until natural expiry |
| CDI GPU passthrough via Podman's Docker-Engine-API-compatible socket not yet verified on target hardware/Podman version - neither Docker API mechanism for requesting a CDI device (`HostConfig.DeviceRequests` with `Driver: "cdi"`, or `HostConfig.Devices` with a CDI name as `PathOnHost`) triggered CDI resolution against a real local Podman 4.9.3 daemon, even though Podman's own CLI resolves CDI names correctly - see 2026-08-10 Decisions Log entry for the full empirical finding | Medium | Requires the actual target Podman version (likely much newer than the 4.9.3 available here) and real GPU hardware to determine whether this is fixed upstream or still needs a workaround; `agent/runtime/containers` implements the documented, correct Docker API contract regardless, which is right for Docker and the best available attempt for Podman |
| `agent_status` never reaches `unreachable` - `internal/agentconn` only ever sets `online` (on a successful handshake) or `offline` (on any disconnect, clean or not), even though SCHEMA.md's third state exists for a connection that's still technically open but has gone silent | Low | `agent/connection.Conn` sends a `heartbeat` envelope every 30s as of Phase 5 (agent runtime/WebSocket work), but `internal/agentconn` doesn't read-deadline or otherwise track them yet - the server-side timeout/detection logic to actually consume those heartbeats and flip a stale connection to `unreachable` still doesn't exist. Revisit when there's a concrete reason to distinguish `unreachable` from `offline` operationally |
| Whether vLLM's CLI accepts `engines.LaunchSpec.Args`' flag-only form (`--model`/`--port`/etc, no subcommand) as built, or needs a `vllm serve <model>` subcommand form instead, is unverified - no real vLLM install was attempted during the 2026-08-14 RTX-4090-laptop bare-metal validation pass (deliberately deferred, confirmed with the user, to keep that pass scoped). llama.cpp's own half of this question is now closed: confirmed directly against a real `llama-server --help` and a real successful load that `--model`/`--port`/`--host`/`--gpu-layers`/`--ctx-size`/`--threads` are exactly what's emitted | Medium | Requires a real vLLM install (or at minimum its documentation for the specific pinned version this project targets) to confirm either way |
| `rbac.Service.ElevateTier`'s tier update and its audit-log write are two separate calls, not one database transaction - a tier change can persist while its audit record fails to write (surfaced to the caller as an error after the fact, but not rolled back) | Low | No cross-repository transaction pattern exists anywhere else in the codebase yet to extend; the failure mode requires the audit Postgres write itself to fail immediately after a successful update, which is rare enough not to block this pass |
| `agent/connection`'s `resolveModelPath` requires exactly one `.gguf` file on local storage for a partial-offload (llama.cpp-style) load, erroring otherwise - but v0.1.0's downloader fetches every file in a GGUF repo's default revision (2026-08-11 Decisions Log entry), which is commonly several quantizations at once. Loading a profile whose model is a multi-quantization GGUF repo fails until only one `.gguf` file remains on disk | Medium | No quantization selector exists anywhere in the pipeline yet (`model_ref` has no way to name one) - the same gap the 2026-08-11 Model transfers entry already flagged and deferred pending exactly this feature (Running instances) existing. Revisit by either parsing a `repo:QUANT` suffix out of `model_ref` (llama.cpp's own convention) or downloading only the selected quantization in the first place |
| `internal/lifecycle.Service.LoadInstance`/`UnloadInstance` persist their `running_instances` row (or its `stopping` transition) before dispatching to the agent - a dispatch failure after that point leaves the row in `starting`/`stopping` with nothing to move it forward, same known limitation already accepted for `rbac.Service.ElevateTier` and `internal/nodes.Service.RegisterNode` | Low | No cross-repository transaction or saga pattern exists anywhere in the codebase to extend; the failure mode requires the WebSocket send itself to fail immediately after a successful DB write, and the operator-visible symptom (a stuck `starting` row) is easy to diagnose manually until this is worth solving generally |
| `agent/telemetry.Collector`'s `nvidia-smi` integration (CSV parsing, multi-GPU aggregation) is unverified against a real `nvidia-smi` binary or real GPU hardware - this dev environment has neither (same gap already on record for CDI GPU passthrough). The CSV query shape (`--query-gpu=utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits`) is well-documented, stable `nvidia-smi` behavior, not guessed, but has never actually been run here | Medium | Requires real GPU hardware with `nvidia-smi` installed to close, same blocker already tracked for CDI verification; the parsing logic itself is unit-tested against a fake command runner exercising realistic CSV shapes (single GPU, multiple GPUs, malformed lines), so the gap is specifically "does the real binary's output match the documented format," not "is the parser correct for the format it's given" |
| Dashboard UI's pages (Phase 1, Phase 2's login page, Phase 3's Transfers page) were verified via `curl` against the real compiled binary, not a real browser - no display exists in this sandboxed environment (PLANNING.md Decisions Log, confirmed with the user rather than assumed). CSS layout, responsive behavior, and real-DOM htmx behavior are all unverified beyond what raw markup inspection can confirm. Phase 7's Metrics chart carries the same gap in a more consequential form: the server-generated chart series JSON and Chart.js/metrics.js's correct loading were confirmed structurally (curl, correct byte counts and content), but whether `initMetricsChart` actually renders a correct, readable chart in a real browser - and whether htmx's script-execution behavior holds exactly as read from source for this page's own script ordering - has never been visually confirmed | Medium | No headless-browser tooling is available in this environment either; closing this requires either running the binary and checking it in a real browser outside this sandbox, or provisioning browser automation tooling here - neither happened this pass |
| No rate limiting exists on any authentication endpoint (`/login`, `/login/break-glass`), despite CLAUDE.md Security Considerations calling for it | Medium | Same app-wide, pre-existing category as the CSRF gap above. Implementing it means either a new Go module dependency (CLAUDE.md: never added without discussion first) or hand-rolled in-memory/store-backed logic - a real design decision on its own, not attempted this pass |
| `RequireSession` still responds with its existing JSON 401 for an unauthenticated request to `/dashboard`/`/nodes`/`/profiles`, not a redirect to the now-real `/login` page | Low | An htmx partial (`HX-Request`) fetch that hit a redirect would have the redirect followed transparently by the browser's `fetch`, landing `login.html`'s full standalone document inside `#main-content` - broken markup nesting. Needs to distinguish a full-page navigation from an htmx partial fetch before redirecting only the former; deferred rather than solved with an untested guess |
| The sidebar's `Audit log`, `Users & permissions`, and `Settings` links are shown to every authenticated viewer regardless of tier, same as every other nav link - a non-Admin who clicks any of them gets a raw JSON 403 (`handleAuditLog`/`handleUsers`/`handleSettings`'s error response), not a friendlier page or a hidden link | Low | Same root cause as the row above: the shared `pageData`/`render()` path every page uses has no notion of the viewer's tier today, only whether a session exists at all - conditionally hiding these nav links by tier would mean threading a tier lookup into every page's render call (an extra DB round trip each time), not just these three pages' own. Consistent with the already-accepted JSON-401-instead-of-redirect gap directly above, extended to a second gate (403) rather than left as a worse, inconsistent special case. Phases 5 and 6 (Users & permissions, Settings) each hit the identical gap Phase 4 (Audit log) already documented here rather than opening further near-duplicate rows |
| `agent/enginetransfer`'s download/checksum-verify/extract/symlink-swap flow is verified only against an `httptest.Server` and a faked `tar` shell-out (gzip standing in for xz in tests, since Go's stdlib has no xz support) - it has never downloaded a real GitHub Release asset or extracted a real `.tar.xz` on real hardware, since no engine release tarball has actually been published to the main Sparky repo's Releases yet | Medium | Requires a first real maintainer-built `llamacpp-<version>-<arch>.tar.xz` (+ `.sha256`) to actually exist as a published release before an end-to-end pass is possible - same "logic and tests first, real artifact later" gap as `internal/engines`' unverified-against-a-live-install vLLM adapter. `runCommand` itself (the real, non-faked shell-out) is exercised against the real system `tar` binary (`TestRunCommand_RealTarBinary`), so only the specific `-xJf`/`.xz` combination and a real GitHub download are unverified, not the shell-out mechanism itself |
| `internal/httpapi`'s Model profiles create/edit HTTP handlers (`handleCreateProfile`/`handleUpdateProfile`) have no test coverage of their own - noticed while adding the `engine_version` form field, not introduced by it. The two pure functions those handlers delegate to (`fieldsFromForm`, `profileFormValuesFromProfile`) do have unit tests as of that change, but the handlers themselves (form parsing, RBAC/session handling, redirect-on-success, error-redisplay-on-failure) have never been exercised by any automated test, for any field | Low | Pre-existing gap spanning the whole file, not specific to `engine_version` - closing it properly means building the same kind of fake-`profileEditor`-plus-session-harness test infrastructure other handler packages don't yet have either (see the `httpapi`-wide CSRF/rate-limiting gaps above), a real scope increase beyond what adding one more optional field justified doing unilaterally |

---

## Dependencies and Blockers

- Budget approval for a 4-node, 200GB/s switch is pending - blocks true 4-node
  clustering, but 2-3 node direct-cable clustering is unblocked and can proceed.
- CDI/Podman GPU passthrough verification is what's actually blocking the v0.1.0
  "Agent: Docker/Podman runtime backend" item's top-level checkbox, not v0.4.0
  (Historical metrics, an unrelated milestone - this line originally misstated
  that). The laptop (RTX 4090) and Dell Precision (RTX 3080Ti) are both already in
  hand and in active use as this project's primary dev/test hardware. As of
  2026-08-13, the laptop is understood to be a permanent bare-metal case, not a
  pending CDI fix - its GPU is already claimed by its own host OS session and
  can't be passed through to a container at all, independent of drivers or CDI
  configuration (see this date's Decisions Log entry). What (if anything) still
  validates the Docker/Podman backend's CDI passthrough for this checkbox is an
  open question, not asserted either way - the Dell Precision's own
  GPU-passthrough situation (headless/dedicated vs. also host-session-bound) is
  unconfirmed, and it's similarly unconfirmed whether the "separate laptop GPU"
  the 2026-08-11 Decisions Log entry describes (nvidia drivers not loaded, a
  different blocker than the RTX 4090's) refers to the same machine or a third
  one - this environment's own dev sandbox has no GPU at all either way - see
  the CDI Known Issues row.
- Real DGX Spark hardware is needed for v0.2.0's Docker/Podman-backend CDI
  validation (not, as previously stated here, the bare-metal backend - see the
  2026-08-13 Decisions Log entry) - separate from the CDI/Podman blocker above,
  which concerns hardware already in hand.
- The bare-metal runtime backend's real-hardware validation blocker is
  resolved as of 2026-08-14 - done directly against the RTX 4090 laptop,
  see that date's Decisions Log entries and the milestone item's own
  writeup above. The only piece of it still open is the vLLM
  CLI-argument-shape question, tracked in Known Issues, not blocked on
  hardware access (a real vLLM install was simply not attempted this
  pass, by choice).

---

## Future Ideas

- Native Vault sidecar/CSI integration in the Helm chart, if a concrete user need
  emerges (deliberately deferred - see Decisions Log)
- Additional engine adapters beyond vLLM/Aphrodite/llama.cpp as the ecosystem evolves
- Cross-node comparison views in the historical metrics dashboard
