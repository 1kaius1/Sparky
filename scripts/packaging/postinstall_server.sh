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
#
# An optional local Postgres database IS provisioned here, but only if the
# operator opted in by setting SPARKY_INSTALL_LOCAL_DB=podman or
# SPARKY_INSTALL_LOCAL_DB=native before running apt/dnf install - postinstall
# scripts run unattended, so there's no interactive prompt equivalent to
# install_server.sh's own (CLAUDE.md documents the exact invocation). This is
# still opt-in, not automatic-by-default: an unset SPARKY_INSTALL_LOCAL_DB
# behaves exactly as if this paragraph didn't exist. See
# scripts/packaging/lib/server-db-setup.sh for what it actually does -
# database creation/migration, never `sparky-server setup` itself.
set -e

. /opt/sparky/share/sparky-server/server-common.sh

ensure_service_account
ensure_secrets_file /opt/sparky/share/sparky-server/secrets.env.template

if [ -n "${SPARKY_INSTALL_LOCAL_DB:-}" ]; then
    . /opt/sparky/share/sparky-server/server-db-setup.sh
    setup_local_database "$SPARKY_INSTALL_LOCAL_DB" /opt/sparky/share/sparky-server
fi

systemctl daemon-reload
systemctl enable sparky-server >/dev/null 2>&1 || true

# Never auto-start on a fresh install - an unconfigured secrets.env would
# just crash-loop, and even a correctly-filled-in one still needs
# `sparky-server setup` run once before the app will serve normal routes
# (the database itself may already be ready, above). On a fresh install
# nothing is active yet, so this check is always false. On an upgrade, the
# old process is still running at this point (preremove_server.sh
# deliberately no-ops on upgrade rather than stopping the service first), so
# this correctly restarts it onto the new binary.
if systemctl is-active --quiet sparky-server 2>/dev/null; then
    systemctl restart sparky-server
elif [ -n "${SPARKY_INSTALL_LOCAL_DB:-}" ]; then
    echo "sparky-server installed but not started."
    echo "Database ready - run: sudo -u sparky sh -c 'set -a && . /etc/sparky-server/secrets.env && set +a && /opt/sparky/bin/sparky-server setup'"
    echo "then: sudo systemctl start sparky-server"
else
    echo "sparky-server installed but not started."
    echo "Fill in /etc/sparky-server/secrets.env, then complete the database"
    echo "setup (createdb, migrate, sparky-server setup - see CLAUDE.md"
    echo "Database Setup and First Run), then run: sudo systemctl start sparky-server"
fi
