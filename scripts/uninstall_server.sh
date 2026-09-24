#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sparky-server tarball uninstaller - see CLAUDE.md Build and Run. Undoes
# what scripts/install_server.sh did. A copy of this script is persisted at
# /opt/sparky/share/sparky-server/uninstall_server.sh by the installer, so
# it can be run later without the original tarball.
#
# By default (matching the .deb "remove" semantics), this stops and
# disables the service and removes the binary, but leaves the sparky system
# account and /etc/sparky-server/secrets.env in place - avoiding orphaned
# file ownership and accidental credential loss. Pass --purge to also
# remove those (matching the .deb "purge" semantics). This never touches
# the database - that's outside this installer's concern entirely.
set -e

if [ "$(id -u)" -ne 0 ]; then
    echo "sparky-server: uninstall_server.sh must be run as root (sudo)" >&2
    exit 1
fi

purge=false
if [ "$1" = "--purge" ]; then
    purge=true
fi

systemctl stop sparky-server 2>/dev/null || true
systemctl disable sparky-server 2>/dev/null || true
rm -f /etc/systemd/system/sparky-server.service
systemctl daemon-reload

rm -f /usr/local/bin/sparky-server
rm -f /opt/sparky/bin/sparky-server

if [ "$purge" = true ]; then
    userdel sparky 2>/dev/null || true
    rm -rf /etc/sparky-server
fi

echo "sparky-server uninstalled."
if [ "$purge" = false ]; then
    echo "sparky account and /etc/sparky-server left in place - rerun with --purge to remove them too."
fi

# Removes /opt/sparky/share/sparky-server, including this running script -
# safe on Linux: an unlinked file stays valid for the process that already
# has it open until that process exits. Done before the rmdir cleanup below
# so /opt/sparky/share and /opt/sparky itself can actually go empty and be
# removed, rather than leaving stray empty directories behind.
rm -rf /opt/sparky/share/sparky-server
rmdir /opt/sparky/bin 2>/dev/null || true
rmdir /opt/sparky/share 2>/dev/null || true
rmdir /opt/sparky 2>/dev/null || true
