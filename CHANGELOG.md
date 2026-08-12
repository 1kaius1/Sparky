# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Go module and repository skeleton: `cmd/sparky-server` and `cmd/sparky-agent`
  entry points that load and validate their environment configuration and fail
  fast on startup, per ARCHITECTURE.md Application Lifecycle. No routes or
  agent protocol handling yet.
- `.gitignore`
- Database layer: `internal/db` connection pool (pgx/v5), verified with a
  ping before `sparky-server` proceeds past startup, per ARCHITECTURE.md
  Application Lifecycle. First migration (`000001_create_users`) creates the
  `users` table per SCHEMA.md.
- Documented the `golang-migrate` CLI install step (CLAUDE.md, CONTRIBUTING.md)
  now that `migrations/` actually exists and Database Setup/Migrations depend
  on it.
- `sparky-server` now actually loads `.env` on startup (`godotenv`), so
  CLAUDE.md's documented `cp .env.example .env` workflow works as written. A
  missing `.env` is treated as expected (production never has one); a
  present-but-malformed one logs a non-fatal warning. `sparky-agent` is
  unaffected - its local dev convention is a secrets file, not `.env`.
- CLAUDE.md: a disposable-Postgres-via-podman recipe in Database Setup, and a
  warning against `apt install python3-migrate` - an unrelated package Debian/
  Ubuntu's command-not-found hook suggests for the same `migrate` command
  name.
- `internal/db`'s `UserRepository`: create, find-by-AD-SID, update last
  login, and update tier (elevation), matching the `users` table per
  SCHEMA.md. Covered by integration tests against a real Postgres instance,
  per ARCHITECTURE.md Testing Strategy - they skip cleanly if `DATABASE_URL`
  is unset rather than failing.
- `internal/auth`: the `IdentityProvider` interface and its on-prem AD
  implementation (`LDAPProvider`, `go-ldap/v3`) - service-account bind, user
  search by `sAMAccountName`, and login-gate group membership resolution via
  `LDAP_MATCHING_RULE_IN_CHAIN`, per ARCHITECTURE.md Auth & Identity
  Provider. Password verification uses a dedicated connection so a user's
  own directory permissions never replace the service account's for the
  group-membership lookup, and explicitly guards against LDAP's
  unauthenticated-bind pitfall (a bind with a valid DN and an empty password
  otherwise succeeds without checking the password). No HTTP or session
  wiring yet. Unit-tested against a fake LDAP connection.
- `internal/session`: hand-rolled HMAC-SHA256 signed cookie sessions (no
  server-side store), matching the "own it, don't add dependencies you
  don't need" reasoning already on record for `chi`.
- `internal/httpapi`: the `chi` router, `POST /login` and `POST /logout`
  handlers, and `RequireSession` middleware for future protected routes.
  `LoginService` wires `internal/auth`'s identity provider and
  `internal/db`'s Users repository together - it enforces the login-gate
  group check (`AuthenticatedUser.InAccessGroup`), provisions first-time
  users at `TierReadOnly`, and updates `last_login_at` on repeat logins.
  `sparky-server` now runs a real HTTP listener with graceful shutdown on
  SIGTERM/SIGINT, completing ARCHITECTURE.md's Application Lifecycle up
  through routes and the listener (the Setup Check step is still not
  implemented). Verified with a real `httptest` browser round trip
  (cookiejar, actual `Set-Cookie` parsing) against a fake identity
  provider, and the compiled binary was smoke-tested end-to-end against a
  real Postgres instance.
- Migrations `000002_create_permission_overrides` and
  `000003_create_break_glass_credential`, with `internal/db`'s
  `PermissionOverrideRepository` (grant/get/revoke the `manage_model_store`
  capability) and `BreakGlassRepository` (get/set the isolated break-glass
  credential row). The break-glass table can hold at most one row by
  construction - a boolean primary key fixed to `true` plus a matching
  CHECK constraint, verified empirically by attempting a second insert and
  confirming Postgres rejects it. This is Phase A of the "Users, RBAC
  tiers, SuperAdmin break-glass" work (PLANNING.md v0.1.0); no RBAC logic
  or SuperAdmin login yet, just the storage layer. Covered by integration
  tests against a real Postgres instance, same pattern as the Users
  repository.
