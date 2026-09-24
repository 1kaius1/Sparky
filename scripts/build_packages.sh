#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds .deb, .rpm, and tarball artifacts for both sparky-agent and
# sparky-server, for amd64 and arm64, into dist/ (gitignored) - see
# docs/AGENT.md Build and Install (agent), CLAUDE.md Build and Run (server),
# and PLANNING.md Decisions Log for why nfpm. Maintainer-facing only; not
# wired into any CI - run locally before cutting a release.
#
# Requires: go, nfpm (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest -
# a build-time tool only, same category as this repo's own documented
# golang-migrate install; it is never imported into go.mod).
set -e

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

if ! command -v nfpm >/dev/null 2>&1; then
    echo "build_packages.sh: nfpm not found on PATH - see this script's own header for the install command" >&2
    exit 1
fi

version=$(cat VERSION)
gopath=$(go env GOPATH)

rm -rf dist
mkdir -p dist/build

for arch in amd64 arm64; do
    echo "==> building sparky-agent linux/$arch"
    GOOS=linux GOARCH="$arch" go build -o "dist/build/sparky-agent-linux-$arch" ./cmd/sparky-agent

    # nfpm.yaml's contents[].src entries are plain repo-root-relative paths -
    # nfpm does not expand ${VAR} inside contents[].src the way it does for
    # top-level fields like arch/version (confirmed empirically, not
    # assumed), so the per-arch binary is staged at a fixed name here
    # instead of being referenced by an arch-suffixed path in the config.
    cp "dist/build/sparky-agent-linux-$arch" dist/build/sparky-agent

    echo "==> packaging .deb ($arch)"
    ARCH="$arch" VERSION="$version" nfpm pkg --config scripts/packaging/nfpm.yaml --packager deb --target dist/

    echo "==> packaging .rpm ($arch)"
    ARCH="$arch" VERSION="$version" nfpm pkg --config scripts/packaging/nfpm.yaml --packager rpm --target dist/

    echo "==> assembling tarball ($arch)"
    tarball_root="dist/build/tarball-$arch"
    rm -rf "$tarball_root"
    mkdir -p "$tarball_root/bin" "$tarball_root/lib"
    cp "dist/build/sparky-agent-linux-$arch" "$tarball_root/bin/sparky-agent"
    cp scripts/packaging/lib/agent-common.sh "$tarball_root/lib/agent-common.sh"
    cp scripts/install_agent.sh "$tarball_root/install_agent.sh"
    cp scripts/uninstall_agent.sh "$tarball_root/uninstall_agent.sh"
    cp deploy/systemd/sparky-agent.service "$tarball_root/sparky-agent.service"
    cp deploy/secrets.env.template "$tarball_root/secrets.env.template"
    chmod +x "$tarball_root/bin/sparky-agent" "$tarball_root/lib/agent-common.sh" \
        "$tarball_root/install_agent.sh" "$tarball_root/uninstall_agent.sh"

    tar -C dist/build -czf "dist/sparky-agent-$version-linux-$arch.tar.gz" "tarball-$arch"

    echo "==> building sparky-server linux/$arch"
    GOOS=linux GOARCH="$arch" go build -o "dist/build/sparky-server-linux-$arch" ./cmd/sparky-server

    # Same "stage at a fixed non-arch-suffixed path" reasoning as the agent
    # build above - see nfpm-server.yaml's own doc comment.
    cp "dist/build/sparky-server-linux-$arch" dist/build/sparky-server

    echo "==> building migrate linux/$arch (bundled for the optional local-database flow)"
    # `go install` cross-compiling honors GOOS/GOARCH like `go build`, but its
    # output location depends on whether the target matches the build host's
    # own: a genuine cross-build lands at $GOPATH/bin/<goos>_<goarch>/migrate,
    # while a same-arch build lands at the unqualified $GOPATH/bin/migrate
    # (confirmed empirically, not assumed) - so check both rather than assume
    # one. This never touches this repo's own go.mod/go.sum: `go install
    # pkg@version` runs outside module mode entirely.
    GOOS=linux GOARCH="$arch" go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
    if [ -f "${gopath}/bin/linux_${arch}/migrate" ]; then
        cp "${gopath}/bin/linux_${arch}/migrate" "dist/build/migrate-linux-$arch"
    elif [ -f "${gopath}/bin/migrate" ]; then
        cp "${gopath}/bin/migrate" "dist/build/migrate-linux-$arch"
    else
        echo "build_packages.sh: could not locate a freshly-installed migrate binary for linux/$arch" >&2
        exit 1
    fi
    cp "dist/build/migrate-linux-$arch" dist/build/migrate

    echo "==> packaging sparky-server .deb ($arch)"
    ARCH="$arch" VERSION="$version" nfpm pkg --config scripts/packaging/nfpm-server.yaml --packager deb --target dist/

    echo "==> packaging sparky-server .rpm ($arch)"
    ARCH="$arch" VERSION="$version" nfpm pkg --config scripts/packaging/nfpm-server.yaml --packager rpm --target dist/

    echo "==> assembling sparky-server tarball ($arch)"
    server_tarball_root="dist/build/server-tarball-$arch"
    rm -rf "$server_tarball_root"
    mkdir -p "$server_tarball_root/bin" "$server_tarball_root/lib"
    cp "dist/build/sparky-server-linux-$arch" "$server_tarball_root/bin/sparky-server"
    cp "dist/build/migrate-linux-$arch" "$server_tarball_root/migrate"
    cp -r migrations "$server_tarball_root/migrations"
    cp scripts/packaging/lib/server-common.sh "$server_tarball_root/lib/server-common.sh"
    cp scripts/packaging/lib/server-db-setup.sh "$server_tarball_root/lib/server-db-setup.sh"
    cp scripts/packaging/lib/run-with-secrets-env.sh "$server_tarball_root/lib/run-with-secrets-env.sh"
    cp scripts/install_server.sh "$server_tarball_root/install_server.sh"
    cp scripts/uninstall_server.sh "$server_tarball_root/uninstall_server.sh"
    cp deploy/systemd/sparky-server.service "$server_tarball_root/sparky-server.service"
    cp deploy/systemd/sparky-local-postgres.service "$server_tarball_root/sparky-local-postgres.service"
    cp .env.example "$server_tarball_root/secrets.env.template"
    chmod +x "$server_tarball_root/bin/sparky-server" "$server_tarball_root/migrate" \
        "$server_tarball_root/lib/server-common.sh" "$server_tarball_root/lib/server-db-setup.sh" \
        "$server_tarball_root/lib/run-with-secrets-env.sh" \
        "$server_tarball_root/install_server.sh" "$server_tarball_root/uninstall_server.sh"

    tar -C dist/build -czf "dist/sparky-server-$version-linux-$arch.tar.gz" "server-tarball-$arch"
done

echo "==> done - artifacts in dist/"
ls -la dist/*.deb dist/*.rpm dist/*.tar.gz
