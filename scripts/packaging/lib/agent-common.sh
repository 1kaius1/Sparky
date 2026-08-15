#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Shared sparky-agent install logic - see docs/AGENT.md Build and Install.
# Sourced by three different callers:
#   - scripts/packaging/postinstall.sh, from its *installed* package path
#     (nfpm reads script files at build time and embeds their contents into
#     the produced package's own control scripts, so this file is shipped as
#     real package content - see scripts/packaging/nfpm.yaml - rather than
#     referenced from a repo checkout that won't exist on the target host)
#   - scripts/packaging/postremove.sh, same reasoning, for the purge path
#   - scripts/install_agent.sh, directly from the extracted tarball
#
# The serviceloop account/GPU-group/model-storage-directory provisioning
# that used to live here moved into `sparky-agent setup` (agent/provision) -
# see PLANNING.md's 2026-08-07 Decisions Log entry for why: real go test
# coverage, and one implementation instead of duplicating useradd/usermod
# handling across bash's apt/dnf/tarball branches. Only the secrets-file
# step remains here - a different concern (materializing a config file
# template, not OS account/group state) that was never part of that
# migration.
#
# This file only defines functions - it does not execute anything on its
# own, and does not set shell options (that's each caller's own decision to
# make about its own script).

# ensure_secrets_file materializes /etc/sparky-agent/secrets.env from the
# template at $1, but only if it doesn't already exist - an upgrade (or a
# reinstall after a plain `remove`, which deliberately leaves this file in
# place - see scripts/packaging/postremove.sh) must never overwrite a real,
# already-configured secrets file with placeholder values.
ensure_secrets_file() {
    template="$1"
    install -d -m 0755 /etc/sparky-agent
    if [ ! -f /etc/sparky-agent/secrets.env ]; then
        install -m 0600 -o serviceloop -g serviceloop "$template" /etc/sparky-agent/secrets.env
    fi
}
