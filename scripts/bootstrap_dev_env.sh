#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Prepares a virgin Ubuntu/Debian x86_64 workstation to work on Sparky:
# installs the Go toolchain (matching go.mod's minimum version), the
# golang-migrate CLI, nfpm, and rootless-podman's real prerequisites, then
# proves each one actually works rather than just checking it's present -
# a real disposable Postgres container, a real `migrate up`, a real
# `go build`/`go test` pass, and a real arm64 cross-compile, matching
# CLAUDE.md's Database Setup recipe and scripts/build_packages.sh's own
# cross-compile step. Idempotent - safe to re-run to pick up where a prior
# run left off, or to re-verify later.
#
# Must be run as a normal user (not root) from inside a Sparky checkout -
# it needs sudo for OS package installs, but rootless podman and the Go
# toolchain both need to be set up as the real user, not root.

set -euo pipefail

if [ "$(id -u)" -eq 0 ]; then
    echo "bootstrap_dev_env.sh: run as a normal user, not root - sudo is invoked internally where needed" >&2
    exit 1
fi

if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
    echo "bootstrap_dev_env.sh: this script only supports Linux/x86_64" >&2
    exit 1
fi

# shellcheck source=/dev/null
. /etc/os-release
case "${ID:-}:${ID_LIKE:-}" in
    *debian*|*ubuntu*) ;;
    *)
        echo "bootstrap_dev_env.sh: this script only supports Debian/Ubuntu-family distros (found ID=${ID:-unknown})" >&2
        exit 1
        ;;
esac

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

if [ ! -f go.mod ] || [ ! -f CLAUDE.md ]; then
    echo "bootstrap_dev_env.sh: must be run from inside a Sparky checkout (scripts/bootstrap_dev_env.sh)" >&2
    exit 1
fi

step() { printf '\n==> %s\n' "$1"; }
ok() { printf '    OK: %s\n' "$1"; }

DB_CONTAINER_NAME="sparky-bootstrap-check-postgres"
DB_PORT="${DEV_DB_PORT:-5432}"
DB_URL="postgres://sparky:sparky@localhost:${DB_PORT}/sparky_dev?sslmode=disable"

