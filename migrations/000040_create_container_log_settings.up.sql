-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Container log settings - see SCHEMA.md Container log settings. Singleton
-- settings row, same pattern as audit_settings: seeded here rather than left
-- absent, since the retention job reads it on every run.
--
-- retention_months mirrors the audit log's retention: default 12, at most 24,
-- enforced with a CHECK constraint. Unlike the audit log's own setting, which
-- is only displayed and has no job behind it, this one is editable (Settings
-- page, Admin) and enforced by a daily expiry job that deletes archived
-- container logs older than this many months.
CREATE TABLE container_log_settings (
    id boolean PRIMARY KEY DEFAULT true,
    retention_months integer NOT NULL DEFAULT 12,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT container_log_settings_singleton CHECK (id),
    CONSTRAINT container_log_settings_retention_months_range CHECK (retention_months > 0 AND retention_months <= 24)
);

INSERT INTO container_log_settings (id) VALUES (true);
