-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Adds format (safetensors / gguf) as an explicit first-class field on
-- both Model profiles and Node model inventory - see PLANNING.md's
-- Inventory redesign decision. Previously pure unenforced convention
-- (vllm/aphrodite assumed safetensors, llamacpp assumed GGUF) - wrong in
-- general (Aphrodite can also load GGUF), so format is a genuinely
-- independent axis from engine_type, not derivable from it.
--
-- DEFAULT 'safetensors' on both columns is deliberately transitional, not
-- a permanent statement that safetensors is the "normal" case - it exists
-- purely so the INSERT statements internal/profiles and internal/transfers
-- already run today, which don't yet reference this column at all, keep
-- working unmodified until the Go layer that actually threads a real
-- format value through lands (PLANNING.md's Decisions Log). Existing rows
-- are backfilled correctly below, from their own real data, not defaulted.
CREATE TYPE model_format AS ENUM ('safetensors', 'gguf');

ALTER TABLE model_profiles ADD COLUMN format model_format NOT NULL DEFAULT 'safetensors';
UPDATE model_profiles SET format = CASE
    WHEN quantization IS NOT NULL AND quantization <> '' THEN 'gguf'::model_format
    ELSE 'safetensors'::model_format
END;

ALTER TABLE node_model_inventory ADD COLUMN format model_format NOT NULL DEFAULT 'safetensors';
UPDATE node_model_inventory SET format = CASE
    WHEN quantization <> '' THEN 'gguf'::model_format
    ELSE 'safetensors'::model_format
END;

-- Node model inventory's quantization has been NOT NULL DEFAULT '' since
-- migration 000021, with '' as the sentinel for "not quantization-specific
-- / whole repo". That sentinel is retired for new data going forward
-- (PLANNING.md's Decisions Log - a real HuggingFace repo commonly has no
-- reliable precision label at all, so guessing one is worse than admitting
-- it's unknown) - historical '' rows are relabeled to the literal
-- 'UNKNOWN' below, an honest placeholder rather than a confident-but-often-
-- wrong guess, visibly flaggable in the Inventory UI as needing attention.
-- The DEFAULT '' is also dropped so it can no longer be silently relied on
-- - NodeModelInventoryRepository.Upsert already requires an explicit
-- quantization argument on every call, so nothing depends on this default
-- actually firing; it was already vestigial. New '' rows can still occur
-- until agent/modelinspect (a later PR) replaces the current
-- caller-supplies-a-guess behavior with real post-download file
-- inspection - this migration only cleans up what already exists.
UPDATE node_model_inventory SET quantization = 'UNKNOWN' WHERE quantization = '';
ALTER TABLE node_model_inventory ALTER COLUMN quantization DROP DEFAULT;

-- Extends node_model_inventory's primary key to include format, the same
-- way migration 000021 extended it to include quantization: two formats
-- of the same model_ref/quantization on the same node (unusual, but not
-- impossible - a GGUF repo and an unrelated safetensors repo could in
-- principle share a model_ref string) must not collide.
ALTER TABLE node_model_inventory DROP CONSTRAINT node_model_inventory_pkey;
ALTER TABLE node_model_inventory ADD PRIMARY KEY (node_id, model_ref, quantization, format);
