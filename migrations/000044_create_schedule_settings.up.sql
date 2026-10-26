-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Schedule settings - see SCHEMA.md Schedule settings. Singleton settings
-- row, same pattern as container_log_settings: seeded here rather than left
-- absent, since every scheduled job reads it to interpret its schedule.
--
-- timezone is the one global IANA time zone every schedule is read in (stale
-- container monitoring today, scheduled model loads later) and emails will
-- show times in. It is validated in Go against the zone database
-- (internal/schedule), not in SQL: the set of valid names belongs to the
-- server's tzdata, not to the schema.
CREATE TABLE schedule_settings (
    id boolean PRIMARY KEY DEFAULT true,
    timezone text NOT NULL DEFAULT 'UTC',
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT schedule_settings_singleton CHECK (id),
    CONSTRAINT schedule_settings_timezone_not_blank CHECK (length(btrim(timezone)) > 0)
);

INSERT INTO schedule_settings (id) VALUES (true);
