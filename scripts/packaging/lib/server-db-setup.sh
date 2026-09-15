#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Optional local Postgres provisioning for sparky-server - see CLAUDE.md
# Build and Run, Bare-metal deployment (systemd). Sourced by
# scripts/install_server.sh (tarball path) and
# scripts/packaging/postinstall_server.sh (deb/rpm path, driven by the
# SPARKY_INSTALL_LOCAL_DB environment variable since postinstall scripts run
# unattended and can't prompt).
#
# Two methods, chosen by the operator per-host (not auto-detected): "podman"
# runs a persistent, systemd-managed Postgres container - the right choice
# where installing OS packages requires change control but running a
# container doesn't. "native" installs the distro's own postgresql-server
# package - the right choice where that's the easier path instead. Both are
# opt-in; a plain install with neither chosen behaves exactly as before this
# file existed.
#
# Deliberately NOT wired into scripts/uninstall_server.sh or
# scripts/packaging/postremove_server.sh, even under --purge/purge: whichever
# method was used to create it, this is a real database that may hold real
# data - removing it is never implied by removing the sparky-server package,
# and CLAUDE.md's own rule against unconfirmed deletion applies here just as
# much as anywhere else. Tearing it down, if ever wanted, is a separate,
# explicit, manual step.
#
# This file only defines functions - it does not execute anything on its
# own, and does not set shell options (that's each caller's own decision to
# make about its own script).

# random_password prints a 32-character alphanumeric password to stdout.
# Alphanumeric-only deliberately avoids any shell/SQL/URL quoting concerns
# in the rest of this file - every place this value is used (a
# --env-file line, an SQL string literal, a postgres:// URL) would
# otherwise need its own escaping rules.
random_password() {
    LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32
}

# db_url_is_placeholder succeeds only if /etc/sparky-server/secrets.env's
# DATABASE_URL is still exactly .env.example's own placeholder value - the
# guard that keeps this file from ever overwriting a real, already-configured
# database connection string.
db_url_is_placeholder() {
    grep -q '^DATABASE_URL=postgres://sparky:sparky@localhost:5432/sparky_dev?sslmode=disable$' \
        /etc/sparky-server/secrets.env 2>/dev/null
}

# set_database_url_in_secrets replaces the DATABASE_URL line in
# /etc/sparky-server/secrets.env with $1. Only ever called after
# db_url_is_placeholder has already confirmed it's safe to do so.
set_database_url_in_secrets() {
    value="$1"
    esc_value=$(printf '%s' "$value" | sed -e 's/[&/\]/\\&/g')
    sed -i "s|^DATABASE_URL=.*|DATABASE_URL=${esc_value}|" /etc/sparky-server/secrets.env
}

# setup_local_db_podman creates a persistent, systemd-managed Postgres
# container - a named volume (survives container recreation) and its own
# unit ($1/sparky-local-postgres.service, Restart=always, so it comes back
# on reboot same as sparky-server.service itself). Idempotent: if the unit
# is already enabled, assumes a prior run already did this and returns
# without touching anything (in particular, never regenerates the password
# for an already-provisioned database). Sets DATABASE_URL in the calling
# shell's environment on success.
setup_local_db_podman() {
    share_dir="$1"

    if systemctl is-enabled --quiet sparky-local-postgres 2>/dev/null; then
        echo "sparky-server: sparky-local-postgres.service is already set up - leaving it as-is"
        return 0
    fi

    if ! command -v podman >/dev/null 2>&1; then
        echo "sparky-server: podman not found on PATH - install podman, or choose the native database option instead" >&2
        return 1
    fi

    db_password=$(random_password)

    install -d -m 0755 /etc/sparky-server
    umask 077
    {
        echo "POSTGRES_USER=sparky"
        echo "POSTGRES_PASSWORD=${db_password}"
        echo "POSTGRES_DB=sparky"
    } > /etc/sparky-server/local-postgres.env
    umask 022
    chown root:root /etc/sparky-server/local-postgres.env
    chmod 0600 /etc/sparky-server/local-postgres.env

    podman volume create --ignore sparky-postgres-data >/dev/null

    install -m 0644 -o root -g root "${share_dir}/sparky-local-postgres.service" \
        /etc/systemd/system/sparky-local-postgres.service
    systemctl daemon-reload
    systemctl enable --now sparky-local-postgres >/dev/null

    echo "Waiting for the local Postgres container to accept connections..."
    tries=60
    while [ "$tries" -gt 0 ]; do
        if podman exec sparky-postgres pg_isready -U sparky >/dev/null 2>&1; then
            break
        fi
        tries=$((tries - 1))
        sleep 1
    done
    if [ "$tries" -eq 0 ]; then
        echo "sparky-server: local Postgres container did not become ready in time" >&2
        return 1
    fi

    DATABASE_URL="postgres://sparky:${db_password}@127.0.0.1:5432/sparky?sslmode=disable"
    export DATABASE_URL
}

