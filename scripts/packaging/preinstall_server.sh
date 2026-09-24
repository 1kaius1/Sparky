#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# nfpm preinstall scriptlet (deb preinst / rpm %pre) - see
# scripts/packaging/nfpm-server.yaml. Deliberately minimal: nothing about
# creating the sparky account or installing secrets.env needs to happen
# before this package's own files are unpacked, so all of that lives in
# postinstall_server.sh instead, which - unlike this script - can rely on
# /opt/sparky/share/sparky-server/server-common.sh already being on disk.
set -e

if ! command -v systemctl >/dev/null 2>&1; then
    echo "sparky-server: systemctl not found - this package requires systemd" >&2
    exit 1
fi
