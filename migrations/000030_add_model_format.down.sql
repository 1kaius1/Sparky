-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE node_model_inventory DROP CONSTRAINT node_model_inventory_pkey;
ALTER TABLE node_model_inventory ADD PRIMARY KEY (node_id, model_ref, quantization);

ALTER TABLE node_model_inventory ALTER COLUMN quantization SET DEFAULT '';
-- Does not attempt to restore 'UNKNOWN' rows back to '' - a down
-- migration recovering exact prior data is out of scope, same precedent
-- migration 000014's own down migration already set.

ALTER TABLE node_model_inventory DROP COLUMN format;
ALTER TABLE model_profiles DROP COLUMN format;
DROP TYPE model_format;