# setup_local_db_native installs the distro's own postgresql-server package
# and creates a dedicated "sparky" role and database. Idempotent: if the
# "sparky" role already exists, assumes a prior run already did this and
# returns without touching anything (same reasoning as the podman path -
# never regenerates a password for an already-provisioned database). Sets
# DATABASE_URL in the calling shell's environment on success.
setup_local_db_native() {
    if id -u postgres >/dev/null 2>&1 && \
        sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='sparky'" 2>/dev/null | grep -q '^1$'; then
        echo "sparky-server: native Postgres role 'sparky' already exists - leaving it as-is"
        return 0
    fi

    # shellcheck source=/dev/null
    . /etc/os-release
    case "${ID:-}:${ID_LIKE:-}" in
        *debian*|*ubuntu*)
            apt-get update -qq
            apt-get install -y --no-install-recommends postgresql >/dev/null
            ;;
        *rhel*|*fedora*|*centos*|*rocky*|*almalinux*)
            dnf install -y postgresql-server postgresql-contrib >/dev/null
            if [ ! -d /var/lib/pgsql/data ] || [ -z "$(ls -A /var/lib/pgsql/data 2>/dev/null)" ]; then
                postgresql-setup --initdb
            fi
            ;;
        *)
            echo "sparky-server: unsupported distro for the native database option (ID=${ID:-unknown}) - use the podman option instead, or install postgresql-server yourself" >&2
            return 1
            ;;
    esac

    systemctl enable --now postgresql

    db_password=$(random_password)
    sudo -u postgres psql -v ON_ERROR_STOP=1 -c "CREATE ROLE sparky LOGIN PASSWORD '${db_password}';"
    sudo -u postgres createdb -O sparky sparky

    DATABASE_URL="postgres://sparky:${db_password}@127.0.0.1:5432/sparky?sslmode=disable"
    export DATABASE_URL
}

# run_bundled_migrations applies every migration in $1/migrations against
# $DATABASE_URL using the migrate binary bundled at $1/migrate - see
# scripts/build_packages.sh, which cross-compiles/stages it per architecture
# the same way it does the sparky-server binary itself. This is what lets
# migrations run unattended on a target host with no Go toolchain and no
# separately-installed migrate CLI, matching CLAUDE.md Database Migrations'
# own command exactly, just with both binary and SQL files already in place.
run_bundled_migrations() {
    share_dir="$1"
    echo "Running database migrations..."
    "${share_dir}/migrate" -path "${share_dir}/migrations" -database "$DATABASE_URL" up
}

# setup_local_database is the entry point both callers use: $1 is "podman"
# or "native", $2 is the share directory holding sparky-local-postgres.service
# and the bundled migrate binary/migrations (see the two functions above).
# Refuses to do anything if DATABASE_URL is already configured for real -
# see db_url_is_placeholder.
setup_local_database() {
    method="$1"
    share_dir="$2"

    # install_server.sh and postinstall_server.sh both already require root
    # before they ever get here, but this function is also meant to be
    # callable directly (CLAUDE.md's "already installed" invocation) with no
    # such guard upstream - checked explicitly rather than letting a non-root
    # invocation fail confusingly partway through ("cannot stat" on a later
    # install step, or podman silently talking to a *different*
    # rootless per-user podman instance than the one systemd's own rootful
    # unit uses, so a container it never sees looks like it "never becomes
    # ready").
    if [ "$(id -u)" -ne 0 ]; then
        echo "sparky-server: setup_local_database must be run as root (sudo) - both the podman and native paths create system-level state (/etc/sparky-server, a systemd unit, and either a rootful podman container or an OS package)" >&2
        return 1
    fi

    if ! db_url_is_placeholder; then
        echo "sparky-server: DATABASE_URL in /etc/sparky-server/secrets.env is already configured - skipping local database setup"
        return 0
    fi

    case "$method" in
        podman)
            setup_local_db_podman "$share_dir" || return 1
            ;;
        native)
            setup_local_db_native || return 1
            ;;
        *)
            echo "sparky-server: unknown local database method '${method}' (expected podman or native)" >&2
            return 1
            ;;
    esac

    # A method function's idempotent-skip branch (already provisioned by a
    # prior run) intentionally leaves DATABASE_URL unset rather than
    # re-deriving a password it never stored - if secrets.env is still on
    # the placeholder in that case, there's a real mismatch to resolve by
    # hand rather than paper over with an empty/wrong value.
    if [ -z "${DATABASE_URL:-}" ]; then
        echo "sparky-server: local database appears already provisioned by a prior run, but DATABASE_URL is still unset here - check the existing setup (/etc/sparky-server/local-postgres.env for podman, the native 'sparky' role otherwise) and set DATABASE_URL in /etc/sparky-server/secrets.env by hand" >&2
        return 1
    fi

    set_database_url_in_secrets "$DATABASE_URL"
    run_bundled_migrations "$share_dir"
    echo "Local Postgres database ready - DATABASE_URL written to /etc/sparky-server/secrets.env"
}
