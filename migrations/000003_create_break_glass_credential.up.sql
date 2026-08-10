-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Break-glass credential - see SCHEMA.md Break-glass credential. Not a row
-- in Users: a single, isolated secret only the application process reads
-- or validates. The boolean primary key fixed to true, combined with the
-- CHECK constraint, makes a second row impossible at the database level -
-- this table can only ever hold zero or one rows.

CREATE TABLE break_glass_credential (
    id boolean PRIMARY KEY DEFAULT true,
    password_hash text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT break_glass_credential_singleton CHECK (id)
);
