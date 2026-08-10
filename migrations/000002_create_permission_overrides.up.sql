-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Permission overrides - see SCHEMA.md Permission overrides. Per-user
-- capability grants that sit outside the tier ladder, modeled as a table
-- rather than a boolean column so the next one-off exception is a new row,
-- not a schema change. Admins and SuperAdmin have every capability
-- implicitly and never need a row here.

CREATE TYPE permission_capability AS ENUM ('manage_model_store');

CREATE TABLE permission_overrides (
    user_id uuid NOT NULL REFERENCES users (id),
    capability permission_capability NOT NULL,
    granted_by uuid NOT NULL REFERENCES users (id),
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, capability)
);
