#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Repackages koboldcpp's own official pre-built Linux release binary into
# the exact tarball+.sha256 shape agent/enginetransfer's Executor expects to
# download - see docs/AGENT.md Engine binary provisioning and PLANNING.md's
# Decisions Log for why this is a repackage, not a from-source build like
# scripts/build_engine_release.sh. koboldcpp's real build pipeline
# bootstraps its own micromamba/conda Python environment (a pinned CUDA
# toolkit pulled from conda-forge, not whatever nvcc happens to be on the
# build machine), builds several .so variants via a hand-written Makefile,
# then bundles a Python interpreter + those .so files + koboldcpp.py into
# one PyInstaller-produced executable - confirmed by reading koboldcpp's
# own GitHub Actions workflows and koboldcpp.sh directly, not assumed. That
# is a fundamentally different, much heavier process than llama.cpp's plain
# CMake build, and not something this repo re-implements. Their own
# published koboldcpp-linux-x64 asset already IS that finished, self-
# contained executable - no separate shared libraries to track alongside
# it, unlike llama.cpp's binary+lib*.so shape - so this script downloads it,
# verifies it against the SHA-256 digest GitHub's own Release API already
# recorded for it at upload time, and repackages it into the same tarball
# shape scripts/publish_engine_release.sh already knows how to publish,
# unchanged.
#
# amd64 only - koboldcpp has no official Linux ARM64 build. Their only
# ARM64 CI job (test-unofficial-arm64.yaml) is explicitly labeled
# unofficial, is CPU-only (no CUDA), and cross-compiles rather than builds
# natively - confirmed by reading that workflow directly. A koboldcpp
# bundle from this script can only ever serve Sparky's amd64 nodes.
#
# Required env vars:
#   ENGINE_VERSION - the exact upstream koboldcpp release tag to repackage,
#                    e.g. "v1.119" - never a Sparky app version, same
#                    "always the real upstream tag" convention
#                    build_engine_release.sh's own ENGINE_VERSION already
#                    establishes. Becomes both the download source and this
#                    bundle's own version string.
#
# Optional env vars:
#   TARGET_ARCH - must be "amd64" if set at all (default: amd64) - see the
#                 ARM64 note above. Accepted as a var, not hardcoded
#                 silently, so a future upstream ARM64 release only needs
#                 this constraint relaxed here, not a new script.
#   OUTPUT_DIR  - where the final .tar.xz + .sha256 land (default:
#                 dist/engine-release, the same default
#                 build_engine_release.sh uses, so
#                 scripts/publish_engine_release.sh finds it without any
#                 extra configuration).
#   WORK_DIR    - scratch space for the download (default: a fresh
#                 directory under dist/build/).
#
# Requires: curl, jq (to read the asset digest GitHub's Release API already
# computed, rather than hand-rolling fragile JSON parsing), sha256sum, tar.
set -e

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

require_tool() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "repackage_koboldcpp_release.sh: $1 not found on PATH - see this script's own header for requirements" >&2
        exit 1
    fi
}
for tool in curl jq sha256sum tar; do
    require_tool "$tool"
done

if [ -z "${ENGINE_VERSION:-}" ]; then
    echo "repackage_koboldcpp_release.sh: missing required env var ENGINE_VERSION - see this script's own header" >&2
    exit 1
fi

TARGET_ARCH="${TARGET_ARCH:-amd64}"
if [ "$TARGET_ARCH" != "amd64" ]; then
    echo "repackage_koboldcpp_release.sh: TARGET_ARCH must be amd64 - koboldcpp has no official Linux ARM64 build (see this script's own header), got: $TARGET_ARCH" >&2
    exit 1
fi

koboldcpp_repo="LostRuins/koboldcpp"
upstream_asset="koboldcpp-linux-x64"

OUTPUT_DIR="${OUTPUT_DIR:-dist/engine-release}"
WORK_DIR="${WORK_DIR:-dist/build/koboldcpp-${ENGINE_VERSION}-${TARGET_ARCH}}"

