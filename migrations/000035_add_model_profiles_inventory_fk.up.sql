-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Model profiles redesign (PLANNING.md Decisions Log) - a profile's
-- (target_node_id, model_ref, quantization, format) must now name a real
-- node_model_inventory entry, closing the gap left since Model profile
-- creation always free-typed model_ref/quantization with nothing checking
-- either against what a node actually has. No ON DELETE clause (defaults
-- RESTRICT): this is what stops deleting an inventory entry a Profile
-- still references, forcing a clean "still in use" error at delete time
-- (internal/inventory.Service.Delete) instead of silently orphaning the
-- Profile. Enforces existence only, not status = 'present' - that check is
-- Go-level (internal/profiles.Service.resolve), same validate-in-Go-
-- enforce-in-SQL split engine_params already uses. Postgres's default
-- MATCH SIMPLE semantics skip the check entirely when any referencing
-- column is NULL, so the prior migration's quantization NOT NULL was a
-- real prerequisite, not cosmetic - target_node_id can still be NULL for a
-- future clustered profile (not creatable today - the model_profiles_
-- single_node_only CHECK forces every current row to have it set), which
-- would leave this FK inert for that row until clustering is designed.
--
-- Only meaningful for single-node profiles today; revisit when clustering
-- relaxes the single-node CHECK constraint.
--
-- This migration fails if any existing profile's (target_node_id,
-- model_ref, quantization, format) does not match a real node_model_
-- inventory row - expected and intentional on an environment with stale
-- test data; the releasing operator must reconcile those profiles (delete
-- them, or place the matching model in that node's inventory) before this
-- migration can apply.

ALTER TABLE model_profiles
    ADD CONSTRAINT model_profiles_target_inventory_fkey
    FOREIGN KEY (target_node_id, model_ref, quantization, format)
    REFERENCES node_model_inventory (node_id, model_ref, quantization, format);
