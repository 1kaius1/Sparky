-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Extends node_model_inventory's primary key to include quantization -
-- see SCHEMA.md Node model inventory and PLANNING.md's Decisions Log.
-- Without this, two quantizations of the same GGUF repo downloaded to the
-- same node would collide under the old (node_id, model_ref) key and
-- silently overwrite each other's inventory row.
--
-- quantization is NOT NULL here (unlike model_profiles.quantization and
-- model_transfers.quantization, both nullable) because a PRIMARY KEY
-- column cannot be NULL - empty string is the sentinel for "whole repo /
-- not quantization-specific", carrying the same meaning the other two
-- tables express as NULL.

ALTER TABLE node_model_inventory ADD COLUMN quantization text NOT NULL DEFAULT '';
ALTER TABLE node_model_inventory DROP CONSTRAINT node_model_inventory_pkey;
ALTER TABLE node_model_inventory ADD PRIMARY KEY (node_id, model_ref, quantization);
