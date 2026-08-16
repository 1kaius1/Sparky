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
- Running instances: single-node load/unload, completing that v0.1.0
  milestone item - the Model Lifecycle Orchestrator (ARCHITECTURE.md
  Component Breakdown). Migration `000010_create_running_instances` and
  `internal/db.RunningInstanceRepository` (Create, FindByID,
  FindActiveByProfileID, SetStatus - COALESCE-preserves `actual_port`
  across a status transition with nothing new to report).
  `internal/engines.Adapter` gained `BuildLaunchSpec(params)
  (LaunchSpec, error)`, translating each engine's recognized
  `engine_params` into an image and command-line flags (`--gpu-layers`/
  `--ctx-size`/`--threads` for llama.cpp; `--tensor-parallel-size`/
  `--gpu-memory-utilization`/`--dtype`/`--quantization`/`--max-model-len`
  for vLLM) - deliberately excludes `--model`/`--port`, which only the
  agent (local storage layout) and the profile itself (a fixed value, not
  engine-specific) can supply. `agentproto` gained
  `TypeLoadInstance`/`TypeUnloadInstance` (central to agent) and
  `TypeInstanceResult` (agent to central). `agent/connection.Conn.dispatch`
  gained matching cases (a new `instanceWG`, separate from `transferWG`),
  `resolveModelPath` (the model's whole directory for a full-GPU-residency
  engine, a single `.gguf` file for a partial-offload one - see
  PLANNING.md's Known Issues for the multi-quantization gap this creates),
  and `sendInstanceResult`. `agent/runtime/containers.Spec` gained
  `Cmd`/`Port`/`Mounts`, wired into `container.Config`/`HostConfig` -
  `Port` publishes the identical port number on host and container;
  `Mounts` bind-mounts the agent's model storage root read-only at the
  same path inside the container, so a resolved `--model` path needs no
  translation. `InstanceContainerName` (`sparky-instance-{id}`) is a new
  deterministic container-naming helper, letting `UnloadInstance`'s wire
  payload carry only an instance ID rather than the central app tracking
  a live container ID of its own. `rbac.CanLaunchInstances` (Developer,
  PowerDev, Admin, or SuperAdmin - CLAUDE.md's already-documented
  "Developer launch" sidebar tier) guards both load and unload, the same
  one-function-covers-both shape as `CanManageModelStore`.
  `internal/lifecycle.Service` ties it together: `LoadInstance` refuses a
  second concurrent load for the same profile (`ErrAlreadyRunning`) and an
  offline target node (`ErrTargetNodeOffline`) before persisting anything,
  resolves the launch spec, creates the `running_instances` row,
  dispatches, and audits (`loaded_model` - SCHEMA.md's own audit log
  example); `UnloadInstance` requires the instance to currently be
  `running` (`ErrInstanceNotRunning`), transitions it to `stopping`,
  dispatches, and audits (`unloaded_model`); `HandleInstanceResult` is the
  `agentconn.OnMessageFunc` that updates `running_instances` from whatever
  the agent reports. No `running_instance_nodes` table, Green/Blue/Red
  eligibility, or reduced-capacity launch handling - all v0.3.0 clustering
  scope. No HTTP handler yet, same precedent as the node registry, Model
  profiles, and Model transfers. Verified with real integration tests
  against Postgres (including a migration up/down/up cycle) and unit
  tests against fakes across every touched package; `go test -race`
  clean.
- Metrics: live telemetry collection, completing that v0.1.0 milestone
  item (ingestion only - "and dashboard" in the item's name is the
  eventual consumer, built separately by the still-unstarted Dashboard UI
  item; retention/downsample/export are the separate v0.4.0 Historical
  metrics milestone). New `agent/telemetry.Collector` (ARCHITECTURE.md's
  Telemetry Collector component) reads `nvidia-smi` (aggregated across
  however many GPUs it reports - averaged utilization, summed memory,
  unverified against real GPU hardware, an honest gap logged in
  PLANNING.md's Known Issues) and `/proc/stat`/`/proc/meminfo` directly
  (confirmed against this dev machine's real files). CPU utilization is a
  stateful delta between successive `Read` calls - the first reading
  after agent startup is always 0, with nothing yet to diff against.
  `agentproto` gained `TypeTelemetry` (agent to central, unprompted) and
  its `Telemetry` payload, `RecordedAt` trusted from the agent the same
  way `Heartbeat.SentAt` already is. Migration `000011_create_metrics`
  creates the `metrics` table per SCHEMA.md - no separate `id` column,
  `(node_id, recorded_at)` is the composite primary key.
  `internal/db.MetricsRepository` is write-only for now, same precedent
  as `AuditRepository`. `RunningInstanceRepository` gained
  `FindActiveByNode` (status = `running` specifically) so ingestion can
  resolve `running_instance_id` server-side, from the connection's own
  authenticated node identity - the agent is never asked to track its own
  Running-instance state. `agent/connection.Conn` gained a telemetry
  goroutine (own ticker, `Config.TelemetryPollInterval` -
  `SPARKY_TELEMETRY_POLL_INTERVAL`, parsed and validated fail-fast by
  `cmd/sparky-agent`) alongside the existing heartbeat goroutine; a
  zero/negative interval is guarded against inside `sendTelemetry` itself
  (logs and disables telemetry for that connection) since
  `time.NewTicker` panics on one and an unrecovered goroutine panic would
  take the whole agent process down over what should only disable one
  feature. `internal/metrics.Service.HandleTelemetry` is the
  `agentconn.OnMessageFunc` that persists a reading - unlike
  `internal/transfers.Service`/`internal/lifecycle.Service`, no RBAC
  check or audit record, since a telemetry push is agent-initiated
  observational data, not a human actor's state-changing action. No HTTP
  handler yet, same precedent as every other v0.1.0 service so far.
  Verified with real integration tests against Postgres (including a
  migration up/down/up cycle) and unit tests against fakes/real `/proc`
  fixtures across every touched package; `go test -race` clean.
- Dashboard UI Phase 1: base layout/sidebar shell, session-gated routing,
  and three working read-only pages (Dashboard overview, Nodes, Model
  profiles) - the first real frontend code in the repo, and the first
  HTTP wiring for `internal/nodes.Service`/`internal/profiles.Service`/
  `internal/lifecycle.Service`. New `web` package (`web.FS`,
  `//go:embed templates static`) holds the templates and static assets
  (htmx 2.0.10 vendored, plain CSS - CLAUDE.md Tech Stack). Each page is
  parsed together with the base layout into its own isolated
  `*template.Template` (`internal/httpapi.loadPageTemplates`) rather than
  one combined `ParseGlob`, since every page defines a block also named
  `"content"` and a shared template set would make the last-parsed page
  silently win for all of them. `render` serves the full document or just
  the `"content"` block depending on htmx's own `HX-Request` header - one
  template serves both a full page load and an
  `hx-get`/`hx-target="#main-content"` partial swap. `internal/nodes.Service`,
  `internal/profiles.Service`, and `internal/lifecycle.Service` each
  gained an unguarded `List*` method (viewing is available at the lowest
  RBAC tier and is never audited); `httpapi.New` now also takes
  `nodeLister`/`profileLister`/`instanceLister` and a `*log.Logger`, and
  returns `(*API, error)` - a template parse failure is now a
  startup-time failure, not a broken page on first request.
  `RunningInstanceRepository` gained `List` for the Dashboard's fleet
  summary. `RequireSession` (defined during the AD/LDAP auth work, unused
  until now) gates the three new routes; `/` redirects to `/dashboard`,
  `/static/*` stays public. No write/action forms and no HTML login page
  yet - both are Dashboard UI Phase 2, along with the remaining five
  sidebar sections (Transfers, Metrics, Users & permissions, Audit log,
  Settings). Verified with unit tests against fakes for every new
  handler, and a genuine end-to-end pass against the actual compiled
  `sparky-server` binary: real Postgres, a real `set-superadmin-password`
  run over a real pty, a real `POST /login/break-glass`, then
  `curl`+cookiejar against every new route, an `HX-Request: true` partial
  fetch, both static assets, and an unauthenticated request confirming
  the existing JSON 401 - not just asserted, actually run. No visual
  browser check was possible in this sandboxed environment (no display) -
  confirmed with the user as the verification approach for this phase.
- Dashboard UI Phase 2: a real HTML login page (`GET /login`,
  `web/templates/pages/login.html`) and a logout control in the sidebar.
  `POST /login` now serves both the existing JSON API contract and the
  login page's own `application/x-www-form-urlencoded` submission,
  branching on `Content-Type` (`isFormRequest`/`handleLoginFormSubmit` in
  the new `login_page.go`) - ordinary content negotiation, not a second
  endpoint, since it's the same identity source and login flow as the
  JSON contract, just two response formats. A failed submission
  re-renders the login page in place with an error message (no
  flash-message mechanism exists to carry one across a redirect); a
  successful one redirects to `/dashboard`. `GET /login` redirects an
  already-authenticated request straight to `/dashboard`. `handleLogout`
  now sets `HX-Redirect: /login` so the sidebar's new logout control (an
  `hx-post` to `/logout`) drives a full client-side navigation back to
  the login page. `RequireSession`'s unauthenticated response is
  deliberately left as its existing JSON 401, not changed to redirect -
  an htmx partial fetch following a redirect would land the login page's
  full standalone document inside `#main-content`. Neither CSRF
  protection nor auth-endpoint rate limiting was added - both are
  pre-existing, app-wide gaps spanning every POST route already built,
  not specific to this page - see PLANNING.md Known Issues. Verified with
  8 new unit tests and a genuine end-to-end pass against the actual
  compiled `sparky-server` binary: the unauthenticated form, a failed
  submission against a real (dial-failing, no AD server in this
  environment) LDAP provider, a real break-glass session, the
  already-authenticated redirect, the sidebar's real logout markup,
  `POST /logout` clearing the cookie and setting `HX-Redirect`, and a
  post-logout request actually getting 401 again.
- Dashboard UI Phase 3: the Transfers sidebar section as a fourth read-only
  page, following Phase 1's exact pattern. `db.ModelTransferRepository`
  gains `List` (every transfer across every node, most recently requested
  first); `internal/transfers.Service` gains `ListTransfers`, unguarded by
  RBAC like every other `List*` method. New `internal/httpapi/transfers.go`
  resolves each transfer's `dest_node_id` to a node name the same way
  `handleModelProfiles` resolves `target_node_id`, and formats
  `bytes_transferred`/`bytes_total` as plain `"N.N MB / N.N MB"` text - no
  smart unit-scaling, matching `nodes.html`'s own minimal `{{.GPUMemoryGB}}
  GB` formatting. New CSS status classes for
  `queued`/`transferring`/`completed`/`cancelled`. `internal/transfers.Service`
  is now constructed in `cmd/sparky-server/main.go` for the first time -
  Model transfers Phase 4 built it but nothing had an HTTP-facing caller
  for it until this page. `HandleTransferProgress` is still not wired as
  `agentconn`'s `onMessage` callback - `ListTransfers` is a pure read path
  that doesn't need it; that consolidation stays a separate, explicitly
  deferred unit of work. Verified with new unit tests (dest-node-name
  resolution, empty state, list-failure handling, unauthenticated gating),
  a new `ModelTransferRepository.List` integration test against a real
  Postgres instance, and a genuine end-to-end pass against the actual
  compiled `sparky-server` binary with a real node/transfer row inserted
  directly via SQL (no write path exists yet to create one through the
  app itself).
- Dashboard UI Phase 4: the Audit log sidebar section as a fifth read-only
  page - the first whose sidebar tier floor is Admin, not Read-only, so
  the first needing a real RBAC check rather than just a session check.
  `rbac.CanViewAuditLog` (Admin/SuperAdmin only, no override path).
  `db.AuditRepository` gains `List` (most recently created first) and
  `db.UserRepository` gains `List` (ordered by display name) - the latter
  resolves both the viewer's own tier for the RBAC check and each audit
  record's `actor_id` to a display name. `audit.Recorder`'s internal
  `writer` dependency is renamed `store` and widened to cover both `Write`
  and the new `List(ctx, actor rbac.Actor)`, which checks
  `CanViewAuditLog` and returns `rbac.ErrNotPermitted` internally - the
  RBAC decision lives in the Service-layer method itself, not only at the
  HTTP layer, matching `transfers.Service.InitiateTransfer`'s own
  internal-check precedent. New `internal/httpapi/audit.go` resolves the
  viewer's tier via a new `actorFromIdentity` helper (the session cookie
  deliberately carries no tier) and maps `rbac.ErrNotPermitted` to a 403.
  The sidebar's `Audit log` link is shown to every authenticated viewer
  regardless of tier, same as every other nav link - a non-Admin who
  clicks it gets a JSON 403, not a hidden link or a friendlier page; see
  PLANNING.md Known Issues. Verified with new unit tests across
  `internal/rbac`, `internal/audit`, and `internal/httpapi` (permit/deny,
  actor-name resolution, empty state, list failure, forbidden,
  unauthenticated), new `AuditRepository.List`/`UserRepository.List`
  integration tests against a real Postgres instance, and a genuine
  end-to-end pass against the actual compiled `sparky-server` binary with
  a real Admin user, a real Developer (non-Admin) user, and a real audit
  record inserted directly via SQL.
- Dashboard UI Phase 5: the Users & permissions sidebar section as a sixth
  read-only page, same Admin sidebar tier floor as Audit log.
  `rbac.CanViewUsers` (Admin/SuperAdmin only, no override path, same shape
  as `CanViewAuditLog`). Unlike Audit log, this page exposes the full
  roster itself rather than resolving an already-permitted record's
  `actor_id`, so the RBAC check lives in `rbac.Service.ListUsers` instead
  of a new package - `rbac.Service` already wraps `UserRepository` via its
  `userStore` interface (widened with `List`) for `ElevateTier`, so this
  keeps every RBAC-gated `Users` action in one place. `rbac.Service` is
  constructed in `cmd/sparky-server/main.go` for the first time this
  phase, ahead of `ElevateTier` having any HTTP caller. New
  `internal/httpapi/users.go` reuses the existing `actorFromIdentity`
  helper and maps `rbac.ErrNotPermitted` to a 403; each row's
  `elevated_by` resolves to a display name via the same map-of-names
  pattern already used for Audit log's actor names. The sidebar's `Users &
  permissions` link is shown to every authenticated viewer regardless of
  tier, same known gap as the `Audit log` link - see PLANNING.md Known
  Issues. Verified with new unit tests across `internal/rbac` and
  `internal/httpapi` (permit/deny, elevated-by-name resolution, empty
  state, list failure, forbidden, unauthenticated, HX-Request partial),
  and a genuine end-to-end pass against the actual compiled
  `sparky-server` binary with a real Admin user, a real Developer
  (non-Admin) user with `elevated_by`/`elevated_at` set, and the
  break-glass SuperAdmin session, against a real Postgres instance.
- Dashboard UI Phase 6: the Settings sidebar section as a seventh
  read-only page, same Admin sidebar tier floor as Audit log and Users &
  permissions. Discovered along the way that the two singleton config
  tables SCHEMA.md already documented - `metrics_export_config` and
  `audit_settings` - had never actually been migrated; migrations
  000012/000013 create them, each seeded with exactly one default row at
  creation time (same `id boolean PRIMARY KEY DEFAULT true` plus
  `CHECK (id)` singleton pattern as `break_glass_credential`, but with an
  `INSERT` in the same migration rather than left absent - neither table
  has a real "not configured yet" state distinct from "configured to do
  nothing"). `audit_settings.retention_months` had no default decided
  anywhere before this phase; confirmed with the user as 12 months, a
  middle value between the Metrics table's own 6-month retention window
  and this column's 24-month ceiling, now enforced with a CHECK
  constraint. New `internal/db` repositories
  (`MetricsExportConfigRepository`, `AuditSettingsRepository`), each with
  a read-only `Get`. `rbac.CanViewSettings` (Admin/SuperAdmin only, no
  override path, same shape as `CanViewAuditLog`/`CanViewUsers`, but a
  distinct function - three capabilities that happen to share a tier
  floor today). New `internal/settings` package wraps both repositories
  behind one RBAC-gated `Service.Get(ctx, actor)` - neither
  `internal/metrics.Service` (scoped to telemetry ingestion only) nor
  `internal/audit.Recorder` (scoped to `audit_log`, not the separate
  `audit_settings` table) was the right home for this read. New
  `internal/httpapi/settings.go` reuses `actorFromIdentity` and resolves
  each row's `updated_by` via a single `FindByID` lookup, simpler than the
  Audit log/Users pages' list-and-map pattern since at most two IDs ever
  need resolving here. SCHEMA.md updated in the same change: both tables'
  `updated_by` notes corrected to nullable (the break-glass SuperAdmin
  case) and their seeded-default behavior documented. The sidebar's
  `Settings` link is shown to every authenticated viewer regardless of
  tier, same known gap as `Audit log`/`Users & permissions` - see
  PLANNING.md Known Issues. Verified with new unit tests across
  `internal/rbac`, `internal/settings`, and `internal/httpapi`
  (permit/deny, updated-by-name resolution, empty-state placeholders,
  forbidden, unauthenticated, HX-Request partial), new
  `MetricsExportConfigRepository`/`AuditSettingsRepository` integration
  tests confirming the migrations' seeded rows against a real Postgres
  instance (including a full down/up migration round-trip), and a genuine
  end-to-end pass against the actual compiled `sparky-server` binary with
  a real Admin user, a real Developer (non-Admin) user, the break-glass
  SuperAdmin, and both config rows updated directly via SQL to
  non-default values.
- Dashboard UI Phase 7: the Metrics sidebar section as the eighth and
  final sidebar page - every section now has a working read-only view.
  Unlike every page since Phase 4, this one's floor is Read-only, same as
  Dashboard/Nodes/Model profiles/Transfers - no RBAC check. `internal/db`'s
  `MetricsRepository` (previously write-only) gains `LatestByNode` (one
  most-recent row per node, via `DISTINCT ON`) and `Recent` (the 200 most
  recent rows across every node combined, most-recently-recorded first -
  a recent-window chart data source, not full historical retention, which
  stays the separate v0.4.0 Historical metrics milestone).
  `internal/metrics.Service` gains `ListLatestByNode`/`ListRecent`,
  unguarded by RBAC like `nodes.Service.ListNodes`, and is constructed in
  `cmd/sparky-server/main.go` for the first time - it existed as a type
  before this phase but nothing had ever instantiated it in production.
  Chart.js 4.4.4 (MIT license) vendored at
  `web/static/js/chart.umd.min.js` per ARCHITECTURE.md's no-CDN
  static-asset policy, loaded unconditionally in `base.html`'s `<head>`
  alongside a new small `web/static/js/metrics.js`
  (`initMetricsChart`) - loading both unconditionally, rather than only
  from inside the Metrics page's own content block, sidesteps depending
  on an unverifiable (no browser in this environment) assumption about
  htmx's execution order for dynamically swapped external `<script src>`
  tags. `internal/httpapi/metrics.go` builds the chart's series data as
  JSON server-side and embeds it via `template.JS` inside an inline
  `<script>` in `metrics.html`'s content block, relying on
  `encoding/json`'s default HTML-safe escaping for safe embedding.
  Verified with new unit tests across `internal/metrics` and
  `internal/httpapi` (node-name resolution in both the table and the
  embedded chart JSON, empty state, both list-failure paths,
  unauthenticated, HX-Request partial), new
  `MetricsRepository.LatestByNode`/`Recent` integration tests against a
  real Postgres instance, and a genuine end-to-end pass against the
  actual compiled `sparky-server` binary with a real Developer
  (non-Admin) user, two real nodes, and nine real metrics rows across a
  20-minute window inserted directly via SQL. The chart's actual visual
  rendering in a real browser remains unverified - see PLANNING.md Known
  Issues.
- Dashboard UI Phase 8: the Users & permissions tier-change form, the
  Dashboard UI's first write/action form. No changes needed in
  `internal/rbac` - `rbac.CanElevate` and `rbac.Service.ElevateTier` were
  both already fully built and tested before this milestone started; this
  phase only gave `ElevateTier` its first HTTP caller. New
  `internal/httpapi`: `userElevator` interface, satisfied by the same
  `*rbac.Service` value as `userRoster` (now passed twice into
  `httpapi.New`); `reachableTiers`, which derives the tier-change
  dropdown's offered options by calling `rbac.CanElevate` per candidate
  rather than a hand-written approximation, so the UI can never offer an
  option the server-side check would then refuse; `handleElevateUser`
  (`POST /users/{id}/tier`), validating the posted tier against the four
  known values before calling `ElevateTier`, and responding with
  `HX-Redirect: /users` on success - the same pattern `handleLogout`
  already established. `users.html` gained a per-row tier `<select>` +
  submit button, shown only when a row has at least one reachable tier
  (an Admin viewing another Admin's row, or their own, correctly gets no
  form). This is also the first new state-changing endpoint added since
  the app-wide CSRF gap was documented - deliberately left unprotected
  consistent with every other write route, rather than retrofitted for
  one endpoint in isolation; see PLANNING.md Known Issues, now corrected
  to accurately list which write routes actually exist. Verified with new
  unit tests (`reachableTiers`'s tier-matrix coverage mirroring
  `rbac.CanElevate`'s own exhaustive test, `handleElevateUser`'s
  success/forbidden/not-found/invalid-tier/missing-tier/generic-failure/
  unauthenticated paths) and a genuine end-to-end pass against the actual
  compiled `sparky-server` binary against a real Postgres instance - a
  real Admin elevating a real Developer to PowerDev (confirmed in the
  database and via a real `elevated_user` audit row), that Admin then
  correctly refused a skip-a-step promotion to Admin, a Developer session
  refused any elevation at all, an unknown target user ID getting 404, an
  invalid tier value getting 400, and finally the break-glass SuperAdmin
  session successfully performing the exact skip-a-step promotion the
  regular Admin had just been refused.
- Dashboard UI Phase 9: the node registration form, the Dashboard UI's
  second write/action form. No changes needed in `internal/nodes` -
  `nodes.Service.RegisterNode` and `RegisterNodeParams.validate()` were
  both already fully built and tested before this milestone started, same
  shape as Phase 8's `ElevateTier` discovery; this phase only gave
  `RegisterNode` its first HTTP caller. New `internal/httpapi`:
  `nodeRegistrar` interface, referencing `nodes.RegisterNodeParams`
  directly (the one interface in this package that can't avoid importing
  a domain package's exported type, since that struct is
  `RegisterNode`'s real parameter type); `handleRegisterNodeForm` (`GET
  /nodes/register`) and `handleRegisterNode` (`POST /nodes/register`).
  Unlike Phase 8's tier-change form, a successful submission here cannot
  use the blunt `HX-Redirect`-to-the-list pattern - `RegisterNode`'s
  plaintext bearer token is returned exactly once and would be lost on an
  immediate redirect - so success instead renders a dedicated
  confirmation page (`node_registered.html`) showing the token with an
  explicit one-time warning, and the whole flow uses a plain `<form
  method="post">` full-page navigation rather than an `hx-post` partial
  swap. A validation failure (`nodes.ErrInvalidNode`) re-renders the form
  with the specific reason and every previously-typed field preserved.
  The Nodes list page gained a `CanRegister`-gated "Register node" link,
  resolved the same non-security-boundary way as the Users page's
  per-row `ReachableTiers`. This is the second new state-changing
  endpoint added since the app-wide CSRF gap was documented - joins it
  the same way Phase 8's did, not retrofitted in isolation; PLANNING.md
  Known Issues extended accordingly. Verified with new unit tests
  (`CanRegister` shown/hidden by tier, the form's own 403/401 gating,
  successful registration with correct `RegisterNodeParams` construction
  including the spark-vs-docker-gpu `ContainerRuntime` nil/non-nil cases,
  forbidden, invalid-node re-display with preserved field values,
  non-numeric memory input rejected before the Service call,
  unauthenticated) and a genuine end-to-end pass against the actual
  compiled `sparky-server` binary against a real Postgres instance - a
  real Admin registering a real spark node (confirmed in the database:
  only `bearer_token_hash` persisted, never the plaintext; `registered_by`
  set to the Admin; a real `registered_node` audit row) with the real
  plaintext token rendered on the confirmation page, a Developer session
  correctly refused both the form and the submission, a missing-hostname
  submission correctly re-shown with its specific validation message, a
  docker-gpu submission missing `container_runtime` correctly refused
  with the matching message, and an unauthenticated request getting 401.
- Dashboard UI Phase 10: the model profile create/edit form, the
  Dashboard UI's third write/action form. `internal/profiles.Service`
  gains `GetProfile` (wrapping `ProfileRepository.FindByID`, already
  exported at the repository layer but not previously exposed through
  the Service), unguarded by RBAC like `ListProfiles`, backing the edit
  form's own prefill read; `CreateProfile`/`UpdateProfile` themselves
  needed no changes, same "already fully built and tested" shape as
  Phase 8/9's discoveries. New `internal/httpapi`: `profileEditor`
  interface, referencing `profiles.CreateParams`/`UpdateParams` directly
  (the second interface in this package, after `nodeRegistrar`, that
  can't avoid importing a domain package's exported type);
  `handleNewProfileForm`/`handleCreateProfile` and
  `handleEditProfileForm`/`handleUpdateProfile` share one template
  (`profile_form.html`, an `IsEdit` flag picking title/button/POST
  target) and one field-parsing helper. Unlike node registration, a
  successful submission has no one-time secret, so it uses a standard
  Post/Redirect/Get (`http.Redirect` to `/profiles`) rather than a
  dedicated confirmation page. A validation failure
  (`profiles.ErrInvalidProfile`, already carrying the specific
  engine-adapter-level reason) re-renders the form with that message and
  every previously-typed value preserved, including the raw
  `engine_params` JSON text. A blank `engine_params` submission defaults
  to `"{}"` before validation, since every `internal/engines` adapter
  treats an empty params object as valid. The engine-type dropdown
  includes `aphrodite` even though it has no registered adapter until
  v0.3.0 - selecting it fails through the same validation path as any
  other invalid submission rather than the UI maintaining a separate
  notion of availability. The Model profiles list page gained a
  `CanManage`-gated "New profile" link and per-row "Edit" links, resolved
  the same non-security-boundary way as the Nodes/Users pages' own gated
  affordances. This is the third new state-changing endpoint pair added
  since the app-wide CSRF gap was documented - joins it the same way
  Phases 8 and 9 did; PLANNING.md Known Issues extended accordingly.
  Verified with new unit tests (`GetProfile` found/not-found/
  store-failure, `CanManage` shown/hidden by tier, both forms' own
  403/401/404 gating, the edit form's prefill including a real JSON
  `engine_params` round-trip, successful create/update with correct
  params construction, forbidden, invalid-profile re-display with
  preserved field values, non-numeric port rejected before the Service
  call) and a genuine end-to-end pass against the actual compiled
  `sparky-server` binary against a real Postgres instance - a real
  PowerDev creating a real vLLM profile with real `engine_params`
  (confirmed in the database: `requires_full_gpu_residency = true`, the
  exact submitted params, a real `created_profile` audit row), the edit
  form correctly prefilling every field including the JSON textarea, a
  real edit changing the port and persisting `updated_by`, a Developer
  session correctly refused both the new-profile form and its
  submission, an adapter-rejected `engine_params` value correctly
  re-shown with vLLM's own specific validation message, an unknown
  profile ID on the edit form getting 404, and an unauthenticated
  request getting 401.
- Dashboard UI Phase 11, completing the milestone item: the OnMessage
  dispatcher consolidation, the instance load/unload controls (the fourth
  and last Dashboard UI write/action form this milestone scoped), and SSE
  wiring for live telemetry/transfer progress/instance results, per
  ARCHITECTURE.md's Server-Sent Events commitment. `cmd/sparky-server/main.go`
  no longer passes `nil` for `agentconn.NewHandler`'s `onMessage` -
  `internal/transfers.Service.HandleTransferProgress`,
  `internal/lifecycle.Service.HandleInstanceResult`, and
  `internal/metrics.Service.HandleTelemetry` were all already fully built
  and tested with no caller; a new dispatch closure switches on the
  envelope's own `Type` to route to the right one. Without this, a
  Load/Unload click left a `running_instances` row stuck in
  `starting`/`stopping` forever, since nothing processed the agent's
  response - fixed and verified against real binaries (see below), not
  just asserted.

  New `internal/events` package: an in-process, non-persisted
  publish/subscribe broker (`Broker.Publish`/`Subscribe`) - SSE per
  ARCHITECTURE.md is explicitly "internal use only" live signaling, not a
  durable log like the audit log. `Publish` drops rather than blocks for a
  subscriber whose buffer is full, so one slow or abandoned browser tab
  can never back-pressure the agent's own WebSocket read loop - the same
  "don't block the real work over a side channel" reasoning
  `agentconn.Registry.Send`'s own doc comment already documents for its
  mutex scope. The consolidated onMessage dispatch closure publishes an
  `events.Event{Type: ...}` after handling each of the three message
  types. New `internal/httpapi/events.go`: `GET /events`, session-gated
  like every other Dashboard UI page (no RBAC beyond that - the events
  carry nothing more than a type). New `web/static/js/sse.js`: a small
  vanilla-JS `EventSource` client (CLAUDE.md Frontend Conventions'
  "minimal vanilla JS - no framework" rule - no htmx SSE extension added,
  since that would be a new JS library beyond Chart.js's already-discussed
  carve-out) that triggers a debounced htmx refetch of whatever page is
  currently visible on a relevant event, rather than patching individual
  DOM nodes - confirmed with the user as this phase's intended scope, see
  PLANNING.md's Decisions Log.

  New `internal/httpapi/instances.go`: `instanceLauncher` interface over
  `lifecycle.Service.LoadInstance`/`UnloadInstance` - both were already
  fully built and tested before this phase, same "already built, just
  needed an HTTP caller" shape as Phases 8/9/10's own discoveries.
  `handleLoadInstance` (`POST /profiles/{id}/load`) and
  `handleUnloadInstance` (`POST /instances/{id}/unload`) - the RBAC
  decision (`rbac.CanLaunchInstances`) is not re-checked in the handler,
  it lives entirely inside the Service methods themselves, matching every
  other write path in this codebase; `rbac.ErrNotPermitted` -> 403,
  `db.ErrProfileNotFound`/`db.ErrRunningInstanceNotFound` -> 404,
  `lifecycle.ErrAlreadyRunning`/`ErrTargetNodeOffline`/`ErrInstanceNotRunning`
  -> 409. Both use the same `hx-post`/`hx-swap="none"`/`HX-Redirect`
  pattern `handleElevateUser` established - a one-click action with no
  fields to preserve on failure. The controls live on the **Model
  profiles** page, not a new sidebar section - matching CLAUDE.md Frontend
  Conventions' existing tier note for that section ("PowerDev create /
  Developer launch") directly. `internal/httpapi/model_profiles.go` now
  also lists running instances (`instances.ListInstances`, already
  unguarded) and folds them into a `profileID -> active instance` map,
  keeping only the three non-terminal statuses
  (`starting`/`running`/`stopping`) - a `stopped`/`failed` instance leaves
  its profile eligible to load again rather than showing a stale status.

  This is the fourth write route pair added since the app-wide CSRF gap
  was documented (PLANNING.md Known Issues) - joins it the same way
  Phases 8, 9, and 10 did, not retrofitted here; that Known Issues row's
  route list and its now-stale "Instance load/unload remains a
  Service-layer method only" note are both updated.

  Verified with new unit tests across `internal/events` (publish/
  subscribe/cancel, buffered-drop behavior) and `internal/httpapi`
  (`handleLoadInstance`/`handleUnloadInstance`'s RBAC/not-found/conflict/
  unauthenticated paths, `handleEvents` against a real `httptest.Server`
  and a real `http.Client` - a streaming handler and a concurrently-read
  response body over a plain `httptest.NewRecorder` would race on its
  unsynchronized `bytes.Buffer`, which a real connection doesn't have -
  confirming a real SSE-framed event is delivered and that a client
  disconnect actually releases the handler goroutine/subscription, not
  just that the route returns 200, and the Model profiles page's new
  status/Load/Unload rendering), and `go test -race` clean across every
  touched package. Also verified end to end against the real compiled
  `sparky-server`/`sparky-agent` binaries, a real Postgres instance, and a
  real local Podman daemon: registered a real node, connected a real
  agent, created a real profile, Loaded it through the real form, and
  confirmed the `running_instances` row actually left `starting` (a real
  agent error - no `.gguf` file present, an expected outcome in this
  sandbox with no real model on disk - transitioned it to `failed`,
  proving OnMessage consolidation processes the agent's response rather
  than leaving the row stuck) while a real `curl -N /events` connection
  concurrently received the matching `event: instance_result` SSE frame;
  a subsequent Unload attempt against that non-running instance correctly
  got a real 409 `NOT_RUNNING`, and the Model profiles page correctly
  re-offered Load once the instance reached its terminal `failed` state.
- A browser-usable break-glass (SuperAdmin) login page (`GET
  /login/break-glass`, `web/templates/pages/breakglass_login.html`, a
  single password field - no username, since the SuperAdmin is not an AD
  identity), gated by a new `breakGlassIPWhitelist` middleware
  (`internal/httpapi/breakglass_ip_whitelist.go`). `POST /login/break-glass`
  now serves both the existing JSON API contract and the new form's own
  submission, branching on `Content-Type` via the same `isFormRequest`/
  `handleBreakGlassLoginFormSubmit` pattern the regular login page already
  established. Both verbs sit behind the new middleware equally - the JSON
  API path is gated the same as the browser form, since both check the
  identical sensitive credential; `internal/config` gains an optional
  `BREAKGLASS_ALLOWED_IPS` (comma-separated IPs/CIDR ranges, parsed once at
  `httpapi.New()` construction time, empty means allow from anywhere, same
  off-by-default shape as `AUDIT_FORWARD_ENABLED`). Client IP is
  `r.RemoteAddr`, not `X-Forwarded-For` - see PLANNING.md's Decisions Log
  for both of these confirmed design choices. A rejection gets the existing
  JSON error envelope (403 `IP_NOT_ALLOWED`) regardless of caller type,
  plus a plain log line - no DB audit write, since login events remain
  outside `internal/audit`'s scope, unchanged from every prior login phase.
  No CSRF or rate limiting added - both are pre-existing, app-wide gaps
  affecting every form in this codebase equally, not specific to this page.
  Verified with new unit tests across `internal/config` and
  `internal/httpapi` (whitelist parsing/matching for a single IP, a CIDR
  range, IPv6, a malformed-`RemoteAddr` fallback, and empty-allows-all; the
  page's render/redirect/re-render paths; and that a wrong-password attempt
  from a non-whitelisted IP gets 403 `IP_NOT_ALLOWED` before the credential
  check ever runs, not 401).
- Bare-metal packaging for `sparky-agent` (v0.1.0's "Bare-metal install
  script" item) - agent-only, built via `nfpm` into a `.deb`, an `.rpm`, and
  a tarball with `install_agent.sh`, covering both `amd64` and `arm64`
  (`scripts/build_packages.sh`). Binary installs to
  `/opt/sparky/bin/sparky-agent` for all three methods, with a
  `/usr/local/bin/sparky-agent` convenience symlink since `/opt/sparky/bin`
  isn't on any distro's default `$PATH`. New `deploy/systemd/sparky-agent.service`
  and `deploy/secrets.env.template` - both long referenced by `docs/AGENT.md`
  but never actually committed until now. `scripts/packaging/lib/agent-common.sh`
  is the single implementation of the idempotent `serviceloop` account
  creation and `video`/`render` GPU-group detection, shared by all three
  install methods. Fresh installs are enabled but never auto-started (an
  unconfigured `secrets.env` would crash-loop); an already-running agent is
  safely restarted on upgrade. `apt remove`/`dnf remove` stop and disable
  the service and remove the binary but leave `serviceloop`/`secrets.env`
  in place; `apt purge` removes both. RPM has no native purge concept at
  all (confirmed, not assumed) - `scripts/packaging/purge_rpm.sh` is copied
  to `/usr/local/sbin/sparky-agent-purge.sh` just before removal so a real
  cleanup command survives on the box, and `docs/AGENT.md` documents
  running it by hand. No CI wiring and no signed package repository -
  both explicitly deferred. `ARCHITECTURE.md`'s Deployment Model and
  `README.md`'s Quick Start, which both implied a single unified installer
  for both binaries, are corrected in the same change - the central app has
  no packaged bare-metal installer at all. Verified via `nfpm`-built
  packages installed/upgraded/removed/purged inside disposable Debian and
  Rocky Linux podman containers running real systemd (`--systemd=always
  /sbin/init`) - install, start against a syntactically valid but
  unreachable `secrets.env` (confirmed the agent retries with backoff
  rather than crash-looping), upgrade a running install (confirmed restart
  and an untouched `secrets.env`), `remove`, and `purge`/`purge_rpm.sh`
  (confirmed full cleanup - this caught a real bug during development,
  where `purge_rpm.sh` was initially ordinary package content and got
  deleted by a plain `dnf remove` before it could ever run). Real GPU
  hardware, real Spark ARM64 execution, and true bare-metal PID 1 behavior
  remain unverified - a container's systemd is a reasonable proxy, not
  identical to real hardware.
- Agent bare-metal runtime backend (`agent/runtime/baremetal`) - direct
  process exec for hosts where GPU passthrough isn't viable, per
  ARCHITECTURE.md Runtime Backends. Introduced a shared `agent/runtime`
  package (`Spec`, `Backend` interface with `Start`/`Stop`/`Shutdown`) so
  `agent/connection` no longer branches on which concrete runtime backend
  it holds; the existing `agent/runtime/containers` backend was refactored
  onto this interface with no behavior change. `cmd/sparky-agent` now
  actually branches on `SPARKY_RUNTIME_BACKEND` (`docker`/`podman` ->
  containers, `bare-metal` -> the new backend), failing fast on an
  unrecognized value - previously loaded but never validated or dispatched
  on. `agentproto.LoadInstance` gained an `EngineType` field so the agent
  can resolve a local executable per engine type via two new optional env
  vars, `SPARKY_LLAMACPP_BINARY_PATH`/`SPARKY_VLLM_BINARY_PATH` (only
  `vllm`/`llamacpp` have adapters today) - a load for an engine type with
  no configured path fails clearly via the existing `InstanceResult`
  `status=failed` path. `SPARKY_MODEL_STORAGE_PATH`'s documented
  bare-metal default (`/opt/sparky/serviceloop/models`) is now actually
  applied in `agent/config.Load` when unset - `serviceloop`'s home
  directory moved there from `/home/serviceloop` (packaging's `useradd`
  gained `--home-dir`, plus a new idempotent directory-creation step) after
  real-hardware validation prep found the original path unreachable under
  the systemd unit's `ProtectHome=true`, confirmed via a disposable
  podman-plus-real-systemd install. On agent shutdown (SIGTERM/SIGINT),
  every process the bare-metal backend is still tracking is sent SIGTERM,
  given a grace period, then SIGKILLed if still running - closing the gap
  docs/AGENT.md's Signal Handling section previously described only as a
  statement of intent; the containers backend's own `Shutdown` stays a
  no-op, since a Running instance's container is deliberately left running
  across an agent restart. `deploy/systemd/sparky-agent.service` gained
  `KillMode=mixed` after real-hardware validation found the unit's default
  `control-group` mode double-signals a tracked bare-metal child (once from
  systemd directly, once from the agent's own shutdown logic), which made a
  real llama.cpp server abort instead of exiting cleanly on a second
  SIGTERM arriving mid-shutdown - `mixed` leaves the agent's own per-child
  signaling as the only path during a normal stop, confirmed by
  reproducing and then re-verifying the exact same scenario against the
  rebuilt package on real hardware. Verified via `go test -race` across every
  touched package, including new tests exercising real process lifecycle
  (SIGTERM, SIGKILL escalation, concurrent multi-process shutdown) against
  harmless real binaries - no GPU or real engine binary needed for that.
  Validated against this project's own RTX 4090 laptop: a real `llamacpp`
  profile loaded as an actual child of `sparky-agent` running as
  `serviceloop`, `nvidia-smi` attributing real GPU memory directly to that
  process, a real inference request against it succeeding, and unload/
  shutdown both exiting the child cleanly. Confirms an engine adapter's
  existing launch arguments are correct as a raw bare-metal command line
  for llama.cpp; vLLM's half of that question was deliberately not
  attempted this pass and remains open, tracked in PLANNING.md.
- `sparky-agent setup` subcommand (`agent/provision`) - creates/verifies the
  `serviceloop` system account, its model storage home directory, and
  GPU-passthrough group membership, replacing the equivalent bash in
  `scripts/packaging/lib/agent-common.sh` with a fakeable, unit-tested
  implementation (mirroring `agent/telemetry`'s `commandRunner` seam).
  Requires root, exits with a clear error otherwise. All three install
  methods now call it automatically after placing the binary; also safe to
  run by hand for diagnostics or to repair an already-provisioned node.
- Agent: compiled-engine binary provisioning from GitHub Releases
  (`agent/enginetransfer`, `internal/engineprovision`) - self-service
  download/install of a maintainer-built engine release (`llamacpp` today)
  onto a bare-metal node, mirroring the Transfer Executor's download/
  progress-reporting pattern. Downloads the release tarball and its sibling
  `.sha256` checksum from the main Sparky repo's own GitHub Releases,
  verifies it (mandatory here, unlike the Hugging Face downloader's own
  precedent, since these bundles are maintainer-built and always come with a
  checksum), and installs into a versioned directory under the new
  `SPARKY_ENGINE_INSTALL_PATH` (default `/opt/sparky/serviceloop/engines` on
  bare-metal), atomically repointing a `latest` symlink at each successful
  install - multiple versions deliberately coexist on disk rather than being
  overwritten, so a later profile-level version-pinning feature can select
  between them. Extraction shells out to the system `tar` (no new Go
  dependency). New `internal/agentproto` message types
  (`TypeStartEngineTransfer`/`TypeEngineTransferProgress`); new
  `engine_transfers`/`node_engine_inventory` tables and repositories
  (migrations `000015`/`000016`); new RBAC-gated (`CanManageNodes` -
  Admin/SuperAdmin only, no PowerDev-override path) `internal/engineprovision.
  Service`, mirroring `internal/transfers`' shape. `agent/connection` gained
  a dispatch case and its own `engineTransferWG`, separate from the existing
  `transferWG`. No HTTP handler or dashboard form yet - logic and agent-side
  mechanics only, matching this project's own repeated "logic before HTTP
  wiring" precedent. See PLANNING.md's 2026-08-15 Decisions Log entries for
  the full design.
- Per-profile engine version pinning - a new, optional `model_profiles.
  engine_version` column (migration `000017`) lets a Model profile pin its
  launch to a specific installed engine binary version instead of whatever
  a node's `latest` symlink currently points to, so two otherwise-identical
  profiles can each pin a different version for direct output/timing/tuning
  comparison. Threaded through `internal/db`, `internal/profiles`,
  `internal/lifecycle`, and a new `agentproto.LoadInstance.EngineVersion`
  field; resolved agent-side by the new
  `agent/connection.resolveEngineBinaryPath`, which reuses the filename of
  the already-configured `SPARKY_<ENGINE>_BINARY_PATH` under the pinned
  version's own directory - no new per-version agent configuration needed,
  and zero behavior change for any profile that leaves it unset. The
  profile create/edit form gained a plain optional text field next to
  engine type; a pin is not validated against `node_engine_inventory` at
  save time - a bad pin fails clearly at launch time instead, matching
  `required_memory_gb`'s own precedent. See PLANNING.md's per-profile
  engine version pinning Decisions Log entry for the full design.

### Changed
- `scripts/packaging/lib/agent-common.sh`, `scripts/packaging/postinstall.sh`,
  and `scripts/install_agent.sh`: account/group/model-storage-directory
  provisioning moved into `sparky-agent setup` (see above) - the shell
  library now only handles `secrets.env` materialization.
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
- Nodes' `node_type` (`spark` / `docker-gpu`) and `container_runtime`
  (`docker` / `podman`, CHECK-constraint-paired) collapsed into a single
  `runtime_backend` enum (`docker` / `podman` / `bare-metal`) - migration
  `000014_nodes_collapse_runtime_backend`, backfilling existing rows
  (`docker-gpu`+`container_runtime` -> the matching value, `spark` ->
  `bare-metal`) and verified up/down/up against a real Postgres instance.
  The original design conflated a hardware label with a runtime-mechanism
  choice, backwards: a DGX Spark's GB10 GPU supports passthrough to a
  container without affecting a display connected to it (NVIDIA's
  supported use case for that hardware), so CDI GPU passthrough into a
  Docker/Podman container should work fine there, while a single-GPU
  workstation (e.g. this project's own RTX 4090 dev laptop) is the case
  that actually needs the bare-metal backend, since its GPU is already
  claimed by the host OS's own session and can't be passed through at
  all - see PLANNING.md's 2026-08-13 Decisions Log entry for the full
  correction. `internal/db.RuntimeBackend` replaces `NodeType`/
  `ContainerRuntime`; `Node.RuntimeBackend` is a single non-nullable field
  (was two, one nullable); `NodeRepository.Create` takes one parameter
  instead of two. `internal/nodes.RegisterNodeParams.RuntimeBackend`
  replaces its two fields, and `validate()`'s pairing logic (spark must
  have a nil `container_runtime`, docker-gpu must have a non-nil one)
  simplifies to a plain known-value check, since a mismatched pairing is
  no longer constructible at the Go type level - the integration test
  that existed specifically to prove the database's CHECK constraint
  rejected such a mismatch is removed, not updated, since there is
  nothing left to mismatch. The node registration form
  (`web/templates/pages/register_node.html`) now offers one "Runtime
  backend" dropdown (`docker` / `podman` / `bare-metal`) instead of two
  linked fields. `agent/config`'s `SPARKY_NODE_TYPE`/
  `SPARKY_CONTAINER_RUNTIME` collapse to one required
  `SPARKY_RUNTIME_BACKEND`, matching the schema - the agent doesn't
  branch on this value for backend selection yet either way, that
  remains real v0.2.0 work. SCHEMA.md, ARCHITECTURE.md, CLAUDE.md, and
  docs/AGENT.md updated to match; PLANNING.md's v0.2.0 milestone entry
  and Dependencies and Blockers section corrected to stop describing the
  bare-metal backend as Spark-specific. Already-completed Phase
  write-ups describing the old schema as it was actually built at the
  time are deliberately left unedited, matching this project's existing
  never-rewrite-history discipline.

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
- `running_instances` no longer stays stale at `status = running` after an
  ungraceful agent stop (a crash, `kill -9`, or a `systemctl stop` that
  raced an engine's own shutdown handling) - the Dashboard used to keep
  reporting a model as loaded indefinitely with nothing to reconcile it.
  `agent/runtime.Backend` gained `IsRunning`; `internal/agentconn.Handler`
  gained an `OnConnectFunc` hook fired on every fresh agent connection;
  new `internal/lifecycle.Service.ReconcileNode` (wired as that hook)
  asks the agent, via a new `check_instance` protocol message, to confirm
  each `running`-status instance it still believes is loaded on that
  node - reusing the existing `instance_result` response path with no new
  response type needed. Works correctly for both runtime backends with no
  special-casing: a Docker/Podman container survives an agent restart by
  design, so `IsRunning` querying the daemon directly still confirms it;
  a bare-metal process's tracking is pure in-memory state, so a genuine
  crash-and-restart correctly reports "not running" for anything the
  agent no longer remembers starting. Verified with a real end-to-end
  pass: a real compiled agent and server, a real `SIGKILL` crash and
  restart, confirming the stale row flips to `stopped` within about a
  second of reconnecting with zero operator action. See PLANNING.md's
  2026-08-16 Decisions Log entry for the full design.
- A `load_instance`/`unload_instance` dispatch failure (the WebSocket send
  itself erroring right after a `running_instances` row was already
  persisted) no longer leaves that row stuck at `starting`/`stopping`
  forever with no way to retry. `LoadInstance` now moves the row to
  `failed` with the dispatch error recorded, since the agent was never
  actually told to start anything; `UnloadInstance` now reverts the row
  back to `running`, since the instance is presumably still fine and this
  is also what unblocks a retry (`UnloadInstance` requires `status =
  running` before it will act). See PLANNING.md's Decisions Log for the
  full rationale.

### Security
- CSRF protection on every state-changing endpoint (`/login`,
  `/login/break-glass`, `/logout`, `/nodes/register`, `/profiles/new`,
  `/profiles/{id}/edit`, `/profiles/{id}/load`, `/instances/{id}/unload`,
  `/users/{id}/tier`) - closes an app-wide gap on record since Dashboard UI
  Phase 2, per CLAUDE.md Security Considerations. New
  `internal/httpapi/csrf.go`: a hand-rolled double-submit-cookie token
  (`sparky_csrf`, `HttpOnly`/`Secure`/`SameSite=Lax`, matching the session
  cookie's own attributes), a global `ensureCSRFToken` middleware that
  guarantees a token exists before any page renders, and a per-route
  `RequireCSRF` middleware wired explicitly alongside `RequireSession` on
  each write route. The four classic `<form method="post">` writes (login,
  break-glass login, node registration, profile create/edit) embed the
  token as a hidden `csrf_token` field; the four htmx `hx-post` writes
  (logout, tier-change, load, unload) pick it up from a single `hx-headers`
  attribute on `base.html`'s `<body>`. Skips enforcement for non-form
  (JSON API) requests, which aren't triggerable by a naive cross-site form
  submission in the first place. See PLANNING.md's 2026-08-16 Decisions Log
  entry for the full design and the choices confirmed with the user.

---

## [0.1.0] - YYYY-MM-DD

### Added
- Initial release

[Unreleased]: https://github.com/1kaius1/Sparky/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/1kaius1/Sparky/releases/tag/v0.1.0
