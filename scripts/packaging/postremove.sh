#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm postremove scriptlet (deb postrm / rpm %postun) - see
# scripts/packaging/nfpm.yaml.
#
# Only dpkg ever passes "purge" here (dpkg --purge / apt purge / apt remove
# --purge) - rpm has no equivalent concept at all: %postun only ever
# receives a numeric argument (0 = final removal, 1+ = upgrade), with no
# third state for "and also delete everything." This is a real RPM
# limitation, not an oversight - see scripts/packaging/purge_rpm.sh and
# docs/AGENT.md for the documented manual equivalent on RPM systems.
set -e

case "$1" in
    purge)
        userdel serviceloop 2>/dev/null || true
        rm -rf /etc/sparky-agent
        rm -f /usr/local/sbin/sparky-agent-purge.sh
        ;;
esac
