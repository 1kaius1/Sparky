#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds sparky-agent's .deb, .rpm, and tarball artifacts for amd64 and
# arm64 into dist/ (gitignored) - see docs/AGENT.md Build and Install and
# PLANNING.md Decisions Log for why nfpm. Maintainer-facing only; not
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
done

echo "==> done - artifacts in dist/"
ls -la dist/*.deb dist/*.rpm dist/*.tar.gz
