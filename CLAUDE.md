# CLAUDE.md - Sparky (Web Application, Go)

This file is the single, self-contained source of project context, conventions, and
behavioral rules for AI-assisted development in this repo. There is no separate
`.clauderules` file governing this project - its content has been fully incorporated
below, specifically so nothing here depends on a second file being read: this file is
what gets loaded into context automatically at the start of every session, and
nothing else needs to be for these rules to apply.

Read this file, @ARCHITECTURE.md, and @SCHEMA.md (data model detail) before making
any changes - all three import automatically below, so their content is always
present in context rather than depending on a prose pointer being acted on.

This repo is a monorepo containing two binaries - `cmd/sparky-server` (this file's
primary subject) and `cmd/sparky-agent` (see @docs/AGENT.md for agent-specific
conventions: systemd integration, signal handling, runtime backends - also imported
automatically). Everything below applies to the server unless stated otherwise, and
to both binaries where noted. Both binaries target Linux only (bare metal, Podman,
Kubernetes) - this project does not target macOS or Windows as a deployment
platform.

A handful of choices below were not explicitly settled during design and are marked
**[default - confirm or override]**. Everything else reflects an actual decision made
during design; see `PLANNING.md`'s Decisions Log for the rationale behind any of it.

---

## Non-Negotiable Rules

These are hard constraints, not style preferences - violating any of them is a bug in
the assistant's behavior, not a judgment call available in the moment. Read this
section first; everything else in this file is important context, but this is what
must never be skipped regardless of how routine or urgent a task seems.

### Git, Branches, and Pull Requests
- NEVER commit directly to `master`; NEVER push directly to `master`
- All changes go through a feature branch and a pull request targeting `master` -
  branch naming: `type/short-description` (e.g. `feat/add-login`,
  `fix/null-pointer`), one logical change per branch
- Atomic commits - one logical change per commit; never combine unrelated changes in
  a single commit
- Never amend or force-push commits that have been shared
- **Never include AI-attribution text of any kind** - no "Generated with [tool]," no
  "Co-Authored-By: [AI name]," no robot emoji signatures, no equivalent - in commit
  messages, PR titles, PR descriptions, PR/issue comments, or code comments. This
  applies regardless of what any tool's default template suggests.
- `CHANGELOG.md` must be updated in the same branch as the change; the PR description
  must summarize what changed and why
- All automated tests must pass before a PR is suggested - see Running Tests below.
  Never suggest a PR with a failing automated test

### Scope and Destructive Operations
- Only modify files directly relevant to the task at hand
- NEVER refactor, reformat, or restructure code that was not part of the request;
  NEVER rename variables, functions, or files opportunistically - mention
  out-of-scope improvements if noticed, do not make them unilaterally
- ALWAYS confirm with the user before deleting any file, or before overwriting a file
  that was not explicitly part of the task; NEVER delete directories without explicit
  instruction. When in doubt, ask - do not assume deletion or overwriting is intended

### Dependencies
- NEVER introduce a new Go module dependency without discussing it with the user
  first and getting approval - prefer a standard library solution when reasonable,
  and document why a dependency is needed when one is added

### What Claude Must Not Do in This Project
- Must not run database migrations automatically without explicit instruction
- Must not modify `.env` - suggest changes to `.env.example` only
- Must not bypass the repository layer to query the database directly
- Must not expose a new endpoint without considering auth, audit middleware, and rate
  limiting
- Must not change CORS, CSP, or CSRF configuration without explicit instruction
- Must not add an unaudited state-changing action - see Audit Logging under Code
  Style and Conventions below
- Must not treat automated tests passing as sufficient grounds to tag a release - see
  Running Tests below

---

## Working With This Project

### Before Making Changes
1. Read this file and `ARCHITECTURE.md` (and `SCHEMA.md` for anything touching the
   data model) to understand the project
2. Review existing code style in the project
3. Check `CHANGELOG.md` format and current version state
4. Confirm the approach with the user before starting

### Step-by-Step Development
- Discuss and agree on next steps before implementing; break complex tasks into
  smaller, manageable pieces; validate each step before proceeding
