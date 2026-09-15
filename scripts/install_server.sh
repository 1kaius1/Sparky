#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sparky-server tarball installer - see CLAUDE.md Build and Run. Run from
# inside the extracted tarball (expects bin/sparky-server,
# lib/server-common.sh, sparky-server.service, secrets.env.template, and
# uninstall_server.sh alongside this script - see scripts/build_packages.sh
# for how the tarball is assembled).
#
# Unlike the .deb/.rpm packages, this path is not tracked by any package
# manager, so the systemd unit installs to /etc/systemd/system/ (the
# correct location for a locally-installed, non-package-managed unit - see
# deploy/systemd/sparky-server.service's own doc comment) rather than
# /usr/lib/systemd/system/.
#
# This installs the OS-level pieces only (binary, unit, service account,
# secrets.env skeleton) - it deliberately does not run `sparky-server setup`
# or any database step, since those require DATABASE_URL to already be
# reachable and prompt interactively. Complete createdb/migrate/
# sparky-server setup yourself afterward, per CLAUDE.md Database Setup and
# First Run.
set -e

if [ "$(id -u)" -ne 0 ]; then
    echo "sparky-server: install_server.sh must be run as root (sudo)" >&2
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "sparky-server: systemctl not found - this installer requires systemd" >&2
    exit 1
fi

script_dir=$(cd "$(dirname "$0")" && pwd)

. "$script_dir/lib/server-common.sh"

install -d -m 0755 /opt/sparky/bin
install -m 0755 -o root -g root "$script_dir/bin/sparky-server" /opt/sparky/bin/sparky-server
ln -sf /opt/sparky/bin/sparky-server /usr/local/bin/sparky-server

install -m 0644 -o root -g root "$script_dir/sparky-server.service" /etc/systemd/system/sparky-server.service

# Persist a copy of the shared library and the uninstaller so a later
# `sudo /opt/sparky/share/sparky-server/uninstall_server.sh` works even if
# this extracted tarball directory no longer exists.
install -d -m 0755 /opt/sparky/share/sparky-server
install -m 0755 -o root -g root "$script_dir/lib/server-common.sh" /opt/sparky/share/sparky-server/server-common.sh
install -m 0644 -o root -g root "$script_dir/secrets.env.template" /opt/sparky/share/sparky-server/secrets.env.template
install -m 0755 -o root -g root "$script_dir/uninstall_server.sh" /opt/sparky/share/sparky-server/uninstall_server.sh

ensure_service_account
ensure_secrets_file /opt/sparky/share/sparky-server/secrets.env.template

systemctl daemon-reload
systemctl enable sparky-server >/dev/null 2>&1 || true

# Same reasoning as scripts/packaging/postinstall_server.sh: never auto-start
# a fresh install, but safely restart an already-running server if this
# script is being re-run to install an upgrade.
if systemctl is-active --quiet sparky-server 2>/dev/null; then
    systemctl restart sparky-server
    echo "sparky-server upgraded and restarted."
else
    echo "sparky-server installed but not started."
    echo "Fill in /etc/sparky-server/secrets.env, then complete the database"
    echo "setup (createdb, migrate, sparky-server setup - see CLAUDE.md"
    echo "Database Setup and First Run), then run: sudo systemctl start sparky-server"
fi