echo "==> repackaging koboldcpp $ENGINE_VERSION for $TARGET_ARCH"

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR" "$OUTPUT_DIR"
# Resolved to an absolute path once, so the archiving step below (which cds
# into the staging dir) can reference it safely regardless of whether the
# caller passed OUTPUT_DIR as relative or already-absolute - same reasoning
# as build_engine_release.sh's own identical resolution.
OUTPUT_DIR=$(cd "$OUTPUT_DIR" && pwd)

echo "==> looking up $upstream_asset's published digest on $koboldcpp_repo @ $ENGINE_VERSION"
if ! release_json=$(curl -sf "https://api.github.com/repos/$koboldcpp_repo/releases/tags/$ENGINE_VERSION"); then
    echo "repackage_koboldcpp_release.sh: failed to fetch release metadata for tag $ENGINE_VERSION from $koboldcpp_repo - check the tag actually exists (https://github.com/$koboldcpp_repo/releases)" >&2
    exit 1
fi

want_digest=$(printf '%s' "$release_json" | jq -r --arg name "$upstream_asset" '.assets[] | select(.name == $name) | .digest // empty')
if [ -z "$want_digest" ]; then
    echo "repackage_koboldcpp_release.sh: no asset named $upstream_asset found on release $ENGINE_VERSION of $koboldcpp_repo - check the release still publishes an asset by that name (upstream has renamed release assets before, re-check https://github.com/$koboldcpp_repo/releases/tag/$ENGINE_VERSION)" >&2
    exit 1
fi
want_digest=${want_digest#sha256:}

download_url=$(printf '%s' "$release_json" | jq -r --arg name "$upstream_asset" '.assets[] | select(.name == $name) | .browser_download_url')

echo "==> downloading $download_url"
curl -sfL -o "$WORK_DIR/$upstream_asset" "$download_url"

echo "==> verifying against GitHub's own recorded digest"
got_digest=$(sha256sum "$WORK_DIR/$upstream_asset" | awk '{print $1}')
if [ "$got_digest" != "$want_digest" ]; then
    echo "repackage_koboldcpp_release.sh: checksum mismatch - the downloaded file does not match the digest GitHub's own Release API recorded for it (want $want_digest, got $got_digest). Refusing to repackage a file that doesn't match what upstream published." >&2
    exit 1
fi

echo "==> packaging"
stage="$WORK_DIR/stage"
rm -rf "$stage"
mkdir -p "$stage"
# Renamed to a stable, engine-generic name inside the tarball -
# koboldcpp-linux-x64 already encodes the engine and architecture, which
# would be redundant once extracted into this project's own
# $SPARKY_ENGINE_INSTALL_PATH/koboldcpp/<version>/ layout (docs/AGENT.md
# Engine binary provisioning) - matching llama.cpp's own recipe, which
# packages a plain "llama-server", not "llama-server-linux-amd64".
cp "$WORK_DIR/$upstream_asset" "$stage/koboldcpp"
chmod +x "$stage/koboldcpp"

asset_name="koboldcpp-${ENGINE_VERSION}-${TARGET_ARCH}.tar.xz"
echo "==> archiving $asset_name"
# Run from inside the staging dir, archiving "." - same reasoning as
# build_engine_release.sh's own identical step: agent/enginetransfer's
# Executor extracts straight into the version directory it creates, with no
# path stripping, so a top-level wrapper folder here would land the binary
# one directory too deep.
( cd "$stage" && tar -cJf "$OUTPUT_DIR/$asset_name" . )

echo "==> checksumming"
( cd "$OUTPUT_DIR" && sha256sum "$asset_name" > "$asset_name.sha256" )

echo "==> done"
ls -la "$OUTPUT_DIR/$asset_name" "$OUTPUT_DIR/$asset_name.sha256"
