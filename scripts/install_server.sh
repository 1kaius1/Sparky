#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sparky-server tarball installer - see CLAUDE.md Build and Run. Run from
# inside the extracted tarball (expects bin/sparky-server,
# lib/server-common.sh, lib/server-db-setup.sh, sparky-server.service,
# sparky-local-postgres.service, secrets.env.template, migrate,
# migrations/, and uninstall_server.sh alongside this script - see
# scripts/build_packages.sh for how the tarball is assembled).
#
# Unlike the .deb/.rpm packages, this path is not tracked by any package
# manager, so the systemd unit installs to /etc/systemd/system/ (the
# correct location for a locally-installed, non-package-managed unit - see
# deploy/systemd/sparky-server.service's own doc comment) rather than
# /usr/lib/systemd/system/.
#
# This installs the OS-level pieces (binary, unit, service account,
# secrets.env skeleton) and, if --db=podman or --db=native is passed (or
# chosen at the interactive prompt below when neither is passed and this is
# a real terminal), an optional local Postgres database too - see
# lib/server-db-setup.sh. Either way, `sparky-server setup` itself
# (SuperAdmin break-glass credential entry) is never run automatically - it
# stays a separate, deliberately interactive manual step, per CLAUDE.md
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

db_method="none"
for arg in "$@"; do
    case "$arg" in
        --db=podman) db_method="podman" ;;
        --db=native) db_method="native" ;;
        --db=none) db_method="none" ;;
        *)
            echo "sparky-server: unknown argument '$arg' (expected --db=podman, --db=native, or --db=none)" >&2
            exit 1
            ;;
    esac
done

script_dir=$(cd "$(dirname "$0")" && pwd)

. "$script_dir/lib/server-common.sh"

install -d -m 0755 /opt/sparky/bin
install -m 0755 -o root -g root "$script_dir/bin/sparky-server" /opt/sparky/bin/sparky-server
ln -sf /opt/sparky/bin/sparky-server /usr/local/bin/sparky-server

install -m 0644 -o root -g root "$script_dir/sparky-server.service" /etc/systemd/system/sparky-server.service

# Persist a copy of the shared libraries, the bundled migrate binary and
# migrations, and the uninstaller so a later
# `sudo /opt/sparky/share/sparky-server/uninstall_server.sh` works even if
# this extracted tarball directory no longer exists.
install -d -m 0755 /opt/sparky/share/sparky-server
install -m 0755 -o root -g root "$script_dir/lib/server-common.sh" /opt/sparky/share/sparky-server/server-common.sh
install -m 0755 -o root -g root "$script_dir/lib/server-db-setup.sh" /opt/sparky/share/sparky-server/server-db-setup.sh
install -m 0644 -o root -g root "$script_dir/secrets.env.template" /opt/sparky/share/sparky-server/secrets.env.template
install -m 0644 -o root -g root "$script_dir/sparky-local-postgres.service" /opt/sparky/share/sparky-server/sparky-local-postgres.service
install -m 0755 -o root -g root "$script_dir/uninstall_server.sh" /opt/sparky/share/sparky-server/uninstall_server.sh
install -m 0755 -o root -g root "$script_dir/migrate" /opt/sparky/share/sparky-server/migrate
rm -rf /opt/sparky/share/sparky-server/migrations
cp -r "$script_dir/migrations" /opt/sparky/share/sparky-server/migrations

ensure_service_account
ensure_secrets_file /opt/sparky/share/sparky-server/secrets.env.template

if [ "$db_method" = "none" ] && [ -t 0 ]; then
    echo
    echo "Set up a local Postgres database for sparky-server now?"
    echo "  1) No - I'll point DATABASE_URL at my own Postgres (default)"
    echo "  2) Podman container (persistent, systemd-managed)"
    echo "  3) Native postgresql-server package"
    printf "Choice [1]: "
    read -r db_choice
    case "$db_choice" in
        2) db_method="podman" ;;
        3) db_method="native" ;;
        *) db_method="none" ;;
    esac
fi

if [ "$db_method" != "none" ]; then
    . "$script_dir/lib/server-db-setup.sh"
    setup_local_database "$db_method" /opt/sparky/share/sparky-server
fi

systemctl daemon-reload
systemctl enable sparky-server >/dev/null 2>&1 || true

# Same reasoning as scripts/packaging/postinstall_server.sh: never auto-start
# a fresh install, but safely restart an already-running server if this
# script is being re-run to install an upgrade.
if systemctl is-active --quiet sparky-server 2>/dev/null; then
    systemctl restart sparky-server
    echo "sparky-server upgraded and restarted."
elif [ "$db_method" != "none" ]; then
    echo "sparky-server installed but not started."
    echo "Database ready - run: sudo -u sparky sh -c 'set -a && . /etc/sparky-server/secrets.env && set +a && /opt/sparky/bin/sparky-server setup'"
    echo "then: sudo systemctl start sparky-server"
else
    echo "sparky-server installed but not started."
    echo "Fill in /etc/sparky-server/secrets.env, then complete the database"
    echo "setup (createdb, migrate, sparky-server setup - see CLAUDE.md"
    echo "Database Setup and First Run), then run: sudo systemctl start sparky-server"
fi
