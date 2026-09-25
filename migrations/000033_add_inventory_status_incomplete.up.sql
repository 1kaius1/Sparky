-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Adds 'incomplete' to inventory_status: a node holds partial data for a
-- model - what a cancelled or failed download/copy leaves behind (kept on
-- purpose so a new transfer can resume it). Without a status for it, that
-- data would sit on the node's disk, invisible to the Inventory page and so
-- impossible to free from the UI, while the model looks absent.
--
-- The enum is shared with node_engine_inventory, which never uses the new
-- value. A new value cannot be used in the transaction that adds it, and
-- nothing here does.
ALTER TYPE inventory_status ADD VALUE IF NOT EXISTS 'incomplete';