cleanup() {
    command -v podman >/dev/null 2>&1 || return 0
    if podman container exists "$DB_CONTAINER_NAME" 2>/dev/null; then
        podman stop "$DB_CONTAINER_NAME" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

step "OS packages (podman + rootless prerequisites, git, curl, jq)"
sudo apt-get update -qq
sudo apt-get install -y --no-install-recommends \
    podman uidmap slirp4netns fuse-overlayfs dbus-user-session \
    git curl ca-certificates jq
ok "apt packages installed"

step "rootless podman subuid/subgid ranges"
if ! grep -q "^$(id -un):" /etc/subuid 2>/dev/null; then
    sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$(id -un)"
    ok "added subuid/subgid range for $(id -un) - log out and back in if podman still fails to run rootless"
else
    ok "subuid/subgid range already present for $(id -un)"
fi

go_required=$(sed -n 's/^go //p' go.mod | head -1)
step "Go toolchain (go.mod requires ${go_required}+)"
req_major=${go_required%%.*}
req_minor=${go_required#*.}

version_satisfies() {
    local have="$1" have_major have_minor
    have_major=${have%%.*}
    have_minor=${have#*.}
    have_minor=${have_minor%%.*}
    if [ "$have_major" -gt "$req_major" ]; then
        return 0
    elif [ "$have_major" -eq "$req_major" ] && [ "$have_minor" -ge "$req_minor" ]; then
        return 0
    fi
    return 1
}

have_go_version=""
if command -v /usr/local/go/bin/go >/dev/null 2>&1; then
    have_go_version=$(/usr/local/go/bin/go version | sed -n 's/^go version go\([0-9][0-9.]*\).*/\1/p')
fi

if [ -n "$have_go_version" ] && version_satisfies "$have_go_version"; then
    ok "/usr/local/go already has go${have_go_version}, satisfies ${go_required}+"
else
    step "  downloading latest stable Go release for linux/amd64"
    releases_json=$(curl -fsSL 'https://go.dev/dl/?mode=json')
    go_version=$(echo "$releases_json" | jq -r '[.[] | select(.stable==true)][0].version')
    go_ver_num=${go_version#go}
    if ! version_satisfies "$go_ver_num"; then
        echo "bootstrap_dev_env.sh: latest stable Go (${go_version}) does not satisfy go.mod's ${go_required}+ requirement - investigate before proceeding" >&2
        exit 1
    fi
    go_sha256=$(echo "$releases_json" | jq -r --arg v "$go_version" \
        '.[] | select(.version==$v) | .files[] | select(.os=="linux" and .arch=="amd64" and .kind=="archive") | .sha256')
    tmp_tarball=$(mktemp -t "${go_version}.linux-amd64.tar.gz.XXXXXX")
    curl -fsSL "https://go.dev/dl/${go_version}.linux-amd64.tar.gz" -o "$tmp_tarball"
    echo "${go_sha256}  ${tmp_tarball}" | sha256sum -c -
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$tmp_tarball"
    rm -f "$tmp_tarball"
    ok "installed ${go_version} to /usr/local/go"
fi

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
ok "using $(go version)"

step "persisting PATH for future shells (~/.bashrc)"
path_marker="# Added by Sparky scripts/bootstrap_dev_env.sh"
if ! grep -qF "$path_marker" "$HOME/.bashrc" 2>/dev/null; then
    {
        echo ""
        echo "$path_marker"
        echo 'export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"'
    } >> "$HOME/.bashrc"
    ok "appended to ~/.bashrc - open a new shell (or source ~/.bashrc) to pick it up"
else
    ok "~/.bashrc already updated"
fi

step "golang-migrate CLI"
if command -v migrate >/dev/null 2>&1 && migrate -version >/dev/null 2>&1; then
    ok "$(command -v migrate) ($(migrate -version 2>&1))"
else
    go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
    ok "installed to $(go env GOPATH)/bin/migrate ($(migrate -version 2>&1))"
fi

step "nfpm"
if command -v nfpm >/dev/null 2>&1; then
    ok "$(command -v nfpm)"
else
    go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
    ok "installed to $(go env GOPATH)/bin/nfpm"
fi

step "full repo build (go build ./...)"
go build ./...
ok "builds clean"

step "arm64 cross-compile smoke test (sparky-agent)"
tmp_arm64_bin=$(mktemp)
GOOS=linux GOARCH=arm64 go build -o "$tmp_arm64_bin" ./cmd/sparky-agent
file "$tmp_arm64_bin" | grep -q 'ARM aarch64'
rm -f "$tmp_arm64_bin"
ok "cross-compiles a real ARM aarch64 binary"

step "rootless podman smoke test"
podman run --rm docker.io/library/busybox true
ok "podman can pull and run a real container rootless"

step "disposable Postgres + migrate + full test suite"
podman run --rm -d \
    --name "$DB_CONTAINER_NAME" \
    -e POSTGRES_USER=sparky \
    -e POSTGRES_PASSWORD=sparky \
    -e POSTGRES_DB=sparky_dev \
    -p "${DB_PORT}:5432" \
    docker.io/library/postgres:16 >/dev/null

# The official postgres image restarts once during first-run initdb before
# the real instance starts - pg_isready alone can observe that transient
# restart window as ready, so wait for the log line twice instead (see
# CHANGELOG.md's scripts/dev-server.sh entry for the same fix).
until [[ "$(podman logs "$DB_CONTAINER_NAME" 2>&1 | grep -c "ready to accept connections" || true)" -ge 2 ]]; do
    sleep 0.5
done

migrate -path migrations/ -database "$DB_URL" up
ok "migrations applied to a real disposable Postgres instance"

DATABASE_URL="$DB_URL" go test ./...
ok "full test suite (unit + DATABASE_URL-gated integration tests) passes"

step "environment ready"
echo "    Go:      $(go version)"
echo "    migrate: $(migrate -version 2>&1)"
echo "    nfpm:    $(nfpm --version 2>&1 | grep GitVersion)"
echo "    podman:  $(podman --version)"
