-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Restores NOT NULL. Fails (leaving the schema unchanged) if any imported
-- row with a NULL placed_via exists - there is no transfer to point such a
-- row at, so those rows must be deleted or re-placed by hand first.

ALTER TABLE node_model_inventory ALTER COLUMN placed_via SET NOT NULL;
