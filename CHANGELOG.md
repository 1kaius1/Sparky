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

### Changed

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