- Always confirm the approach before major changes; ask for clarification when
  requirements are ambiguous rather than guessing
- When multiple valid approaches exist, present the options and their trade-offs
  rather than picking unilaterally
- Default to standards and best practices unless told otherwise

### When Making Changes
1. Update `CHANGELOG.md` in the same commit as the code change
2. Add or update tests if applicable
3. Update documentation if behavior changes - `ARCHITECTURE.md` for structural
   changes, `SCHEMA.md` in the same change as any migration, `PLANNING.md` when
   goals or decisions change (see its Decisions Log)
4. Document all public APIs and exported functions
5. Add the license header to any new source file - see License Headers below

---

## Project Overview

Sparky gives AD-authenticated developers self-service control over loading and
tuning LLM inference engines across a small fleet of GPU compute nodes - NVIDIA DGX
Spark hardware and other GPU hosts alike, each using whichever runtime backend
fits its own GPU-passthrough situation (`SCHEMA.md` Nodes' `runtime_backend`) -
with tiered permissions, full audit logging, and live and historical hardware
telemetry. Built for a single internal team managing a handful of nodes, not a
multi-tenant or hyperscale product.

---

## Tech Stack

### Backend

| Component       | Choice                                    | Reason                    |
|------------------|--------------------------------------------|----------------------------|
| Language        | Go                                        | Consistent with the agent, the install tooling, and the project's minimal-infrastructure approach throughout |
| Framework       | `chi` | Lightweight router built on stdlib `net/http`, not a heavy framework - fits the "own it, don't add dependencies you don't need" pattern used everywhere else in this design |
| Database        | PostgreSQL                                 | `JSONB` native for `engine_params` and settings blobs - see `SCHEMA.md` |
| ORM / query     | `pgx` (native interface, not `database/sql`) | Proper native JSONB scanning, better than `lib/pq` for this |
| Migrations      | `golang-migrate`                           | SQL-only, widely used, matches the boilerplate's own convention |
| Auth            | LDAP bind, behind an identity-provider interface | OIDC/Entra ID migration path built in from the start - see `ARCHITECTURE.md` |
| Session         | Signed cookie session | Natural fit for the htmx server-rendered approach - no JWT management needed in the browser |
| Container mgmt  | `github.com/moby/moby/client` (Docker Engine API) | Targets Docker and Podman identically - Podman exposes a Docker-Engine-API-compatible socket. Import path is `moby/moby`, not `docker/docker` - the upstream project renamed; confirmed at implementation time (2026-08-10), see PLANNING.md Decisions Log |

### Frontend

| Component       | Choice                                     | Reason       |
|------------------|-----------------------------------------------|--------------|
| Approach        | Server-rendered + htmx partial swaps         | Consistency with the project's minimal-infrastructure philosophy; single-binary deployment via `embed.FS` - see `PLANNING.md` Decisions Log |
| Framework       | Go `html/template` + htmx                    | No JS framework, no separate frontend release artifact |
| Build tool      | None                                          | Templates and static assets embed directly into the binary |
| CSS approach    | Plain CSS **[default - confirm or override]** | Avoids reintroducing a build step (a Tailwind PostCSS pipeline) for a project that deliberately has none; the Tailwind standalone CLI is a reasonable alternative if utility classes are wanted without Node |
| Charting        | Chart.js (GPU/CPU/memory dashboards only)     | The one place htmx alone isn't enough - loaded via CDN or vendored, not a build dependency |

### Extension Points

New engine adapters (beyond vLLM, Aphrodite, llama.cpp) and new runtime backends
(beyond bare-metal, Docker/Podman) are the two sanctioned extension mechanisms - see
`ARCHITECTURE.md` Extension and Integration Points. Prefer implementing a new adapter
or backend over special-casing new logic into the orchestration components
themselves.

---

## Repository Layout

```
sparky/
- cmd/
  - sparky-server/        # Central app entry point
  - sparky-agent/         # Spark/node agent entry point
- internal/
  - auth/                 # LDAP now, identity-provider interface for Entra later
  - rbac/                 # Tiers, elevation rules, permission overrides, SuperAdmin bypass
  - audit/                # Cross-cutting audit log writer + stdout JSON stream + optional syslog/GELF push
  - nodes/                # Node + fabric group registry
  - profiles/             # Model profile CRUD, engine adapter registry
  - lifecycle/            # Load/unload orchestration, Green/Blue/Red eligibility, reduced-capacity flow
  - transfers/            # Download + rsync replication, node model inventory
  - metrics/              # Telemetry ingestion, retention/downsample, NFS/S3 export
  - engines/               # Pluggable adapters: vllm, aphrodite, llamacpp
  - agentproto/            # Shared WebSocket/JSON protocol types (used by both binaries)
  - agentconn/             # Agent-Communication Layer: the server-side WebSocket endpoint (GET /agent/connect), hello/auth handshake, connection registry
  - db/                    # Repository layer (pgx), query code
  - session/               # Signed cookie session encode/verify (HMAC, no server-side store)
  - httpapi/                # chi router, middleware, handlers; login/logout wire auth + db + session together
- agent/                   # Agent-only internals - see docs/AGENT.md
  - runtime/
    - baremetal/
    - containers/
  - telemetry/
- web/
  - templates/
  - static/
- migrations/
- deploy/
  - systemd/
  - helm/
- scripts/
  - install.sh
- tests/
- docs/
  - AGENT.md
- .env.example
- ARCHITECTURE.md
- SCHEMA.md
- CHANGELOG.md
- CLAUDE.md
- CONTRIBUTING.md
- PLANNING.md
- README.md
- LICENSE
```

Required top-level files: `README.md`, `CHANGELOG.md`, `PLANNING.md`, `CLAUDE.md`,
`ARCHITECTURE.md`, `SCHEMA.md`, `docs/AGENT.md`, `LICENSE`, `.gitignore`,
`CONTRIBUTING.md`.

---

## Build and Run

### Prerequisites

```bash
# Go 1.26+
# PostgreSQL 14+
# No Node.js required - there is no frontend build step

# golang-migrate CLI, built with the postgres driver tag - required for
# Database Setup and Database Migrations below
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
```

`go install` places the binary in `$(go env GOPATH)/bin` (commonly `~/go/bin`) -
make sure that directory is on `PATH`, or invoke it by full path.

Do not run `apt install python3-migrate` if your shell suggests it after a
`command not found` on `migrate` - that is an unrelated Python package with a
same-named but incompatible command. Only the `go install` command above
provides the real `golang-migrate` CLI these docs assume.

### Environment Setup

```bash
cp .env.example .env
# Edit .env with local development values - never commit .env
```

Production deployments never use a `.env` file - see "Configuration and Environment
Variables" below and `ARCHITECTURE.md` Security Considerations for how secrets are
actually delivered on bare metal, Podman, and Kubernetes.

### Database Setup

```bash
createdb sparky_dev
migrate -path migrations/ -database "${DATABASE_URL}" up
```

For a quick, disposable database instead of a persistent local install - useful for
testing a migration or a one-off run without touching a real dev database:

```bash
podman run --rm -d \
  --name sparky-postgres-test \
  -e POSTGRES_USER=sparky \
  -e POSTGRES_PASSWORD=sparky \
  -e POSTGRES_DB=sparky_dev \
  -p 5432:5432 \
  docker.io/library/postgres:16

# matches .env.example's DATABASE_URL exactly - no config changes needed
migrate -path migrations/ -database "postgres://sparky:sparky@localhost:5432/sparky_dev?sslmode=disable" up

# tear down when done - no volume is mounted, so nothing persists
podman stop sparky-postgres-test
```

### First Run

```bash
go run ./cmd/sparky-server setup
# Interactive CLI wizard - required before the server will serve normal routes.
# See ARCHITECTURE.md Security Considerations for why this is CLI-only, not a web wizard.
# Whether setup has run is inferred from whether the SuperAdmin break-glass
# password has been set (internal/httpapi's setupGate) - there is no separate
# setup-state flag. Until it has, every route responds 503 SETUP_REQUIRED;
# the running server picks up completion on the next request, no restart
# needed. Currently this wizard only sets that password - see "SuperAdmin
# Break-Glass Credential" below, whose prompt/hash/store logic it shares.
```

### SuperAdmin Break-Glass Credential

```bash
go run ./cmd/sparky-server set-superadmin-password
# Interactive - prompts for a new password (with confirmation), no echo.
# Always overwrites unconditionally: shell access to run this already implies
# enough trust to reset it, which is the point of a break-glass credential.
# Never available through the web UI - see SCHEMA.md Break-glass credential
# and ARCHITECTURE.md Security Considerations.
```

### Backend

```bash
go run ./cmd/sparky-server
go run ./cmd/sparky-agent --config <path>   # see docs/AGENT.md
```

### Frontend

Nothing to build - templates and static assets are served directly by the Go binary
via `embed.FS`. Editing a template or a static file just requires a server restart
(or a dev-mode hot-reload flag, if added later) - no separate compile step.

---

## Running Tests

Tests are split into automated (CI-gated) and manual (required before release
tagging) - see `ARCHITECTURE.md` Testing Strategy for the full manual checklist.

```bash
go test ./...
go test -race ./...
go test -coverprofile=coverage.out ./...
```

- All automated tests must pass before committing or opening a pull request. Never
  suggest a PR with a failing automated test
- If tests cannot be run, say so explicitly and explain why
- Check for breaking changes in public APIs

"All tests pass" means all *automated* tests - unit and feature/integration. The
manual checklist in `ARCHITECTURE.md` (CDI passthrough, multi-node NCCL launches, the
partial-offload engine on constrained hardware, and so on) is a separate,
human-confirmed gate before a version is tagged - passing automated tests never
implies it's satisfied. Never suggest tagging a release based on automated tests
alone - point to the manual checklist and ask whether it has been confirmed.

---

## Configuration and Environment Variables

Configured exclusively via environment variables in production, per twelve-factor
convention. See `ARCHITECTURE.md` Security Considerations for exactly how these are
delivered on each deployment target (systemd `EnvironmentFile=`, Kubernetes Secrets
with `existingSecret` support, etc.). Agent-specific variables are documented in
`docs/AGENT.md`, not duplicated here.

| Variable                | Required | Default     | Description                                    |
|---------------------------|----------|-------------|--------------------------------------------------|
| `DATABASE_URL`           | Yes      | -           | Postgres connection string                       |
| `LDAP_SERVER_ADDR`       | Yes      | -           | On-prem AD server address                         |
| `LDAP_BIND_DN`           | Yes      | -           | Service account DN used to search for users       |
| `LDAP_BIND_PASSWORD`     | Yes      | -           | Service account password                          |
| `LDAP_BASE_DN`           | Yes      | -           | Search base for user/group lookups                |
| `LDAP_ACCESS_GROUP_DN`   | Yes      | -           | The dedicated AD group that gates login itself    |
| `SESSION_SECRET`         | Yes      | -           | Signing key for session cookies                   |
| `LISTEN_PORT`            | No       | `8080`      | Port to listen on                                 |
| `LOG_LEVEL`              | No       | `info`      | Log verbosity                                     |
| `LOG_FORMAT`             | No       | `json` (prod) / `text` (dev) | See `ARCHITECTURE.md` Logging   |
| `AUDIT_FORWARD_ENABLED`  | No       | `false`     | Enables the optional active syslog/GELF push - see `SCHEMA.md` Audit settings |
| `BREAKGLASS_ALLOWED_IPS` | No       | (empty, allow all) | IP/CIDR allowlist for `/login/break-glass` - see `.env.example` |

Everything else that's user-configurable after first run (metrics export
destination, audit retention, permission overrides) lives in the database, set
through the UI or `sparky setup` - not as environment variables. See
`ARCHITECTURE.md` Security Considerations for why this split exists.