- `.gitignore`: `.claude/settings.local.json`, so contributors without a
  personal global git ignore for it (Claude Code's own local-override
  settings file) can't accidentally commit it.
- `internal/rbac`: `CanElevate` and `CanManageModelStore`, pure functions
  implementing SCHEMA.md Users' Elevation rules and the
  `manage_model_store` permission override, plus `Service.ElevateTier` as
  the single sanctioned path to a tier change - it fetches the target's
  current tier fresh, checks the rule, and only then persists via
  `UserRepository.UpdateTier`. This is Phase B of the "Users, RBAC tiers,
  SuperAdmin break-glass" work; no SuperAdmin login yet. `CanElevate` is
  exhaustively table-tested across all 16 tier-pair combinations.
- SuperAdmin break-glass login - Phase C, and the last phase, of "Users,
  RBAC tiers, SuperAdmin break-glass". `sparky-server set-superadmin-password`
  is a new interactive CLI subcommand (no terminal echo, via
  `golang.org/x/term`) that hashes the password with a hand-rolled Argon2id
  wrapper (`internal/auth`) and stores it via `BreakGlassRepository`.
  `POST /login/break-glass` is a separate endpoint from the regular AD
  login, not a special case inside it, since the SuperAdmin is not an
  LDAP/AD identity. `internal/session` and `internal/httpapi`'s
  `RequireSession` now carry `IsSuperAdmin` alongside `UserID`, matching
  `internal/rbac.Actor`'s existing shape. Verified as a genuine end-to-end
  flow: the interactive CLI ran through a real pseudo-terminal (required,
  since `x/term`'s no-echo behavior doesn't work over a plain pipe), the
  resulting hash was confirmed in Postgres, and the compiled server was
  then started for real and logged into against that exact stored
  credential over HTTP.
