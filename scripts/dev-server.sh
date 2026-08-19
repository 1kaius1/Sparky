#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Dev-only convenience script: spins up a disposable Postgres container
# (podman), runs migrations, sets a throwaway SuperAdmin break-glass
# password, builds and starts sparky-server, and opens the break-glass
# sign-in page in your browser. Ctrl-C stops the server and tears down
# the Postgres container - nothing persists between runs, and .env / any
# real database are never touched. See CLAUDE.md's Database Setup for the
# equivalent manual steps this automates.
#
# There is no AD/LDAP server in a throwaway setup like this, so the
# regular username/password login form won't work - this script opens
# the SuperAdmin break-glass sign-in page (/login/break-glass) instead,
# and copies the password you chose to your clipboard so you can just
# paste it into the form.
#
# Prerequisites: podman, the golang-migrate CLI (CLAUDE.md Prerequisites),
# a Go toolchain, and `script` (util-linux - used only to drive
# `sparky-server setup`'s interactive, no-echo password prompt through a
# real pty non-interactively, since it refuses to read from a plain pipe).

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

CONTAINER_NAME="sparky-dev-server-postgres"
DB_PORT="${DEV_DB_PORT:-5432}"
DB_URL="postgres://sparky:sparky@localhost:${DB_PORT}/sparky_dev?sslmode=disable"
LISTEN_PORT="${LISTEN_PORT:-8080}"

BUILD_DIR=""
SERVER_PID=""
CLEANED_UP=0

cleanup() {
  # INT and EXIT both trap here - a real Ctrl-C fires the INT handler and
  # then, as the script's own last command finishes, the EXIT handler too.
  # Guard so the teardown steps (which aren't all safely re-run, cosmetically
  # if nothing else) only happen once.
  [[ "$CLEANED_UP" -eq 1 ]] && return
  CLEANED_UP=1
  echo
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "Stopping sparky-server..."
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  echo "Stopping Postgres container ($CONTAINER_NAME)..."
  podman stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
  [[ -n "$BUILD_DIR" ]] && rm -rf "$BUILD_DIR"
}
trap cleanup EXIT INT TERM

for bin in podman migrate go script curl; do
  command -v "$bin" >/dev/null 2>&1 || { echo "error: '$bin' is required but not found on PATH" >&2; exit 1; }
done

# Idempotent re-run: a container left over from a crashed previous run
# (one that never reached the cleanup trap) would otherwise make `podman
# run --name` fail outright.
podman rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

echo "Starting a disposable Postgres container ($CONTAINER_NAME, port $DB_PORT)..."
podman run --rm -d --name "$CONTAINER_NAME" \
  -e POSTGRES_USER=sparky -e POSTGRES_PASSWORD=sparky -e POSTGRES_DB=sparky_dev \
  -p "${DB_PORT}:5432" docker.io/library/postgres:16 >/dev/null

echo "Waiting for Postgres to accept connections..."
# The official postgres image starts a temporary instance first (to run
# initdb/init scripts) on the very first run of a fresh container, then
# stops it and starts the real one - pg_isready alone can observe that
# temporary instance's "ready" state and return success during the
# restart window right after, which is exactly the "database system is
# starting up" error migrate hit. "ready to accept connections" appears
# once per instance start, so waiting for it twice (or once, on a rerun
# against an already-initialized data directory, where there's no
# temporary instance at all) is the actual ready signal.
until podman exec "$CONTAINER_NAME" pg_isready -U sparky >/dev/null 2>&1; do
  sleep 0.5
done
until [[ "$(podman logs "$CONTAINER_NAME" 2>&1 | grep -c "ready to accept connections" || true)" -ge 2 ]]; do
  sleep 0.5
done

echo "Running migrations..."
# Belt-and-suspenders on top of the log-based wait above: retry briefly
# rather than fail outright on the same transient "starting up" error,
# since the exact log-message wording/count isn't guaranteed stable
# across postgres image versions.
MIGRATE_ATTEMPTS=20
until migrate -path migrations/ -database "$DB_URL" up; do
  MIGRATE_ATTEMPTS=$((MIGRATE_ATTEMPTS - 1))
  if [[ "$MIGRATE_ATTEMPTS" -le 0 ]]; then
    echo "error: migrations failed after repeated retries" >&2
    exit 1
  fi
  sleep 0.5