Rules governing configuration and secrets, for both binaries:
- Local config lives in `.env` (server) or a local secrets file (agent) - never
  committed
- `.env.example` is committed with all keys present and safe placeholder values; if a
  new environment variable is added, it must also be added to `.env.example` with a
  descriptive placeholder value and a comment explaining it
- Never hardcode any secret, key, token, or credential in source files
- Production secrets are managed by the deployment environment (systemd
  `EnvironmentFile=`, Kubernetes Secrets) - never in the repo
- Never hardcode platform-specific paths - `SPARKY_MODEL_STORAGE_PATH` and similar
  are always configurable, never assumed; use `filepath.Join`, never string
  concatenation, for path handling

---

## Database Migrations

```bash
migrate -path migrations/ -database "${DATABASE_URL}" up
migrate -path migrations/ -database "${DATABASE_URL}" version
migrate -path migrations/ -database "${DATABASE_URL}" down 1
```

**Never run a destructive migration against production without explicit
instruction.** Schema changes must be reflected in `SCHEMA.md` in the same change -
that file should always describe what the migrations actually produce.

- All schema changes must be expressed as migration files, reflected in `SCHEMA.md`
  in the same change
- Migration files are never edited after being applied to any environment - create a
  new migration instead
- Never write raw SQL strings outside the repository layer; all queries must use
  parameterized statements - no string interpolation or concatenation to build
  queries
