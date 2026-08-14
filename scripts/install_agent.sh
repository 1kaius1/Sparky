#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sparky-agent tarball installer - see docs/AGENT.md Build and Install. Run
# from inside the extracted tarball (expects bin/sparky-agent,
# lib/agent-common.sh, sparky-agent.service, secrets.env.template, and
# uninstall_agent.sh alongside this script - see scripts/build_packages.sh
# for how the tarball is assembled).
#
# Unlike the .deb/.rpm packages, this path is not tracked by any package
# manager, so the systemd unit installs to /etc/systemd/system/ (the
# correct location for a locally-installed, non-package-managed unit - see
# deploy/systemd/sparky-agent.service's own doc comment) rather than
# /usr/lib/systemd/system/.
set -e

if [ "$(id -u)" -ne 0 ]; then
    echo "sparky-agent: install_agent.sh must be run as root (sudo)" >&2
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "sparky-agent: systemctl not found - this installer requires systemd" >&2
    exit 1
fi

script_dir=$(cd "$(dirname "$0")" && pwd)

. "$script_dir/lib/agent-common.sh"

install -d -m 0755 /opt/sparky/bin
install -m 0755 -o root -g root "$script_dir/bin/sparky-agent" /opt/sparky/bin/sparky-agent
ln -sf /opt/sparky/bin/sparky-agent /usr/local/bin/sparky-agent

install -m 0644 -o root -g root "$script_dir/sparky-agent.service" /etc/systemd/system/sparky-agent.service

# Persist a copy of the shared library and the uninstaller so a later
# `sudo /opt/sparky/share/sparky-agent/uninstall_agent.sh` works even if
# this extracted tarball directory no longer exists.
install -d -m 0755 /opt/sparky/share/sparky-agent
install -m 0755 -o root -g root "$script_dir/lib/agent-common.sh" /opt/sparky/share/sparky-agent/agent-common.sh
install -m 0644 -o root -g root "$script_dir/secrets.env.template" /opt/sparky/share/sparky-agent/secrets.env.template
install -m 0755 -o root -g root "$script_dir/uninstall_agent.sh" /opt/sparky/share/sparky-agent/uninstall_agent.sh

ensure_serviceloop_user
ensure_gpu_group_membership
ensure_secrets_file /opt/sparky/share/sparky-agent/secrets.env.template

systemctl daemon-reload
systemctl enable sparky-agent >/dev/null 2>&1 || true

# Same reasoning as scripts/packaging/postinstall.sh: never auto-start a
# fresh install, but safely restart an already-running agent if this script
# is being re-run to install an upgrade.
if systemctl is-active --quiet sparky-agent 2>/dev/null; then
    systemctl restart sparky-agent
    echo "sparky-agent upgraded and restarted."
else
    echo "sparky-agent installed but not started."
    echo "Fill in /etc/sparky-agent/secrets.env, then run: sudo systemctl start sparky-agent"
fi
