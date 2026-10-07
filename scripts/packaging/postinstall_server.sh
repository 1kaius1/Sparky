#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm postinstall scriptlet (deb postinst / rpm %post) - see
# scripts/packaging/nfpm-server.yaml.
#
# Deliberately does NOT run any "setup"-style subcommand, and does NOT
# provision a local database, automatically: `sparky-server setup` is the
# interactive database/break-glass bootstrap wizard (CLAUDE.md First Run),
# and local-database provisioning (sparky-server-db-setup.sh) both need a
# secrets.env the operator has actually reviewed first - see that script's
# own doc comment for why this is a separate, explicitly-run step rather
# than something passed in at install time (an earlier design read
# SPARKY_INSTALL_LOCAL_DB from the environment before `apt`/`dnf install`,
# removed - many environments' sudo policy disallows a leading VAR=value
# before a sudo'd command). Only OS-level provisioning (the service
# account, the secrets file skeleton) happens here.
set -e

. /opt/sparky/share/sparky-server/server-common.sh

ensure_service_account
ensure_secrets_file /opt/sparky/share/sparky-server/secrets.env.template

systemctl daemon-reload
systemctl enable sparky-server >/dev/null 2>&1 || true

# Never auto-start on a fresh install - an unconfigured secrets.env would
# just crash-loop, and even a correctly-filled-in one still needs
# `sparky-server setup` run once before the app will serve normal routes.
# On a fresh install nothing is active yet, so this check is always false.
# On an upgrade, the old process is still running at this point
# (preremove_server.sh deliberately no-ops on upgrade rather than stopping
# the service first), so this correctly restarts it onto the new binary.
if systemctl is-active --quiet sparky-server 2>/dev/null; then
    systemctl restart sparky-server
else
    echo "sparky-server installed but not started."
    echo "Edit /etc/sparky-server/secrets.env with real values for your deployment,"
    echo "then set SPARKY_SETUP_UNACKNOWLEDGED=0 in that file to confirm you've done so."
    echo "For a local Postgres database, run:"
    echo "  sudo /opt/sparky/share/sparky-server/sparky-server-db-setup.sh podman   (or native)"
    echo "Otherwise point DATABASE_URL at your own Postgres and run the bundled migrate"
    echo "(see CLAUDE.md Database Setup), then:"
    echo "  sudo -u sparky /opt/sparky/share/sparky-server/run-with-secrets-env.sh /opt/sparky/bin/sparky-server setup"
    echo "  sudo systemctl start sparky-server"
fi