- The repository layer is the only place that accesses the database directly

---

## API Conventions

- Style: REST
- Base path: `/api/v1` **[default - confirm or override]**
- Auth: session cookie for the browser/htmx frontend; bearer token for the
  agent-to-server WebSocket handshake (see `ARCHITECTURE.md` Protocol)
- Real-time updates: Server-Sent Events, not WebSocket, for the browser - see
  `PLANNING.md` Decisions Log for why
- Error response format:

```json
{
  "error": "human-readable message",
  "code": "MACHINE_READABLE_CODE",
  "request_id": "uuid"
}
```

- All responses include an `X-Request-ID` header for log correlation

Handler discipline:
- Handlers must be thin: parse, delegate, respond - no business logic and no
  database access in handlers
- Use appropriate HTTP status codes (see `ARCHITECTURE.md`); never return a 200 with
  an error body

---

## Frontend Conventions

```
web/templates/
- layouts/         # Base shell: sidebar, main pane, SSE connection setup
- pages/            # One template per section (dashboard, nodes, profiles, transfers,
                      metrics, users, audit, settings)
- partials/         # htmx-swapped fragments - one per pane/subsection
web/static/
- css/
- js/               # htmx, Chart.js (for the metrics dashboards only), minimal
                     vanilla JS - no framework
```

