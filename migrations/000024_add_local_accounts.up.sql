-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Local-only accounts - see SCHEMA.md Users and PLANNING.md's Decisions
-- Log. ad_sid becomes nullable so a Users row can exist with no AD/LDAP
-- identity behind it at all; the existing UNIQUE constraint already
-- permits multiple NULLs in Postgres (NULL is never equal to another NULL
-- for uniqueness purposes), so no index change is needed there.
-- local_username/local_password_hash are the local-account equivalent of
-- ad_sid/an LDAP bind - local_username UNIQUE gets the exact same
-- "unique when present, unconstrained when absent" behavior for the same
-- reason, no partial index required. The CHECK constraint enforces exactly
-- one identity mechanism per row at the database level, not just in
-- application code - matching this project's existing precedent (Audit
-- settings' retention_months CHECK).

ALTER TABLE users ALTER COLUMN ad_sid DROP NOT NULL;
ALTER TABLE users ADD COLUMN local_username text UNIQUE;
ALTER TABLE users ADD COLUMN local_password_hash text;
ALTER TABLE users ADD CONSTRAINT users_identity_mechanism_check CHECK (
    (ad_sid IS NOT NULL AND local_username IS NULL AND local_password_hash IS NULL)
    OR
    (ad_sid IS NULL AND local_username IS NOT NULL AND local_password_hash IS NOT NULL)
);
