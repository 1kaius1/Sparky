# CLAUDE.md - Sparky (Web Application, Go)

This file provides project context and conventions for AI-assisted development
sessions. Read this file and `ARCHITECTURE.md` (plus `SCHEMA.md` for data model
detail) before making any changes. See `.clauderules` for behavioral rules that
govern this session.

This repo is a monorepo containing two binaries - `cmd/sparky-server` (this file's
primary subject) and `cmd/sparky-agent` (see `docs/AGENT.md` for agent-specific
conventions: systemd integration, signal handling, runtime backends). Everything
below applies to the server unless stated otherwise.

A handful of choices below were not explicitly settled during design and are marked
**[default - confirm or override]**. Everything else reflects an actual decision made
during design; see `PLANNING.md`'s Decisions Log for the rationale behind any of it.

---

## Project Overview

Sparky gives AD-authenticated developers self-service control over loading and
tuning LLM inference engines across a small fleet of GPU compute nodes - NVIDIA DGX
Spark hardware and other Docker/Podman GPU hosts alike - with tiered permissions,
full audit logging, and live and historical hardware telemetry. Built for a single
internal team managing a handful of nodes, not a multi-tenant or hyperscale product.

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
| Container mgmt  | `docker/docker/client` (Docker Engine API) | Targets Docker and Podman identically - Podman exposes a Docker-Engine-API-compatible socket |

### Frontend

| Component       | Choice                                     | Reason       |
|------------------|-----------------------------------------------|--------------|
| Approach        | Server-rendered + htmx partial swaps         | Consistency with the project's minimal-infrastructure philosophy; single-binary deployment via `embed.FS` - see `PLANNING.md` Decisions Log |
| Framework       | Go `html/template` + htmx                    | No JS framework, no separate frontend release artifact |
| Build tool      | None                                          | Templates and static assets embed directly into the binary |
| CSS approach    | Plain CSS **[default - confirm or override]** | Avoids reintroducing a build step (a Tailwind PostCSS pipeline) for a project that deliberately has none; the Tailwind standalone CLI is a reasonable alternative if utility classes are wanted without Node |
| Charting        | Chart.js (GPU/CPU/memory dashboards only)     | The one place htmx alone isn't enough - loaded via CDN or vendored, not a build dependency |

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
  - db/                    # Repository layer (pgx), query code
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
- .clauderules
- ARCHITECTURE.md
- SCHEMA.md
- CHANGELOG.md
- CLAUDE.md
- CONTRIBUTING.md
- PLANNING.md
- README.md
- LICENSE
```

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

All automated tests must pass before committing or opening a pull request. Never
suggest a PR with a failing automated test. If tests cannot be run, say so explicitly.
The manual checklist in `ARCHITECTURE.md` is a separate, human-confirmed gate before a
version is tagged - passing automated tests never implies it's satisfied.

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

Everything else that's user-configurable after first run (metrics export
destination, audit retention, permission overrides) lives in the database, set
through the UI or `sparky setup` - not as environment variables. See
`ARCHITECTURE.md` Security Considerations for why this split exists.

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
  connection never reload. See `ARCHITECTURE.md` Component Breakdown.
- Sidebar sections and their minimum visible tier (fill in as built): Dashboard
  (Read-only), Nodes (Read-only view / Admin edit), Model profiles (Read-only view /
  Developer launch / PowerDev create), Transfers (Read-only view / Admin+grant
  initiate), Metrics (Read-only), Users & permissions (Admin), Audit log (Admin),
  Settings (Admin).
- No inline styles - CSS classes only.

---

## Code Style and Conventions

Behavioral rules are in `.clauderules`. Key reminders:

- ASCII only in code and comments - no emoji, no em-dashes
- Explain WHY in comments, not WHAT
- Errors handled explicitly - no silent failures; wrap with `fmt.Errorf("context: %w", err)`
- `context.Context` is always the first argument on handler and service functions
- Conventional commits: `type(scope): description`
- Every state-changing handler goes through the audit middleware - see
  `ARCHITECTURE.md` Audit Log. If you're writing a handler that changes state and it
  isn't wrapped, that's a bug, not a style choice.

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
RTX 3080Ti), before Spark-specific bare-metal support in v0.2.0. See `PLANNING.md`
Decisions Log for why the sequencing runs this direction.
