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

# On any real removal (never an upgrade), take the peer-transfer sshd
# drop-in out of service: it points at the binary being removed, and
# leaving sshd configured to call a missing AuthorizedKeysCommand is
# needless. Transient grant state goes too. The sparky-peer account itself
# stays until purge, like serviceloop.
case "$1" in
    remove|purge|0)
        rm -f /etc/ssh/sshd_config.d/50-sparky-peer.conf
        rm -rf /opt/sparky/serviceloop/peer-grants /opt/sparky/serviceloop/peer-used
        # "sshd" on RHEL-family, "ssh" on Debian-family; best-effort.
        systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null || true
        ;;
esac

case "$1" in
    purge)
        userdel sparky-peer 2>/dev/null || true
        rm -rf /var/lib/sparky-peer
        userdel serviceloop 2>/dev/null || true
        rm -rf /etc/sparky-agent
        rm -f /usr/local/sbin/sparky-agent-purge.sh
        ;;
esac
