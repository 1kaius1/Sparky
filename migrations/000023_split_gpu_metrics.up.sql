-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Splits GPU readings out of metrics into their own gpu_metrics table - see
-- SCHEMA.md Metrics and the new GPU metrics section. A node has exactly one
-- CPU and one RAM pool regardless of how many GPUs it has, so
-- cpu_utilization_pct/system_memory_* stay node-level in metrics; GPU
-- utilization/memory are genuinely per-device, so they move to a table keyed
-- on (node_id, gpu_index, recorded_at) instead of being summed/averaged into
-- one aggregate reading per node (the original 000011 design, replaced here -
-- see PLANNING.md's Decisions Log for the full history of that choice and
-- this correction).
--
-- gpu_metrics.running_instance_id deliberately duplicates the sibling
-- metrics row's value for the same (node_id, recorded_at) rather than
-- requiring a join back to metrics to find it - both come from the same
-- agent/telemetry.Reading (one tick, one correlation lookup), so this is the
-- same value recorded twice, not a second source of truth. No FK/CHECK ties
-- a gpu_metrics row back to its sibling metrics row: internal/metrics.
-- Service.HandleTelemetry writes them as two independent best-effort
-- inserts (one node-level row, one per reported GPU), matching this path's
-- existing "one missed reading isn't worth failing the rest over"
-- philosophy - a partial tick (e.g. the node-level row lands but one GPU's
-- insert fails) is a self-healing gap, not a consistency violation worth a
-- transaction over.
--
-- No separate id column, same reasoning as metrics/node_model_inventory:
-- (node_id, gpu_index, recorded_at) is the natural composite primary key.
-- No secondary index beyond that PK - its leading columns already serve
-- LatestByNodeAndGPU's DISTINCT ON (node_id, gpu_index) for free, and at
-- the scale this project operates at (a handful of nodes, a few-second poll
-- interval), a plain ORDER BY recorded_at DESC LIMIT scan for Recent needs
-- no supporting index either, same reasoning already on record for
-- metrics' own Recent query.

ALTER TABLE metrics DROP COLUMN gpu_utilization_pct;
ALTER TABLE metrics DROP COLUMN gpu_memory_used_mb;
ALTER TABLE metrics DROP COLUMN gpu_memory_total_mb;

CREATE TABLE gpu_metrics (
    recorded_at timestamptz NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes (id),
    gpu_index integer NOT NULL,
    running_instance_id uuid REFERENCES running_instances (id),
    gpu_utilization_pct numeric NOT NULL,
    gpu_memory_used_mb numeric NOT NULL,
    gpu_memory_total_mb numeric NOT NULL,
    PRIMARY KEY (node_id, gpu_index, recorded_at)
);
