-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Audit settings - see SCHEMA.md Audit settings. Singleton settings row,
-- same pattern as metrics_export_config: seeded with a default row here
-- rather than left absent, since these are effective settings the app
-- reads on every request, not a one-time setup credential.
--
-- forwarding_protocol's default of 'syslog' is already stated in
-- SCHEMA.md's own column description. retention_months' default of 12
-- was not previously decided anywhere in SCHEMA.md or PLANNING.md -
-- confirmed with the user when this migration was written (PLANNING.md
-- Decisions Log), a middle value between the Metrics table's own 6-month
-- raw-resolution retention window and this column's 24-month ceiling.
-- That ceiling itself is enforced with a CHECK constraint, matching
-- SCHEMA.md's "up to 24" wording literally rather than leaving it as an
-- application-level-only convention.

CREATE TYPE audit_forwarding_protocol AS ENUM ('syslog', 'gelf');

CREATE TABLE audit_settings (
    id boolean PRIMARY KEY DEFAULT true,
    retention_months integer NOT NULL DEFAULT 12,
    forwarding_enabled boolean NOT NULL DEFAULT false,
    forwarding_protocol audit_forwarding_protocol NOT NULL DEFAULT 'syslog',
    forwarding_host text,
    forwarding_port integer,
    forwarding_tls_enabled boolean NOT NULL DEFAULT false,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_settings_singleton CHECK (id),
    CONSTRAINT audit_settings_retention_months_range CHECK (retention_months > 0 AND retention_months <= 24)
);

INSERT INTO audit_settings (id) VALUES (true);
