#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds and packages one engine-release bundle (a checksummed .tar.xz)
# matching the exact contract agent/enginetransfer's Executor expects to
# download and install - see docs/AGENT.md Engine binary provisioning and
# PLANNING.md's Decisions Log for the full design. Maintainer-facing today;
# not wired into any CI - a future scheduled pipeline is expected to invoke
# this same script unchanged, per that Decisions Log entry.
#
# Engine-specific knowledge (upstream repo, CMake flags, output binary/lib
# names) lives in scripts/packaging/engine_recipes/<ENGINE_TYPE>.sh, sourced
# below - this script itself has no llama.cpp-specific logic, so adding a
# koboldcpp recipe later needs no change here.
#
# Required env vars:
#   ENGINE_TYPE          - e.g. "llamacpp" - selects the recipe file.
#   ENGINE_VERSION        - the exact upstream release tag to build, e.g.
#                          "b4610". Checked out via `git clone --branch`, so
#                          it must be a real tag, not a branch or commit SHA.
#   TARGET_ARCH           - "amd64" or "arm64" (Go's runtime.GOARCH naming,
#                          NOT Debian's x86_64/aarch64) - used verbatim in
#                          the output filename, since agent/enginetransfer's
#                          assetName() embeds runtime.GOARCH literally.
#   CUDA_ARCHITECTURES    - the CMAKE_CUDA_ARCHITECTURES value for this
#                          build, e.g. "86;89". This is fleet-hardware
#                          policy (which real GPUs need to run the result),
#                          not an engine-source-code fact, so it is supplied
#                          by the caller rather than hardcoded in a recipe -
#                          confirm real values via
#                          `nvidia-smi --query-gpu=compute_cap --format=csv`
#                          on real target hardware before relying on this.
#
# Optional env vars:
#   OUTPUT_DIR - where the final .tar.xz + .sha256 land (default:
#                dist/engine-release, gitignored via dist/).
#   WORK_DIR   - scratch space for the clone and CMake build tree (default:
#                a fresh directory under dist/build/).
#   BUILD_JOBS - parallelism for the build step (default: nproc).
#
# Requires: git, cmake, a generator (ninja preferred, falls back to the
# default Unix Makefiles generator if ninja isn't on PATH), sha256sum, tar
# with xz support, and nvcc (every recipe today enables CUDA - see each
# recipe's own comments).
set -e

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

require_tool() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "build_engine_release.sh: $1 not found on PATH - see this script's own header for requirements" >&2
        exit 1
    fi
}

for tool in git cmake sha256sum tar nvcc; do
    require_tool "$tool"
done

missing=""
for var in ENGINE_TYPE ENGINE_VERSION TARGET_ARCH CUDA_ARCHITECTURES; do
    eval "val=\${$var:-}"
    if [ -z "$val" ]; then
        missing="$missing $var"
    fi
done
if [ -n "$missing" ]; then
    echo "build_engine_release.sh: missing required env var(s):$missing - see this script's own header" >&2
    exit 1
fi

case "$TARGET_ARCH" in
    amd64|arm64) ;;
    *)
        echo "build_engine_release.sh: TARGET_ARCH must be amd64 or arm64 (Go's runtime.GOARCH naming), got: $TARGET_ARCH" >&2
        exit 1
        ;;
esac

# This script builds natively - there is no cross-compilation toolchain here,
# so TARGET_ARCH must match the machine actually running it, or the output
# would be silently mislabeled (e.g. a real amd64 binary shipped as
# "...-arm64.tar.xz"). Run this on a real machine of the target architecture
# instead - see PLANNING.md's Decisions Log for why (native builds avoid the
# fragile cross-compilation toolchain problem entirely).
case "$(uname -m)" in
    x86_64) host_arch=amd64 ;;
    aarch64|arm64) host_arch=arm64 ;;
    *) host_arch="$(uname -m)" ;;
