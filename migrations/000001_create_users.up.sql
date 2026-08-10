-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Users - see SCHEMA.md Users. Keyed by an internal UUID rather than an
-- AD-specific identifier, so the LDAP-to-Entra ID migration and any future
-- username change never breaks a foreign key elsewhere in the schema.

CREATE TYPE user_tier AS ENUM ('read_only', 'developer', 'power_dev', 'admin');

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ad_sid text NOT NULL UNIQUE,
    entra_object_id text,
    display_name text NOT NULL,
    tier user_tier NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    elevated_by uuid REFERENCES users (id),
    elevated_at timestamptz
);
