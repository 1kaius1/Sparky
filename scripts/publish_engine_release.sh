#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Publishes an already-built engine-release bundle (produced by
# scripts/build_engine_release.sh) as a GitHub Release on Sparky's own repo,
# matching the exact contract agent/enginetransfer's Executor expects to
# download - see docs/AGENT.md Engine binary provisioning and PLANNING.md's
# Decisions Log for the full design. Deliberately kept separate from the
# build script: build can run anywhere without repo-write credentials;
# publish is the one step that touches Sparky's GitHub Releases and should
# be a distinct, deliberate action. Maintainer-facing today; a future
# scheduled pipeline is expected to invoke this same script unchanged.
#
# Required env vars:
#   ENGINE_TYPE    - e.g. "llamacpp" - identifies which bundle files to look
#                    for and which release tag/notes to use.
#   ENGINE_VERSION - the upstream release tag this bundle was built from,
#                    e.g. "b4610" - becomes the GitHub Release's own tag,
#                    exactly matching what agent/enginetransfer's Executor
#                    requests by URL. Never a Sparky app version.
#
# Optional env vars:
#   BUNDLE_DIR    - where to find the built files (default:
#                   dist/engine-release, matching build_engine_release.sh's
#                   own OUTPUT_DIR default).
#   ALLOW_PARTIAL - set to "1" to publish even if only one architecture's
#                   bundle is present. Fails closed by default: a release
#                   missing one architecture's asset leaves that arch's
#                   agents unable to provision this version at all.
#
# Requires: gh (authenticated - `gh auth status`), sha256sum.
set -e

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

# Must match agent/enginetransfer's own hardcoded releaseOwner/releaseRepo
# constants exactly - that is the one repo its Executor will ever download
# release assets from.
release_repo="1kaius1/Sparky"

require_tool() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "publish_engine_release.sh: $1 not found on PATH - see this script's own header for requirements" >&2
        exit 1
    fi
}
require_tool gh
require_tool sha256sum

if ! gh auth status >/dev/null 2>&1; then
    echo "publish_engine_release.sh: gh is not authenticated - run 'gh auth login' first" >&2
    exit 1
fi

missing=""
for var in ENGINE_TYPE ENGINE_VERSION; do
    eval "val=\${$var:-}"
    if [ -z "$val" ]; then
        missing="$missing $var"
    fi
done
if [ -n "$missing" ]; then
    echo "publish_engine_release.sh: missing required env var(s):$missing - see this script's own header" >&2
    exit 1
fi

BUNDLE_DIR="${BUNDLE_DIR:-dist/engine-release}"
if [ ! -d "$BUNDLE_DIR" ]; then
    echo "publish_engine_release.sh: BUNDLE_DIR $BUNDLE_DIR does not exist - build a bundle with scripts/build_engine_release.sh first" >&2
    exit 1
fi

echo "==> looking for $ENGINE_TYPE $ENGINE_VERSION bundles in $BUNDLE_DIR"
found_archs=""
files_to_upload=""
for tarball in "$BUNDLE_DIR/${ENGINE_TYPE}-${ENGINE_VERSION}-"*.tar.xz; do
    [ -e "$tarball" ] || continue

    base=$(basename "$tarball")
    arch=${base#"${ENGINE_TYPE}-${ENGINE_VERSION}-"}
    arch=${arch%.tar.xz}

    checksum="$tarball.sha256"
    if [ ! -f "$checksum" ]; then
        echo "publish_engine_release.sh: $tarball has no sibling checksum file ($checksum) - refusing to publish" >&2
        exit 1
    fi

    echo "==> verifying checksum for $base"
    if ! ( cd "$BUNDLE_DIR" && sha256sum -c "$(basename "$checksum")" >/dev/null ); then
        echo "publish_engine_release.sh: checksum mismatch for $tarball - the local files are not internally consistent, refusing to publish" >&2
        exit 1
    fi

    found_archs="$found_archs $arch"
    files_to_upload="$files_to_upload $tarball $checksum"
done

if [ -z "$found_archs" ]; then
    echo "publish_engine_release.sh: no bundles found in $BUNDLE_DIR matching ${ENGINE_TYPE}-${ENGINE_VERSION}-*.tar.xz" >&2
    exit 1
fi
echo "==> found bundle(s) for:$found_archs"

has_amd64=0
has_arm64=0
case " $found_archs " in *" amd64 "*) has_amd64=1 ;; esac
case " $found_archs " in *" arm64 "*) has_arm64=1 ;; esac

if [ "$has_amd64" -eq 0 ] || [ "$has_arm64" -eq 0 ]; then
    if [ "${ALLOW_PARTIAL:-0}" != "1" ]; then
        echo "publish_engine_release.sh: only found arch(es):$found_archs - a release missing one architecture leaves that arch's agents unable to provision this version. Set ALLOW_PARTIAL=1 to publish anyway." >&2
        exit 1
    fi
    echo "==> WARNING: publishing a partial release (only:$found_archs) because ALLOW_PARTIAL=1 was set"
fi

if gh release view "$ENGINE_VERSION" --repo "$release_repo" >/dev/null 2>&1; then
    echo "==> release $ENGINE_VERSION already exists on $release_repo - uploading (--clobber)"
    # shellcheck disable=SC2086
    gh release upload "$ENGINE_VERSION" $files_to_upload --clobber --repo "$release_repo"
else
    echo "==> creating release $ENGINE_VERSION on $release_repo"
    # shellcheck disable=SC2086
    gh release create "$ENGINE_VERSION" $files_to_upload \
        --repo "$release_repo" \
        --title "$ENGINE_TYPE $ENGINE_VERSION" \
        --notes "Compiled $ENGINE_TYPE release bundle(s) for Sparky's agent/enginetransfer - built via scripts/build_engine_release.sh. Architectures:$found_archs."
fi

echo "==> done"
gh release view "$ENGINE_VERSION" --repo "$release_repo" --json url,assets \
    --jq '"url: " + .url, "assets: " + ([.assets[].name] | join(", "))'
