-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE node_model_inventory DROP CONSTRAINT node_model_inventory_pkey;
ALTER TABLE node_model_inventory ADD PRIMARY KEY (node_id, model_ref);
ALTER TABLE node_model_inventory DROP COLUMN quantization;
