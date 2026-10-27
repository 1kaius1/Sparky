-- SPDX-License-Identifier: AGPL-3.0-or-later

DROP TABLE email_digest_items;

ALTER TABLE email_settings
    DROP CONSTRAINT email_settings_digest_time_format,
    DROP COLUMN last_daily_digest_at,
    DROP COLUMN last_hourly_digest_at,
    DROP COLUMN digest_schedule_set_at,
    DROP COLUMN daily_digest_time;

ALTER TABLE users
    DROP COLUMN email_min_severity,
    DROP COLUMN email_delivery;

DROP TYPE email_delivery;
