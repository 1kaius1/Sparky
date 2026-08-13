-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Metrics export config - see SCHEMA.md Metrics export config. Singleton
-- settings row, same boolean-primary-key-plus-CHECK pattern as
-- break_glass_credential, guaranteeing at most one row at the database
-- level. Unlike break_glass_credential, which legitimately has no row
-- until `sparky set-superadmin-password` runs, this row is seeded here
-- with a safe default (backend_type = 'none', nothing exported) so the
-- Settings page and any future export job never need to handle a
-- "not configured yet" state distinct from "configured to export
-- nothing".

CREATE TYPE metrics_export_backend AS ENUM ('none', 'nfs', 's3');

CREATE TABLE metrics_export_config (
    id boolean PRIMARY KEY DEFAULT true,
    backend_type metrics_export_backend NOT NULL DEFAULT 'none',
    config jsonb,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT metrics_export_config_singleton CHECK (id)
);

INSERT INTO metrics_export_config (id) VALUES (true);
