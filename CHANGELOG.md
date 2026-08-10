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

### Deprecated

### Removed

### Fixed

### Security

---

## [0.1.0] - YYYY-MM-DD

### Added
- Initial release

[Unreleased]: https://github.com/1kaius1/Sparky/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/1kaius1/Sparky/releases/tag/v0.1.0
