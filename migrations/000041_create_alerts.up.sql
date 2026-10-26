-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Alerts - see SCHEMA.md Alerts. An in-app inbox for things that need a
-- person's attention (PLANNING.md Decisions Log, 2026-10-26).
--
-- alerts holds one row per alert. dedupe_key makes a recurring condition one
-- alert instead of many: the partial unique index below allows at most one
-- UNRESOLVED alert per key, so a repeat bumps occurrences instead of adding a
-- row, while a repeat after the alert was resolved is a new row. A NULL key
-- is never deduplicated.
--
-- severity is an enum declared in ascending order so a comparison in SQL
-- (and the "severity increased" check) follows it.
--
-- resolution says how an alert was closed: 'cleared' when the condition that
-- raised it went away, 'user' when a person closed it (resolved_by is that
-- person, or NULL for the break-glass SuperAdmin, which is not a users row).
CREATE TYPE alert_severity AS ENUM ('info', 'warning', 'critical');
CREATE TYPE alert_resolution AS ENUM ('cleared', 'user');
CREATE TYPE alert_audience_type AS ENUM ('user', 'tier');

CREATE TABLE alerts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL,
    severity alert_severity NOT NULL,
    title text NOT NULL,
    body text NOT NULL DEFAULT '',
    object_type text,
    object_id text,
    dedupe_key text,
    first_raised_at timestamptz NOT NULL DEFAULT now(),
    last_raised_at timestamptz NOT NULL DEFAULT now(),
    occurrences integer NOT NULL DEFAULT 1,
    resolved_at timestamptz,
    resolved_by uuid REFERENCES users (id),
    resolution alert_resolution,
    CONSTRAINT alerts_resolution_consistent CHECK ((resolved_at IS NULL) = (resolution IS NULL)),
    CONSTRAINT alerts_occurrences_positive CHECK (occurrences > 0)
);

CREATE UNIQUE INDEX alerts_open_dedupe_key ON alerts (dedupe_key) WHERE dedupe_key IS NOT NULL AND resolved_at IS NULL;
CREATE INDEX alerts_last_raised_at ON alerts (last_raised_at);

-- Who an alert is addressed to. Each row is either one user or "this tier and
-- above" (min_tier, matching the "Developer and above" convention used
-- everywhere else; user_tier is declared in ascending order so <= works). An
-- application-group audience type is added by a later migration.
CREATE TABLE alert_audiences (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id uuid NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    audience_type alert_audience_type NOT NULL,
    user_id uuid REFERENCES users (id) ON DELETE CASCADE,
    min_tier user_tier,
    CONSTRAINT alert_audiences_shape CHECK (
        (audience_type = 'user' AND user_id IS NOT NULL AND min_tier IS NULL) OR
        (audience_type = 'tier' AND min_tier IS NOT NULL AND user_id IS NULL)
    )
);

CREATE UNIQUE INDEX alert_audiences_user ON alert_audiences (alert_id, user_id) WHERE audience_type = 'user';
CREATE UNIQUE INDEX alert_audiences_tier ON alert_audiences (alert_id, min_tier) WHERE audience_type = 'tier';
CREATE INDEX alert_audiences_user_lookup ON alert_audiences (user_id) WHERE audience_type = 'user';

-- Per-user read and dismissed state, created lazily on the first read or
-- dismiss. A missing row means unread and not dismissed. Dismissing hides an
-- alert for that user only; resolving (on alerts) closes it for everyone.
CREATE TABLE alert_user_state (
    alert_id uuid NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    read_at timestamptz,
    dismissed_at timestamptz,
    PRIMARY KEY (alert_id, user_id)
);

-- Alert settings - singleton row, same pattern as container_log_settings:
-- seeded here rather than left absent, because the retention job reads it on
-- every run. retention_months has the audit log's range (default 12, at most
-- 24, enforced here as well as in the service).
CREATE TABLE alert_settings (
    id boolean PRIMARY KEY DEFAULT true,
    retention_months integer NOT NULL DEFAULT 12,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alert_settings_singleton CHECK (id),
    CONSTRAINT alert_settings_retention_months_range CHECK (retention_months > 0 AND retention_months <= 24)
);

INSERT INTO alert_settings (id) VALUES (true);
