#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sparky-agent tarball uninstaller - see docs/AGENT.md Build and Install.
# Undoes what scripts/install_agent.sh did. A copy of this script is
# persisted at /opt/sparky/share/sparky-agent/uninstall_agent.sh by the
# installer, so it can be run later without the original tarball.
#
# By default (matching the .deb "remove" semantics), this stops and
# disables the service and removes the binary, but leaves the serviceloop
# system account and /etc/sparky-agent/secrets.env in place - avoiding
# orphaned file ownership and accidental credential loss. Pass --purge to
# also remove those (matching the .deb "purge" semantics).
set -e

if [ "$(id -u)" -ne 0 ]; then
    echo "sparky-agent: uninstall_agent.sh must be run as root (sudo)" >&2
    exit 1
fi

purge=false
if [ "$1" = "--purge" ]; then
    purge=true
fi

systemctl stop sparky-agent 2>/dev/null || true
systemctl disable sparky-agent 2>/dev/null || true
rm -f /etc/systemd/system/sparky-agent.service
systemctl daemon-reload

rm -f /usr/local/bin/sparky-agent
rm -f /opt/sparky/bin/sparky-agent

if [ "$purge" = true ]; then
    userdel serviceloop 2>/dev/null || true
    rm -rf /etc/sparky-agent
fi

echo "sparky-agent uninstalled."
if [ "$purge" = false ]; then
    echo "serviceloop account and /etc/sparky-agent left in place - rerun with --purge to remove them too."
fi

# Removes /opt/sparky/share/sparky-agent, including this running script -
# safe on Linux: an unlinked file stays valid for the process that already
# has it open until that process exits. Done before the rmdir cleanup below
# so /opt/sparky/share and /opt/sparky itself can actually go empty and be
# removed, rather than leaving stray empty directories behind.
rm -rf /opt/sparky/share/sparky-agent
rmdir /opt/sparky/bin 2>/dev/null || true
rmdir /opt/sparky/share 2>/dev/null || true
rmdir /opt/sparky 2>/dev/null || true