- `sparky setup` CLI first-run wizard (`cmd/sparky-server/setup.go`), and
  `internal/httpapi`'s `setupGate` middleware that enforces it: every
  route responds 503 `SETUP_REQUIRED` until the break-glass password has
  been set. The gate checks the database only until it first observes
  setup complete, then caches that forever - a `sparky-server setup` run
  in another terminal takes effect on the running server's very next
  request, no restart needed, which was verified live against a real
  running server, not just asserted. `setup` shares its prompt/hash/store
  logic with `set-superadmin-password`, adds a database-readiness check
  (a clear `migrate ... up` hint if the schema isn't there), and is safely
  re-runnable.
- Audit log: migration `000004_create_audit_log` creates the `audit_log`
  table per SCHEMA.md, `internal/db.AuditRepository` is its append-only
  writer, and `internal/audit.Recorder` is the single sanctioned path to
  it - it writes the authoritative Postgres record, then additionally
  emits a structured JSON line to a configured stream (stdout in
  production), per ARCHITECTURE.md Audit Log's always-on, shipper-friendly
  stream. Wired into `internal/rbac.Service.ElevateTier` (`elevated_user`),
  covering both a regular Admin actor and the SuperAdmin -
  `is_superadmin_action` and a nil `actor_id` for the latter, since the
  SuperAdmin is not a `Users` row - in the same code path, so there is no
  separate branch that could accidentally skip the audit write.
  `audit_settings` (retention, syslog/GELF forwarding) and the generic
  HTTP audit middleware ARCHITECTURE.md describes are deferred - see
  PLANNING.md Decisions Log 2026-08-10. Covered by integration tests
  against a real Postgres instance (including that the migration's `down`
  reverses cleanly), same pattern as the Users and Permission overrides
  repositories.
- Node registry: migration `000005_create_nodes` creates the `nodes`
  table per SCHEMA.md - `node_type`, `container_runtime`, and
  `agent_status` as Postgres enums, `gpu_memory_gb`/`cpu_memory_gb` split
  from the start, and a `CHECK` constraint enforcing that
  `container_runtime` is set if and only if `node_type = docker-gpu`.
  `fabric_group_id` is not part of this migration - it references
  `fabric_groups`, which doesn't exist until v0.3.0 - and `registered_by`
  is nullable, since the break-glass SuperAdmin can register a node too
  and is not a `Users` row. `internal/db.NodeRepository` covers Create,
  FindByID, and List. `rbac.CanManageNodes` (Admin or SuperAdmin only, no
  permission-override path) and `internal/nodes.Service.RegisterNode` are
  the single sanctioned path to a new node - RBAC check, parameter
  validation, persist, then audit (`registered_node`, the audit log's
  second real caller after `elevated_user`). No HTTP handler yet, same
  precedent as RBAC Phase B and the audit log itself. Covered by
  integration tests against a real Postgres instance, including that the
  `CHECK` constraint rejects a mismatched `node_type`/`container_runtime`
  pair and that the migration's `down` reverses cleanly.
- `agent/runtime/containers`: the Docker/Podman runtime backend - Phase 1
  of the agent runtime/WebSocket work. `Backend.StartContainer`/
  `StopContainer`/`IsRunning` via `github.com/moby/moby/client` (the
  upstream project renamed from `docker/docker`), including pull-if-missing
  (the raw Engine API does not auto-pull like the `docker run` CLI does)
  and CDI GPU device requests. Non-GPU container lifecycle fully verified
  against a real local Podman daemon - create, pull, start, inspect, stop,
  remove. CDI GPU passthrough itself has a documented open gap: neither
  Docker API mechanism for requesting a CDI device triggered CDI
  resolution through Podman 4.9.3's compat socket in testing, even though
  Podman's own CLI resolves CDI names correctly - see PLANNING.md's
  2026-08-10 Decisions Log entry and Known Issues table. Retry/CDI-request
  construction logic unit-tested against a fake client.
- `internal/agentproto`: the shared WebSocket/JSON protocol message types
  used by both binaries - Phase 2 of the agent runtime/WebSocket work.
  `Envelope{Type, RequestID, Payload}` per ARCHITECTURE.md Protocol's
  request-ID correlation field, plus `Hello`/`HelloAck`/`Heartbeat`/
  `ErrorPayload` payload types and `NewEnvelope`/`DecodePayload` helpers.
  `DecodePayload` rejects unknown JSON fields, so decoding a payload with
  the wrong Go type for its `Envelope.Type` fails loudly rather than
  silently zero-filling fields that don't overlap. Pure types - no
  networking, bearer-token enforcement, or connection lifecycle yet;
  covered entirely by marshal/unmarshal round-trip tests.
- Node bearer token issuance and storage - Phase 3 of the agent runtime/
  WebSocket work. Migration `000006_add_node_bearer_token` adds
  `nodes.bearer_token_hash`. `internal/auth`'s `GenerateNodeToken` makes a
  `spk_`-prefixed 256-bit random token; `HashNodeToken`/`VerifyNodeToken`
  use plain SHA-256, not Argon2id like the break-glass credential -
  deliberately, since a memory-hard KDF only helps against a low-entropy
  human-chosen secret, not an already-random 256-bit token, and would add
  needless latency to every agent reconnect. `nodes.Service.RegisterNode`
  now returns the plaintext token once, alongside the created node; only
  its hash is ever persisted, and that hash is excluded from `Node`/
  `FindByID`/`List` so it can't leak through the future Nodes dashboard.
  Migration verified up and down against a real local Postgres instance.
- `internal/agentconn`: the server-side Agent-Communication Layer - Phase 4
  of the agent runtime/WebSocket work, ARCHITECTURE.md's "only component
  that speaks the agent protocol." `Handler`, mounted at `GET
  /agent/connect`, uses `github.com/coder/websocket` to accept the
  connection and run the hello/auth handshake (`internal/agentproto`)
  against new `internal/nodes.AuthService` - the first real caller of Phase
  3's stored bearer token hash (new `db.NodeCredential`/
  `FindCredentialByName`/`SetAgentStatus`). A rejected handshake (unknown
  node name or wrong token) gets the same generic reason either way, so it
  can't be used to enumerate node names. The success ack is sent only after
  the node is marked `online` in `agent_status` and added to `Registry`
  (which just tracks live connections for now - no command dispatch yet),
  so the agent can never observe acceptance before the central app's own
  state reflects it. `agent_status` only ever reaches `online`/`offline` for
  now, not `unreachable` - that needs a heartbeat timeout with nothing to
  time out yet, since the agent doesn't send heartbeats until Phase 5; see
  PLANNING.md Known Issues. Verified end-to-end through the real compiled
  `internal/httpapi` router with a real Postgres-issued token and a real
  WebSocket client, in addition to the committed fake-based unit tests.
- `agent/connection`: the agent-side Connection goroutine - Phase 5, and
  the last phase, of the agent runtime/WebSocket work. `Conn.Run` dials
  `SPARKY_CENTRAL_URL`, presents `SPARKY_BEARER_TOKEN`/`SPARKY_NODE_NAME`
  in the same hello/auth handshake `internal/agentconn` expects, and
  reconnects with exponential backoff (1s-30s, equal jitter, reset after
  any accepted handshake) on disconnect - per docs/AGENT.md Service
  Architecture Notes. Also sends a `heartbeat` envelope every 30s over an
  established connection. `agent/runtime/containers.Backend` (Phase 1) is
  wired in as a field, ready for a real command type to dispatch to once
  Model profiles/Running instances define one - `dispatch` today only
  recognizes `heartbeat`/`error`, logging anything else, since no command
  message type exists yet. `cmd/sparky-agent` now actually starts the
  connection loop on `SIGTERM`/`SIGINT`-cancellable context instead of
  just validating config and exiting. Verified end-to-end (ad hoc, not
  committed) with the real compiled `sparky-server`/`sparky-agent`
  binaries over a real TCP socket: connect, `agent_status` -> `online`,
  `SIGTERM` -> clean shutdown -> `offline`, and a second run with the
  server killed mid-connection showing real backoff growth and a
  successful reconnect once the server came back.
- Model profiles schema + `internal/db.ProfileRepository` - Phase 1 of
  the Model profiles work (PLANNING.md). Migration
  `000007_create_model_profiles` matches SCHEMA.md's Model profiles,
  single-node only for v0.1.0: `topology` is declared with both enum
  values up front, but a `model_profiles_single_node_only` CHECK
  constraint is what actually enforces the scope (rejects `clustered`
  or a null `target_node_id`) - `fabric_group_id` itself isn't part of
  this migration at all, same reasoning as `nodes.fabric_group_id`.
  `ProfileRepository` covers Create/FindByID/List/Update/Delete;
  `engine_params` is `jsonb NOT NULL DEFAULT '{}'::jsonb`. Verified up
  and down against a real local Postgres instance, plus integration
  tests confirming the CHECK constraint rejects both violations via a
  direct `INSERT`, not just that the Go API happens to never construct
  one.
- `internal/engines` - Phase 2 of the Model profiles work. The `Adapter`
  interface and a `Registry` mapping `db.ProfileEngineType` to it, plus
  vLLM and llama.cpp-style adapters (Aphrodite is v0.3.0 scope). An
  adapter reports its engine's fixed `requires_full_gpu_residency` and
  validates the handful of `engine_params` keys Sparky recognizes when
  present, passing everything else through unvalidated - `engine_params`
  stays deliberately opaque beyond that, per SCHEMA.md Model profiles.
  llama.cpp's recognized keys (`n_gpu_layers`, `ctx_size`, `threads`)
  were confirmed against a real `llama-server --help`, not guessed;
  vLLM's reflect well-established, stable CLI arguments but weren't
  verified against a live install (impractical here - vLLM's CUDA/torch
  dependency chain). Pure validation logic, no networking or container
  calls - no database dependency either, unlike most of this project's
  other packages.
- `internal/profiles.Service` - Phase 3, and the last phase, of the
  Model profiles work: RBAC-gated (new `rbac.CanManageProfiles` -
  PowerDev, Admin, or SuperAdmin) `CreateProfile`/`UpdateProfile`/
  `DeleteProfile`. `RequiresFullGPUResidency` is deliberately absent
  from the input params - it's derived from the resolved engine adapter
  (Phase 2), not caller-supplied, so it can never disagree with the
  chosen `EngineType`. Validates `engine_params` through the adapter
  registry and confirms `target_node_id` refers to a real registered
  node (`internal/nodes`) before persisting (Phase 1) and auditing
  every create/update/delete. No HTTP handler yet, same precedent as
  the node registry. All three Model profiles phases are now done -
  the top-level checklist item is checked off in PLANNING.md.
- `model_transfers` and `node_model_inventory` schema and repositories -
  Phase 1 of the Model transfers work. Migrations `000008`/`000009` per
  SCHEMA.md Model transfers and Node model inventory; `source_type`/
  `source_node_id` get the same CHECK pairing as `nodes.container_runtime`/
  `node_type`, enforced now even though nothing produces `peer_node` yet
  (v0.3.0 rsync work). `node_model_inventory` has no separate `id` -
  `(node_id, model_ref)` is the composite primary key, and
  `NodeModelInventoryRepository.Upsert` replaces the existing row rather
  than inserting a new one per transfer, the same `ON CONFLICT` pattern as
  `PermissionOverrideRepository.Grant`. `ModelTransferRepository` covers
  `Create`, `FindByID`, `UpdateProgress`, and `SetStatus` (which stamps
  `completed_at` only for the three terminal statuses). Covered by
  integration tests against a real Postgres instance, including both
  directions of the CHECK constraint violation and a verified up/down/up
  migration cycle.
- Model transfers Phase 2: protocol extension and dispatch capability.
  `internal/agentproto` gains `TypeStartTransfer` (central -> agent) and
  `TypeTransferProgress` (agent -> central) message types - the latter's
  `Status` field is a plain string, not `db.TransferStatus`, since this
  package has no dependency on `internal/db`. `internal/agentconn.Registry`
  gains its first real send capability, `Send(ctx, nodeID, envelope)
  error`, returning the new `ErrNodeNotConnected` when the target node has
  no live connection; it only holds its mutex for the map lookup, not the
  network write, since `coder/websocket` supports concurrent writes on one
  connection. `Handler` gains a pluggable `OnMessageFunc` callback (a new
  required `NewHandler` parameter, nil-able) for message types it doesn't
  handle internally - `hello`/`hello_ack`/`heartbeat` stay silently
  internal, `error` is logged, everything else is forwarded. `readLoop` now
  actually decodes every message instead of discarding it unread.
  `cmd/sparky-server` currently passes `nil` for `onMessage` - nothing
  dispatches a real command until Phase 3 exists. Pure types/plumbing,
  covered by unit tests using real `coder/websocket` connections, no actual
  download involved.
- `agent/transfer.Executor` - Model transfers Phase 3, the agent-side
  Transfer Executor. Downloads a Hugging Face model repository over plain
  `net/http` - no `huggingface_hub`/Python, no `git`/`git-lfs` on the
  agent host, see PLANNING.md's Decisions Log. Lists a repo's files via
  `GET /api/models/{repo}`, HEAD-requests each file's size up front so
  `BytesTotal` is accurate from the first progress report, then downloads
  each via `GET /{repo}/resolve/{revision}/{file}`. A file already present
  at its full remote size is skipped; a partial file is resumed via a
  `Range` request when the server honors it - a `200` response to a
  resumed request (some small, non-LFS Hugging Face files silently ignore
  `Range` despite advertising `Accept-Ranges: bytes`, confirmed against a
  real repo) is treated as "start this file over," never appended to
  blindly. Progress is throttled by a byte threshold (4 MiB default), not
  wall-clock time. Wired into `agent/connection.Conn`'s dispatch on
  `TypeStartTransfer`: one goroutine per active transfer, pushing
  `TypeTransferProgress` back over the connection via Phase 2's plumbing.
  `Conn` gained a `sync.WaitGroup` around transfer goroutines and `Run`
  now waits on it before returning, so a graceful shutdown lets an
  in-flight transfer reach a safe stopping point instead of killing it
  mid-write - the behavior docs/AGENT.md already documented but nothing
  built until now. 9 new unit tests in `agent/transfer` (full download,
  periodic progress, real resume, a server that ignores `Range`,
  skip-if-complete, list/download error handling, a canceled context, a
  nested file path) plus 2 new tests in `agent/connection`. Manually
  verified end to end against the real Hugging Face Hub
  (`hf-internal-testing/tiny-random-bert`): a full download landed
  byte-correct files, a re-run skipped every file, and resuming a
  truncated file produced output `md5sum`-identical to a fresh
  independent download.
- `internal/transfers.Service` - Model transfers Phase 4, the last of the
  four, completing the milestone item. RBAC-gated with the existing
  `rbac.CanManageModelStore`; `Service.canManageModelStore` resolves its
  `hasOverride` argument itself, only querying
  `PermissionOverrideRepository.Get` for a PowerDev actor (Admin/SuperAdmin
  already have the capability implicitly, and no other tier can have it
  regardless). `InitiateTransfer` checks `agentconn.Registry.Connected` and
  returns the new `ErrDestNodeOffline` before creating the `model_transfers`
  row, so an unreachable destination never leaves behind a queued transfer
  nothing will pick up; only then does it persist (`queued`,
  `TransferSourceInternet` - v0.1.0 has no peer-replication source yet) and
  dispatch `TypeStartTransfer`. `HandleTransferProgress` is `Service`'s
  `agentconn.OnMessageFunc` for `TypeTransferProgress` - trusts the
  connection's own authenticated `nodeID` rather than a value out of the
  transfer row, updates `bytes_transferred`/`status`, and upserts
  `node_model_inventory` (`present`) once status is `completed`, looking up
  the transfer's `model_ref` via `FindByID` since the wire payload doesn't
  carry it. As an `OnMessageFunc` with no return value, failures are logged
  through a `*log.Logger` dependency, the same shape as `agentconn.Handler`'s
  own. No HTTP handler yet, same precedent as the node registry and Model
  profiles. 17 unit tests against fakes, covering RBAC denial (including the
  PowerDev-with/without-override split), an offline destination node, and
  the happy path through to a completed transfer; `go test -race` clean.

### Changed
- `internal/db`'s `UserRepository.UpdateTier` now takes a nullable
  `elevatedBy *string` instead of a required `string`, since a
  SuperAdmin-made change has no `Users` row to reference and
  `elevated_by` must be able to store `NULL`. Added `FindByID` for
  `internal/rbac`'s use. No other callers existed yet to migrate.
- `internal/httpapi.New` now also takes a `*BreakGlassLoginService`
  parameter. `UserIDFromContext` is replaced by `IdentityFromContext`,
  returning an `Identity{UserID, IsSuperAdmin}` instead of a bare string -
  no other callers existed yet to migrate.
- `internal/httpapi.New` takes a fourth parameter, the break-glass store
  the setup gate checks - no other callers existed yet to migrate.
- `internal/httpapi.New` takes a fifth parameter, an `http.Handler` for
  `internal/agentconn`'s WebSocket endpoint, mounted at `GET
  /agent/connect` via chi's `Method` (not `Get`) so a nil handler - as in
  this package's own login/logout tests, which never exercise that route -
  doesn't panic building the router at all.
- `CLAUDE.md` fully absorbs `.clauderules`' content and now `@import`s
  `ARCHITECTURE.md`, `SCHEMA.md`, and `docs/AGENT.md` directly, so project
  conventions and behavioral rules are guaranteed present in context every
  session rather than depending on a prose pointer to a second file being
  acted on - see PLANNING.md's 2026-08-11 Decisions Log entry for the full
  reasoning and the tradeoffs knowingly accepted (`CLAUDE.md` now exceeds
  Claude Code's own ~200-line size guidance; `PLANNING.md` stays
  unimported).
- `PLANNING.md`'s Current Status, Current Sprint / Active Work, Open
  Questions, and Dependencies and Blockers sections corrected for
  staleness - several described pre-implementation state, referenced the
  wrong milestone version, or duplicated an entry now tracked in Known
  Issues and Technical Debt.

### Deprecated

### Removed
- `.clauderules` - fully merged into `CLAUDE.md`. Its two real remaining
  references (`docs/AGENT.md`, a code comment in
  `internal/auth/ldap.go`) now point at `CLAUDE.md` instead.

### Fixed
- Commits and pull requests going forward no longer include AI-attribution
  text (a `Co-Authored-By` trailer, a "Generated with" footer) - this
  violated `.clauderules`' explicit prohibition throughout the session,
  because `.clauderules` was never actually being read: `CLAUDE.md` only
  mentioned it in prose, and nothing auto-loads an arbitrary filename.
  Already-merged history is not rewritten - see PLANNING.md's Decisions
  Log for why.

### Security

---

## [0.1.0] - YYYY-MM-DD

### Added
- Initial release

[Unreleased]: https://github.com/1kaius1/Sparky/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/1kaius1/Sparky/releases/tag/v0.1.0
