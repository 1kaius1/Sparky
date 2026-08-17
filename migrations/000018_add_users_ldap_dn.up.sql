-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Caches each user's LDAP distinguishedName, resolved at every login -
-- see SCHEMA.md Users and PLANNING.md's Decisions Log entry on mid-session
-- AD group re-validation. Nullable: existing rows (and any session created
-- before this migration) have no cached DN yet - a mid-session recheck
-- treats a NULL the same as "no longer a member," which self-heals on the
-- user's next real login.

ALTER TABLE users ADD COLUMN ldap_dn text;
