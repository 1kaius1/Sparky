#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Stand-alone, admin-invoked entry point for the optional local-database
# setup (server-db-setup.sh's setup_local_database) - the one documented
# way to provision a local Postgres for sparky-server, regardless of
# install method (.deb, .rpm, or tarball). Run manually after installing
# the package and editing /etc/sparky-server/secrets.env:
#
#   sudo /opt/sparky/share/sparky-server/sparky-server-db-setup.sh podman
#   sudo /opt/sparky/share/sparky-server/sparky-server-db-setup.sh native
#
# Deliberately not triggered automatically by, or passed a method through,
# any installer - see postinstall_server.sh/install_server.sh's own doc
# comments and CLAUDE.md's "Optional local database" section for why: many
# environments' sudo policy disallows a leading VAR=value before a sudo'd
# command, so this is the one path every install method documents and uses
# the same way, run as its own deliberate step after install.
set -e

if [ "$(id -u)" -ne 0 ]; then
    echo "sparky-server-db-setup.sh: must be run as root (sudo)" >&2
    exit 1
fi

method="$1"
case "$method" in
    podman|native) ;;
    *)
        echo "sparky-server-db-setup.sh: expected 'podman' or 'native' as the first argument" >&2
        exit 1
        ;;
esac

. /opt/sparky/share/sparky-server/server-common.sh
require_secrets_acknowledged

. /opt/sparky/share/sparky-server/server-db-setup.sh
setup_local_database "$method" /opt/sparky/share/sparky-server

echo "Database ready - run: sudo -u sparky /opt/sparky/share/sparky-server/run-with-secrets-env.sh /opt/sparky/bin/sparky-server setup"
echo "then: sudo systemctl start sparky-server"
