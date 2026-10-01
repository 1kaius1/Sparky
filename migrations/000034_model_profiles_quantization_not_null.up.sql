-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Model profiles redesign (PLANNING.md Decisions Log) - from this point on,
-- quantization comes from the Inventory-driven picker: a real value copied
-- from the chosen node_model_inventory entry's own Quantization column
-- ("" for a whole-repo entry, same sentinel node_model_inventory already
-- uses - never a distinct "not specified" meaning). NULL stops being a
-- possible value, so model_profiles.quantization can anchor the composite
-- FK the next migration adds - Postgres's default MATCH SIMPLE composite FK
-- semantics silently skip the check entirely if any referencing column is
-- NULL. Existing NULL rows backfill to the literal sentinel 'UNKNOWN' -
-- the same sentinel node_model_inventory already uses for "a real
-- quantization exists but was never determined" - rather than '', since a
-- pre-existing row's true quantization genuinely isn't known, and '' would
-- misrepresent it as a confirmed whole-repo profile.

UPDATE model_profiles SET quantization = 'UNKNOWN' WHERE quantization IS NULL;
ALTER TABLE model_profiles ALTER COLUMN quantization SET NOT NULL;
