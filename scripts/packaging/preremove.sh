#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm preremove scriptlet (deb prerm / rpm %preun) - see
# scripts/packaging/nfpm.yaml. Stops and disables the service only on a
# real removal, never on an in-place upgrade - dpkg passes the word
# "remove"/"purge"/"upgrade" as $1, rpm passes a numeric install-count (0 =
# final removal, 1+ = an upgrade is in progress and another version's
# scripts will follow). Leaving the service running during an upgrade is
# what lets postinstall.sh's own is-active check correctly decide whether
# to restart it - see that script's own comment.
set -e

action="$1"
case "$action" in
    0|remove)
        systemctl stop sparky-agent 2>/dev/null || true
        systemctl disable sparky-agent 2>/dev/null || true

        # Copy purge_rpm.sh out to a path this package doesn't own *before*
        # removal deletes /opt/sparky/share/sparky-agent along with every
        # other package-tracked file - otherwise it would vanish on a plain
        # `dnf remove` before anyone could ever run it. Deb's own purge path
        # doesn't need this file, but the copy is harmless there too since
        # postremove.sh's purge branch cleans it up - see that script.
        if [ -f /opt/sparky/share/sparky-agent/purge_rpm.sh ]; then
            cp /opt/sparky/share/sparky-agent/purge_rpm.sh /usr/local/sbin/sparky-agent-purge.sh
            chmod 0755 /usr/local/sbin/sparky-agent-purge.sh
        fi
        ;;
    *)
        : # upgrade in progress (rpm: numeric 1+, deb: "upgrade") - no-op
        ;;
esac