done

echo "Building sparky-server..."
BUILD_DIR="$(mktemp -d)"
BIN="$BUILD_DIR/sparky-server"
go build -o "$BIN" ./cmd/sparky-server

export DATABASE_URL="$DB_URL"
export SESSION_SECRET="dev-only-session-secret-not-for-production"
export LISTEN_PORT
# LDAP is required config but unreachable by design here - only the
# SuperAdmin break-glass login (below) works in this throwaway setup.
export LDAP_SERVER_ADDR="ldap://unreachable.invalid:389"
export LDAP_BIND_DN="CN=unused,DC=example,DC=internal"
export LDAP_BIND_PASSWORD="unused"
export LDAP_BASE_DN="DC=example,DC=internal"
export LDAP_ACCESS_GROUP_DN="CN=unused,DC=example,DC=internal"
# BREAKGLASS_ALLOWED_IPS is deliberately left unset - empty means allow
# from anywhere (internal/httpapi/breakglass_ip_whitelist.go), which is
# what a throwaway local script needs. Hardcoding a loopback CIDR here
# would break under setups where "localhost" doesn't resolve the way a
# given dev environment expects (WSL, a container, a forwarded port).

echo
read -r -s -p "Choose a SuperAdmin break-glass password for this session (min 12 characters): " DEV_PASSWORD
echo
if [[ ${#DEV_PASSWORD} -lt 12 ]]; then
  echo "error: password must be at least 12 characters" >&2
  exit 1
fi

echo "Setting the break-glass password..."
if ! printf '%s\n%s\n' "$DEV_PASSWORD" "$DEV_PASSWORD" | script -qec "'$BIN' setup" /dev/null; then
  echo "error: sparky-server setup failed" >&2
  exit 1
fi

echo "Starting sparky-server on :$LISTEN_PORT..."
"$BIN" &
SERVER_PID=$!

BASE_URL="http://localhost:${LISTEN_PORT}"
BREAK_GLASS_URL="${BASE_URL}/login/break-glass"
echo "Waiting for the server to start listening..."
for _ in $(seq 1 50); do
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "error: sparky-server exited before it started listening" >&2; exit 1; }
  curl -s -o /dev/null "${BASE_URL}/login" && break
  sleep 0.2
done

# The break-glass sign-in page (this PR's own new GUI form) needs no
# console workaround anymore - just open it directly and let the operator
# type or paste the password into the real form.
CLIPBOARD_NOTE=""
# A clipboard tool being installed doesn't guarantee it can actually reach
# a clipboard right now (no DISPLAY/Wayland session - SSH, a headless box,
# a container) - with pipefail active, a failing xclip/wl-copy/pbcopy
# would otherwise abort the whole script over a non-essential convenience
# after the server is already up, so failures here are swallowed with
# || true rather than propagated.
if command -v xclip >/dev/null 2>&1; then
  printf '%s' "$DEV_PASSWORD" | xclip -selection clipboard 2>/dev/null && CLIPBOARD_NOTE=" (copied to your clipboard)" || true
elif command -v wl-copy >/dev/null 2>&1; then
  printf '%s' "$DEV_PASSWORD" | wl-copy 2>/dev/null && CLIPBOARD_NOTE=" (copied to your clipboard)" || true
elif command -v pbcopy >/dev/null 2>&1; then
  printf '%s' "$DEV_PASSWORD" | pbcopy 2>/dev/null && CLIPBOARD_NOTE=" (copied to your clipboard)" || true
fi

if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "$BREAK_GLASS_URL" >/dev/null 2>&1 &
elif command -v open >/dev/null 2>&1; then
  open "$BREAK_GLASS_URL" >/dev/null 2>&1 &
elif command -v wslview >/dev/null 2>&1; then
  wslview "$BREAK_GLASS_URL" >/dev/null 2>&1 &
else
  echo "Open ${BREAK_GLASS_URL} in your browser."
fi

echo
echo "sparky-server is running at ${BASE_URL}"
echo
echo "There's no AD/LDAP server here, so the regular login form won't"
echo "work. The break-glass sign-in page is already open - enter this"
echo "password${CLIPBOARD_NOTE}:"
echo
echo "  $DEV_PASSWORD"
echo
echo "Press Ctrl-C to stop the server and remove the Postgres container."
echo

wait "$SERVER_PID"
