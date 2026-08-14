#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Shared sparky-agent install logic - the single implementation of the
# useradd/GPU-group/secrets-file steps every install method needs (see
# docs/AGENT.md Build and Install). Sourced by three different callers:
#   - scripts/packaging/postinstall.sh, from its *installed* package path
#     (nfpm reads script files at build time and embeds their contents into
#     the produced package's own control scripts, so this file is shipped as
#     real package content - see scripts/packaging/nfpm.yaml - rather than
#     referenced from a repo checkout that won't exist on the target host)
#   - scripts/packaging/postremove.sh, same reasoning, for the purge path
#   - scripts/install_agent.sh, directly from the extracted tarball
#
# This file only defines functions - it does not execute anything on its
# own, and does not set shell options (that's each caller's own decision to
# make about its own script).

# ensure_serviceloop_user creates the serviceloop system account if it
# doesn't already exist. Must be idempotent - this runs on every package
# install *and* every upgrade (see postinstall.sh), and re-running useradd
# on an existing account is a hard failure, not a no-op.
ensure_serviceloop_user() {
    if ! id -u serviceloop >/dev/null 2>&1; then
        useradd --system --no-create-home --shell /usr/sbin/nologin serviceloop
    fi
}

# ensure_gpu_group_membership joins serviceloop to whichever of video/render
# actually exists on this host. docs/AGENT.md has long flagged that the
# GPU-device-access group is distro/driver-dependent (video vs render) - not
# resolved here in favor of guessing one, since usermod -aG is idempotent
# and harmless to run against a group that turns out to be irrelevant on a
# given host, but silently skipping the group that *does* matter would
# leave the agent unable to see any GPU device node at all.
ensure_gpu_group_membership() {
    for grp in video render; do
        if getent group "$grp" >/dev/null 2>&1; then
            usermod -aG "$grp" serviceloop
        fi
    done
}

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
