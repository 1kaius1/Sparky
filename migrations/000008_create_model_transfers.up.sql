-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Model transfers - see SCHEMA.md Model transfers. Operation log for both
-- an internet download and a peer-to-peer rsync replication, unified into
-- one table since both are the same shape of thing - a long-running,
-- progress-tracked transfer that lands model bytes on a specific node,
-- differing only in source.
--
-- source_node_id gets the same CHECK pairing as
-- nodes.container_runtime/node_type: a real invariant worth enforcing at
-- the database level even though nothing produces 'peer_node' yet (that's
-- v0.3.0's rsync work) - see PLANNING.md's Model transfers Phase 1
-- breakdown.
--
-- requested_by is nullable for the same reason nodes.registered_by is:
-- the break-glass SuperAdmin can initiate a transfer too and is not a
-- Users row - see SCHEMA.md Break-glass credential. SCHEMA.md's Model
-- transfers table is updated in this same change to record this.

CREATE TYPE transfer_source_type AS ENUM ('internet', 'peer_node');
CREATE TYPE transfer_status AS ENUM ('queued', 'transferring', 'completed', 'failed', 'cancelled');

CREATE TABLE model_transfers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    dest_node_id uuid NOT NULL REFERENCES nodes (id),
    model_ref text NOT NULL,
    source_type transfer_source_type NOT NULL,
    source_node_id uuid REFERENCES nodes (id),
    status transfer_status NOT NULL DEFAULT 'queued',
    bytes_transferred bigint NOT NULL DEFAULT 0,
    bytes_total bigint NOT NULL DEFAULT 0,
    requested_by uuid REFERENCES users (id),
    requested_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    error_message text,
    CONSTRAINT model_transfers_source_node_matches_type CHECK (
        (source_type = 'peer_node' AND source_node_id IS NOT NULL) OR
        (source_type = 'internet' AND source_node_id IS NULL)
    )
);
