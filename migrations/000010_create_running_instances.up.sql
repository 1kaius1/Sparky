-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Running instances - see SCHEMA.md Running instances. Live lifecycle
-- state of what is actually loaded right now - separate from Model
-- profiles (intent) for the same reason Node model inventory is separate
-- from Model transfers (history vs. current state). Single-node load/
-- unload only for v0.1.0 - Running instance nodes (the actual multi-node
-- topology table) is v0.3.0 clustering scope and does not exist yet, same
-- reasoning as model_profiles.fabric_group_id and profile_cluster_nodes
-- not existing until then either.
--
-- No uniqueness constraint stops two concurrent non-terminal rows for the
-- same profile_id - this is an operation-log-style history table, the
-- same shape as model_transfers, not a single-row-per-profile state
-- table. internal/lifecycle.Service is responsible for refusing a second
-- concurrent load in application logic
-- (RunningInstanceRepository.FindActiveByProfileID), not the database.

CREATE TYPE running_instance_status AS ENUM ('starting', 'running', 'stopping', 'stopped', 'failed');
CREATE TYPE instance_health_status AS ENUM ('healthy', 'unhealthy', 'unknown');

CREATE TABLE running_instances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid NOT NULL REFERENCES model_profiles (id),
    status running_instance_status NOT NULL DEFAULT 'starting',
    -- Entry point - the single node, for v0.1.0's single-node-only scope;
    -- will be the head node once clustered profiles exist.
    primary_node_id uuid NOT NULL REFERENCES nodes (id),
    -- Observed, may differ from the profile's declared port - null until
    -- the agent reports the container actually running.
    actual_port integer,
    -- Nullable for the same reason nodes.registered_by is: the
    -- break-glass SuperAdmin can start an instance too and is not a
    -- Users row.
    started_by uuid REFERENCES users (id),
    started_at timestamptz NOT NULL DEFAULT now(),
    stopped_at timestamptz,
    health_status instance_health_status NOT NULL DEFAULT 'unknown',
    last_health_check_at timestamptz,
    -- Populated on failure - the only feedback mechanism when
    -- required_memory_gb was left unset and the launch didn't fit, per
    -- SCHEMA.md.
    error_message text
);
