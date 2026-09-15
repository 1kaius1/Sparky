#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm postinstall scriptlet (deb postinst / rpm %post) - see
# scripts/packaging/nfpm-server.yaml.
#
# Unlike sparky-agent's postinstall, this deliberately does NOT run any
# "setup"-style subcommand automatically: `sparky-server setup` is the
# interactive database/break-glass bootstrap wizard (CLAUDE.md First Run) -
# it requires DATABASE_URL to already be reachable and prompts interactively
# (no-echo password entry) for the break-glass credential, neither of which
# is available or appropriate during an unattended package install. Only OS-
# level provisioning (the service account, the secrets file skeleton)
# happens here; the database/application bootstrap stays a separate,
# explicitly-documented manual step, same as it already is today.
set -e

. /opt/sparky/share/sparky-server/server-common.sh

ensure_service_account
ensure_secrets_file /opt/sparky/share/sparky-server/secrets.env.template

systemctl daemon-reload
systemctl enable sparky-server >/dev/null 2>&1 || true

# Never auto-start on a fresh install - an unconfigured secrets.env would
# just crash-loop, and even a correctly-filled-in one still needs the
# database migrated and `sparky-server setup` run once before the app will
# serve normal routes. On a fresh install nothing is active yet, so this
# check is always false. On an upgrade, the old process is still running at
# this point (preremove_server.sh deliberately no-ops on upgrade rather than
# stopping the service first), so this correctly restarts it onto the new
# binary.
if systemctl is-active --quiet sparky-server 2>/dev/null; then
    systemctl restart sparky-server
else
    echo "sparky-server installed but not started."
    echo "Fill in /etc/sparky-server/secrets.env, then complete the database"
    echo "setup (createdb, migrate, sparky-server setup - see CLAUDE.md"
    echo "Database Setup and First Run), then run: sudo systemctl start sparky-server"
fi
