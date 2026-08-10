-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Nodes - see SCHEMA.md Nodes. One row per compute host, Spark or generic
-- Docker/Podman GPU machine. gpu_memory_gb and cpu_memory_gb are split
-- from the start (equal for a Spark's unified memory - a Spark is the
-- degenerate case of this same model, not a special case) rather than a
-- single memory_capacity_gb field, per PLANNING.md Decisions Log.
--
-- fabric_group_id is deliberately not part of this migration - it
-- references Fabric groups, which does not exist until v0.3.0's
-- clustering work; it will be added by an ALTER TABLE alongside that
-- table, once there is something for it to reference.

CREATE TYPE node_type AS ENUM ('spark', 'docker-gpu');
CREATE TYPE container_runtime AS ENUM ('docker', 'podman');
CREATE TYPE agent_status AS ENUM ('online', 'offline', 'unreachable');

CREATE TABLE nodes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    hostname text NOT NULL,
    ip_address text NOT NULL,
    node_type node_type NOT NULL,
    container_runtime container_runtime,
    gpu_memory_gb numeric NOT NULL,
    cpu_memory_gb numeric NOT NULL,
    agent_status agent_status NOT NULL DEFAULT 'offline',
    last_heartbeat_at timestamptz,
    -- Nullable for the same reason Users.elevated_by is: the break-glass
    -- SuperAdmin is not a Users row and can still register a node - see
    -- SCHEMA.md Break-glass credential.
    registered_by uuid REFERENCES users (id),
    registered_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT nodes_container_runtime_matches_type CHECK (
        (node_type = 'docker-gpu' AND container_runtime IS NOT NULL) OR
        (node_type = 'spark' AND container_runtime IS NULL)
    )
);
