#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Loads /etc/sparky-server/secrets.env into the environment, then execs the
# command given as arguments - e.g. `sparky-server setup`,
# `sparky-server set-superadmin-password`, or any other one-off admin
# command that needs the same DATABASE_URL/LDAP_*/etc. the running service
# gets via systemd's own EnvironmentFile=.
#
# Deliberately NOT `. /etc/sparky-server/secrets.env` (or `set -a && .
# secrets.env && set +a`, this project's own earlier documented approach) -
# secrets.env is written in systemd's EnvironmentFile= format, which is NOT
# shell syntax: systemd's own parser treats the whole rest of a line after
# "=" as the value, spaces included, with no quoting required. A real shell
# sourcing the same file does normal word-splitting instead, so any value
# containing an unquoted space - e.g. .env.example's own example
# LDAP_BIND_DN=CN=svc-sparky,OU=Service Accounts,DC=example,DC=internal -
# gets split into a var assignment followed by a second "word" the shell
# then tries to run as a command ("Accounts,DC=example,DC=internal: command
# not found") - confirmed for real against a live LDAP_BIND_DN value during
# this project's own bare-metal verification pass. Reading the file
# line-by-line with IFS="=" instead captures everything after the first "="
# verbatim into one variable, matching systemd's own parsing behavior
# without reimplementing its (rarely-used here) quoting support.
set -e

if [ ! -r /etc/sparky-server/secrets.env ]; then
    echo "run-with-secrets-env.sh: /etc/sparky-server/secrets.env not found or not readable" >&2
    exit 1
fi

set -a
while IFS='=' read -r key value; do
    case "$key" in
        ''|'#'*) continue ;;
    esac
    export "${key}=${value}"
done < /etc/sparky-server/secrets.env
set +a

exec "$@"
