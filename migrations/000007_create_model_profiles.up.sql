-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Model profiles - see SCHEMA.md Model profiles. A saved, named
-- configuration for running a model; single-node only for v0.1.0 - see
-- PLANNING.md's 2026-08-11 Decisions Log.
--
-- topology is declared with both values now (cheap - an enum, not a FK
-- to a nonexistent table, same reasoning as nodes.agent_status shipping
-- 'unreachable' before anything produces it), but
-- model_profiles_single_node_only is what actually enforces v0.1.0's
-- single-node-only scope: no row may have topology <> 'single_node' or
-- a null target_node_id. fabric_group_id is not part of this migration
-- at all - same reasoning as nodes.fabric_group_id - it references
-- Fabric groups, which does not exist until v0.3.0. That migration is
-- expected to relax this CHECK constraint once fabric_group_id exists
-- to hold the alternative.

CREATE TYPE profile_topology AS ENUM ('single_node', 'clustered');
CREATE TYPE profile_engine_type AS ENUM ('vllm', 'aphrodite', 'llamacpp');

CREATE TABLE model_profiles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    model_ref text NOT NULL,
    engine_type profile_engine_type NOT NULL,
    -- Deliberately opaque to the database - validated by the engine
    -- adapter (internal/engines), not a fixed column per possible flag.
    engine_params jsonb NOT NULL DEFAULT '{}'::jsonb,
    requires_full_gpu_residency boolean NOT NULL,
    required_memory_gb numeric,
    topology profile_topology NOT NULL DEFAULT 'single_node',
    -- Nullable at the column level only because a future clustered
    -- profile won't have one; the CHECK constraint below is what
    -- actually requires it for every row today.
    target_node_id uuid REFERENCES nodes (id),
    port integer NOT NULL,
    -- Nullable for the same reason nodes.registered_by is: the
    -- break-glass SuperAdmin can create/update a profile too and is not
    -- a Users row.
    created_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT model_profiles_single_node_only CHECK (
        topology = 'single_node' AND target_node_id IS NOT NULL
    )
);
