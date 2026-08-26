-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Reverses 000024_add_local_accounts.up.sql. Restoring ad_sid's NOT NULL
-- constraint will fail if any local-only rows (ad_sid IS NULL) still exist
-- at rollback time - same "assumes a clean pre-migration state" limitation
-- every down migration in this project already carries, not something
-- this file works around.

ALTER TABLE users DROP CONSTRAINT users_identity_mechanism_check;
ALTER TABLE users DROP COLUMN local_password_hash;
ALTER TABLE users DROP COLUMN local_username;
ALTER TABLE users ALTER COLUMN ad_sid SET NOT NULL;
