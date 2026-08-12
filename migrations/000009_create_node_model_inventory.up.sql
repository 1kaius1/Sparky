-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Node model inventory - see SCHEMA.md Node model inventory. Current-state
-- answer to "does this node have this model right now", distinct from
-- Model transfers (history) the same way Running instances is distinct
-- from Model profiles (intent).
--
-- No separate id column - SCHEMA.md doesn't list one, so (node_id,
-- model_ref) is the natural composite primary key; the repository upserts
-- on it rather than inserting a new row per transfer, per PLANNING.md's
-- Model transfers Phase 1 breakdown.

CREATE TYPE inventory_status AS ENUM ('present', 'stale', 'removed');

CREATE TABLE node_model_inventory (
    node_id uuid NOT NULL REFERENCES nodes (id),
    model_ref text NOT NULL,
    status inventory_status NOT NULL,
    size_bytes bigint NOT NULL,
    placed_at timestamptz NOT NULL DEFAULT now(),
    placed_via uuid NOT NULL REFERENCES model_transfers (id),
    PRIMARY KEY (node_id, model_ref)
);
