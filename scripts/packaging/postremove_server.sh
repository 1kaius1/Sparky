#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm postremove scriptlet (deb postrm / rpm %postun) - see
# scripts/packaging/nfpm-server.yaml.
#
# Only dpkg ever passes "purge" here (dpkg --purge / apt purge / apt remove
# --purge) - rpm has no equivalent concept at all: %postun only ever
# receives a numeric argument (0 = final removal, 1+ = upgrade), with no
# third state for "and also delete everything." This is a real RPM
# limitation, not an oversight - see scripts/packaging/purge_rpm_server.sh
# and CLAUDE.md for the documented manual equivalent on RPM systems.
set -e

case "$1" in
    purge)
        userdel sparky 2>/dev/null || true
        rm -rf /etc/sparky-server
        rm -f /usr/local/sbin/sparky-server-purge.sh
        ;;
esac
