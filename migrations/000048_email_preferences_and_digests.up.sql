-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Email preferences and digests - see SCHEMA.md Users (email_delivery,
-- email_min_severity), Email settings and Email digest items (PLANNING.md
-- Decisions Log, 2026-10-26).
--
-- Each person chooses how alert email reaches them: off, immediately (one
-- email per alert), or in an hourly or daily digest (one email listing what
-- arrived). The defaults match the agreed design: warning and critical alerts
-- by email at once, info alerts in the inbox only. A person with no address
-- gets nothing whatever they choose.
--
-- alert_severity is declared in ascending order (info < warning < critical),
-- so "at least warning" is a comparison in SQL.
CREATE TYPE email_delivery AS ENUM ('off', 'immediate', 'hourly', 'daily');

ALTER TABLE users
    ADD COLUMN email_delivery email_delivery NOT NULL DEFAULT 'immediate',
    ADD COLUMN email_min_severity alert_severity NOT NULL DEFAULT 'warning';

-- The daily digest goes out at one time, set by an Admin, in the global time
-- zone (schedule_settings). The hourly one goes out on the hour. Both run on
-- the shared scheduler, which needs to know when each last ran and when the
-- schedule was last changed (it never catches up on an occurrence from before
-- that), so a missed digest is sent once when the server comes back, never once
-- per miss.
ALTER TABLE email_settings
    ADD COLUMN daily_digest_time text NOT NULL DEFAULT '08:00',
    ADD COLUMN digest_schedule_set_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_hourly_digest_at timestamptz,
    ADD COLUMN last_daily_digest_at timestamptz,
    ADD CONSTRAINT email_settings_digest_time_format CHECK (daily_digest_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$');

-- Alerts waiting for a person's next digest. One row per (person, alert): a
-- repeat at a higher severity updates the row instead of adding another. The
-- title and severity are what the alert was when it was queued; at digest time
-- the alert itself is consulted, and one that has since been resolved or that
-- the person dismissed is left out. Rows go with the alert (retention) or the
-- person.
CREATE TABLE email_digest_items (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    alert_id uuid NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    severity alert_severity NOT NULL,
    title text NOT NULL,
    queued_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, alert_id)
);

CREATE INDEX email_digest_items_queued_at ON email_digest_items (queued_at);