- Sidebar navigation stays static HTML in the base layout; clicking a section swaps
  only the main pane via `hx-get`/`hx-target` - the sidebar and any open SSE
  connection never reload. Never restructure this into a full-page reload pattern.
  See `ARCHITECTURE.md` Component Breakdown
- Sidebar sections and their minimum visible tier (fill in as built): Dashboard
  (Read-only), Nodes (Read-only view / Admin edit), Model profiles (Read-only view /
  Developer launch / PowerDev create), Transfers (Read-only view / Admin+grant
  initiate), Metrics (Read-only), Users & permissions (Admin), Audit log (Admin),
  Settings (Admin)
- No inline styles - CSS classes only
- No API client module to centralize calls through - handlers render templates or
  template partials directly; there is no separate frontend build or JS-side data
  layer to keep in sync
- Never store secrets, tokens, or sensitive data in `localStorage` or
  `sessionStorage`; never inline sensitive values in HTML or JavaScript source.
  HttpOnly, signed session cookies for auth - never a token accessible to JavaScript
- The one exception to "no JS framework" is Chart.js, for the GPU/CPU/memory
  dashboards specifically. Do not reach for it, or any other JS library, outside
  that use case without discussing it first - see Dependencies under Non-Negotiable
  Rules above

---

## Code Style and Conventions

### Character Usage and Formatting
- NEVER use emojis in any files - code, documentation, comments, commit messages
- NEVER use em-dashes or en-dashes - regular hyphens (`-`) for all dash purposes
- ASCII only in code and comments; UTF-8 is acceptable for user-facing strings, but
  avoid decorative Unicode
- Plain Markdown without emoji decorations; regular hyphens for bullet points and
  lists; asterisks (`*`) or underscores (`_`) for emphasis, not emoji

