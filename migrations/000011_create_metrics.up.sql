-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Metrics - see SCHEMA.md Metrics. Append-only telemetry history, high
-- write volume by design - kept in the same Postgres instance as
-- everything else rather than a dedicated time-series database, since the
-- scale here (a handful of nodes, a few-second poll interval) doesn't
-- justify that infrastructure.
--
-- No separate id column - SCHEMA.md doesn't list one, same reasoning as
-- node_model_inventory: (node_id, recorded_at) is the natural composite
-- primary key for a point-in-time reading from a specific node.
-- running_instance_id is nullable and has no matching CHECK pairing (a
-- node can simply have nothing loaded) and no ON DELETE clause - metrics
-- retention (6 months raw, per SCHEMA.md) is a v0.4.0 item, so a running
-- instance old enough to be worth pruning independent of its metrics rows
-- doesn't happen yet.

CREATE TABLE metrics (
    recorded_at timestamptz NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes (id),
    running_instance_id uuid REFERENCES running_instances (id),
    gpu_utilization_pct numeric NOT NULL,
    gpu_memory_used_mb numeric NOT NULL,
    gpu_memory_total_mb numeric NOT NULL,
    cpu_utilization_pct numeric NOT NULL,
    system_memory_used_mb numeric NOT NULL,
    system_memory_total_mb numeric NOT NULL,
    PRIMARY KEY (node_id, recorded_at)
);
