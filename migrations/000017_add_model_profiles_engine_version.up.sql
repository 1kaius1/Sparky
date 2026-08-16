-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Per-profile engine version pinning - see SCHEMA.md Model profiles and
-- PLANNING.md's 2026-08-15 Decisions Log entry (deferred from the
-- compiled-engine-provisioning work) plus its own dated follow-up entry
-- recording this implementation. NULL means "use whatever the node's
-- latest symlink currently points to" - today's unpinned behavior,
-- unchanged. No CHECK constraint - any non-empty string is accepted, and
-- a bad pin fails clearly at launch time instead of at save time
-- (confirmed with the user, matching required_memory_gb's own "attempt
-- the launch and report failure" philosophy).

ALTER TABLE model_profiles ADD COLUMN engine_version text;
