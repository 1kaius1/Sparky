#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Shared sparky-server install logic - see CLAUDE.md Build and Run. Sourced
# by three different callers:
#   - scripts/packaging/postinstall_server.sh, from its *installed* package
#     path (nfpm reads script files at build time and embeds their contents
#     into the produced package's own control scripts, so this file is
#     shipped as real package content - see scripts/packaging/nfpm-server.yaml
#     - rather than referenced from a repo checkout that won't exist on the
#     target host)
#   - scripts/packaging/postremove_server.sh, same reasoning, for the purge
#     path
#   - scripts/install_server.sh, directly from the extracted tarball
#
# Unlike agent-common.sh, service-account provisioning stays here rather than
# moving into a Go subcommand: the server's account has no GPU-group
# membership or model-storage directory to manage, so a plain idempotent
# useradd is proportionate to the actual need.
#
# This file only defines functions - it does not execute anything on its
# own, and does not set shell options (that's each caller's own decision to
# make about its own script).

# ensure_service_account creates the unprivileged sparky system account if it
# doesn't already exist - no login shell, no home directory (this account
# only ever runs the server process under systemd, never an interactive
# session).
ensure_service_account() {
    if ! id -u sparky >/dev/null 2>&1; then
        useradd --system --no-create-home --shell /usr/sbin/nologin sparky
    fi
}

# ensure_secrets_file materializes /etc/sparky-server/secrets.env from the
# template at $1, but only if it doesn't already exist - an upgrade (or a
# reinstall after a plain `remove`, which deliberately leaves this file in
# place - see scripts/packaging/postremove_server.sh) must never overwrite a
# real, already-configured secrets file with placeholder values.
ensure_secrets_file() {
    template="$1"
    install -d -m 0755 /etc/sparky-server
    if [ ! -f /etc/sparky-server/secrets.env ]; then
        install -m 0600 -o sparky -g sparky "$template" /etc/sparky-server/secrets.env
    fi
}
