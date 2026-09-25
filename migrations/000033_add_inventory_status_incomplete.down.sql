-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Postgres cannot drop a single enum value, so the type is replaced. Rows
-- holding the value are first marked removed - the partial data they
-- describe is no longer tracked, exactly as it was before this migration.
UPDATE node_model_inventory SET status = 'removed' WHERE status = 'incomplete';

ALTER TYPE inventory_status RENAME TO inventory_status_old;
CREATE TYPE inventory_status AS ENUM ('present', 'stale', 'removed');
ALTER TABLE node_model_inventory ALTER COLUMN status TYPE inventory_status USING status::text::inventory_status;
ALTER TABLE node_engine_inventory ALTER COLUMN status TYPE inventory_status USING status::text::inventory_status;
DROP TYPE inventory_status_old;