esac
if [ "$host_arch" != "$TARGET_ARCH" ]; then
    echo "build_engine_release.sh: TARGET_ARCH=$TARGET_ARCH but this machine is $host_arch ($(uname -m)) - this script builds natively, it does not cross-compile. Run it on a real $TARGET_ARCH machine instead." >&2
    exit 1
fi

recipe="scripts/packaging/engine_recipes/${ENGINE_TYPE}.sh"
if [ ! -f "$recipe" ]; then
    echo "build_engine_release.sh: no recipe at $recipe for ENGINE_TYPE=$ENGINE_TYPE" >&2
    exit 1
fi
# shellcheck source=/dev/null
. "$recipe"

OUTPUT_DIR="${OUTPUT_DIR:-dist/engine-release}"
WORK_DIR="${WORK_DIR:-dist/build/${ENGINE_TYPE}-${ENGINE_VERSION}-${TARGET_ARCH}}"
BUILD_JOBS="${BUILD_JOBS:-$(nproc 2>/dev/null || echo 4)}"

echo "==> building $ENGINE_TYPE $ENGINE_VERSION for $TARGET_ARCH (CUDA_ARCHITECTURES=$CUDA_ARCHITECTURES)"

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR" "$OUTPUT_DIR"
# Resolved to an absolute path once, so later subshells that cd elsewhere
# (the archiving step below) can reference it safely regardless of whether
# the caller passed OUTPUT_DIR as relative or already-absolute.
OUTPUT_DIR=$(cd "$OUTPUT_DIR" && pwd)

echo "==> cloning $recipe_repo_url @ $ENGINE_VERSION"
git clone --branch "$ENGINE_VERSION" --depth 1 "$recipe_repo_url" "$WORK_DIR/src"

echo "==> configuring"
generator_flag=""
if command -v ninja >/dev/null 2>&1; then
    generator_flag="-G Ninja"
fi
# shellcheck disable=SC2086
cmake -S "$WORK_DIR/src" -B "$WORK_DIR/build" $generator_flag $recipe_cmake_flags \
    -DCMAKE_CUDA_ARCHITECTURES="$CUDA_ARCHITECTURES"

echo "==> building (jobs=$BUILD_JOBS)"
cmake --build "$WORK_DIR/build" -j "$BUILD_JOBS" --config Release

echo "==> packaging"
stage="$WORK_DIR/stage"
rm -rf "$stage"
mkdir -p "$stage"

primary_path=$(find "$WORK_DIR/build" -type f -name "$recipe_primary_binary" 2>/dev/null | head -n1)
if [ -z "$primary_path" ]; then
    echo "build_engine_release.sh: could not find $recipe_primary_binary anywhere under $WORK_DIR/build - the recipe's expected binary name or the engine's CMake output layout may have changed" >&2
    exit 1
fi
cp "$primary_path" "$stage/$recipe_primary_binary"
chmod +x "$stage/$recipe_primary_binary"

# Shared libraries are packaged best-effort - a matching recipe glob with no
# hits is not a failure (e.g. a future statically-linked recipe variant).
find "$WORK_DIR/build" -type f -name "$recipe_lib_glob" 2>/dev/null | while read -r lib; do
    cp "$lib" "$stage/$(basename "$lib")"
done

asset_name="${ENGINE_TYPE}-${ENGINE_VERSION}-${TARGET_ARCH}.tar.xz"
echo "==> archiving $asset_name"
# Run from inside the staging dir, archiving "." - this is the single most
# important correctness detail: agent/enginetransfer's Executor extracts
# straight into the version directory it creates, with no path stripping,
# so a top-level wrapper folder here would land llama-server one directory
# too deep and break SPARKY_LLAMACPP_BINARY_PATH.
( cd "$stage" && tar -cJf "$OUTPUT_DIR/$asset_name" . )

echo "==> checksumming"
( cd "$OUTPUT_DIR" && sha256sum "$asset_name" > "$asset_name.sha256" )

echo "==> done"
ls -la "$OUTPUT_DIR/$asset_name" "$OUTPUT_DIR/$asset_name.sha256"
