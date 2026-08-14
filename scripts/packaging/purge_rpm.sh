#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Manual purge-equivalent cleanup for the .rpm install path - see
# docs/AGENT.md Build and Install and scripts/packaging/postremove.sh's own
# doc comment for why this can't be automatic on RPM systems: rpm's %postun
# scriptlet only ever receives a numeric argument (0 = final removal, 1+ =
# upgrade), unlike dpkg's "purge" argument, so there is no reliable signal
# available to a scriptlet to distinguish "remove" from "remove and also
# delete the service account and secrets."
#
# Run this by hand after `sudo dnf remove sparky-agent` if you want the
# serviceloop account and /etc/sparky-agent fully removed too. preremove.sh
# copies this file to /usr/local/sbin/sparky-agent-purge.sh just before
# removal, since /opt/sparky/share/sparky-agent (where this copy normally
# lives while installed) is deleted along with the rest of the package's
# files on a plain `dnf remove` - this script wouldn't survive to be run
# otherwise:
#   sudo /usr/local/sbin/sparky-agent-purge.sh
set -e

userdel serviceloop 2>/dev/null || true
rm -rf /etc/sparky-agent