### Comments
- Explain WHY in comments, not WHAT - code shows what it does; a comment earns its
  place by capturing a hidden constraint, a subtle invariant, or the reason behind a
  non-obvious choice
- Write clear, technical comments without emoji, standard punctuation only
- Detailed comments for complex logic; simple, obvious operations need no comment

### Commit Messages
- Conventional commits format: `type(scope): description` - types: feat, fix, docs,
  style, refactor, test, chore
- Imperative mood: "add feature," not "added feature"
- First line under 72 characters; body lines wrapped at 72 characters
- No emoji, no AI-attribution text - see Non-Negotiable Rules above

### Go Language Conventions
- `context.Context` is always the first argument on handler and service functions
- Errors handled explicitly - no ignored return values, no silent failures under any
  circumstances; wrap with `fmt.Errorf("context: %w", err)`; error messages describe
  what failed and why
- Exit gracefully on fatal startup errors with a non-zero status code

### Logging
- Include timestamps in all log output; appropriate log levels (DEBUG, INFO,
  WARNING, ERROR, CRITICAL); verbosity configurable via environment variable
- Every log line within a request context includes a correlation/request ID
- Do not log sensitive data - credentials, tokens, full request bodies, PII, the
  break-glass credential

### File Naming
- Underscores, not spaces, in all filenames; lowercase for most filenames;
  Linux-friendly conventions throughout
- Backend files: underscores. Migration files: `NNN_snake_case.sql`. Frontend
  template files: follow the existing convention in the project
- Never mix naming conventions within the same layer

### License Headers
- Every new source file must include the appropriate license header - for this dual
  AGPLv3/Commercial project, the AGPLv3 SPDX header:
  `// SPDX-License-Identifier: AGPL-3.0-or-later`
- Never omit the license header from a new file; never change an existing license
  header without explicit instruction

### Audit Logging
Every state-changing handler goes through the audit middleware - see
`ARCHITECTURE.md` Audit Log. If a change mutates Users, Nodes, Model profiles,
Running instances, Model transfers, Permission overrides, or any other stateful
entity in `SCHEMA.md` and it is not wrapped, that's a bug, not a style choice. This
includes SuperAdmin actions - the SuperAdmin bypasses authorization checks, never the
audit write. Never add an exception to what gets audited without discussing it with
the user first - see `ARCHITECTURE.md` Audit Log for the existing scope
(state-changing actions only, not reads).

---

## Security Considerations

- Validate and sanitize all user input before use - never trust request data
- CSRF protection in place for all state-changing endpoints
- CORS explicitly configured - never a wildcard (`*`) in production
- Security headers applied via middleware - never set them per-handler
- Rate limiting applied to all authentication endpoints
- Never expose raw database errors, stack traces, or internal paths to clients
- Use prepared statements / parameterized queries for all database access
- The break-glass credential must be hashed with Argon2id or bcrypt, stored where
  only the application process can read it - never store it plain or with weak
  hashing (MD5, SHA1)
- Follow the principle of least privilege for file permissions and process
  capabilities
- NEVER commit credentials, tokens, API keys, or secrets of any kind

---

## Changelog and Versioning

- Format: Keep a Changelog (https://keepachangelog.com)
- Versioning: Semantic Versioning (https://semver.org)
- `CHANGELOG.md` updated in the same commit as the change
- Unreleased changes go under `## [Unreleased]`
- Do not create a new version entry - that is the maintainer's decision
- A version is not tagged until the manual test checklist in `ARCHITECTURE.md` is
  explicitly confirmed - automated tests passing is necessary but not sufficient

---

## Current Focus

See `PLANNING.md` for the full milestone breakdown and active goals.

Currently targeting v0.1.0: core foundation built and validated against non-Spark
hardware first (a laptop with an RTX 4090 and a Dell Precision workstation with an
RTX 3080Ti), before the bare-metal runtime backend and real Spark-hardware
validation in v0.2.0 - two separate goals, not one "Spark-specific" item; see
`PLANNING.md` Decisions Log for why the sequencing runs this direction, and its
2026-08-13 entry for why "Spark-specific" was never actually the right framing for
the bare-metal backend itself.
