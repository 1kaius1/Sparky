#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm preinstall scriptlet (deb preinst / rpm %pre) - see
# scripts/packaging/nfpm.yaml. Deliberately minimal: nothing about creating
# the serviceloop account or installing secrets.env needs to happen before
# this package's own files are unpacked, so all of that lives in
# postinstall.sh instead, which - unlike this script - can rely on
# /opt/sparky/share/sparky-agent/agent-common.sh already being on disk.
set -e

if ! command -v systemctl >/dev/null 2>&1; then
    echo "sparky-agent: systemctl not found - this package requires systemd" >&2
    exit 1
fi
