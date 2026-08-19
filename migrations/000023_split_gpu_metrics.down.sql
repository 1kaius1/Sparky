-- SPDX-License-Identifier: AGPL-3.0-or-later

-- DEFAULT 0 lets these ADD COLUMNs succeed against an already-populated
-- metrics table (existing rows need a value for the reinstated NOT NULL
-- columns); dropped immediately after so the reinstated columns' contract
-- matches the original 000011 shape for any future insert.

DROP TABLE gpu_metrics;

ALTER TABLE metrics ADD COLUMN gpu_utilization_pct numeric NOT NULL DEFAULT 0;
ALTER TABLE metrics ADD COLUMN gpu_memory_used_mb numeric NOT NULL DEFAULT 0;
ALTER TABLE metrics ADD COLUMN gpu_memory_total_mb numeric NOT NULL DEFAULT 0;

ALTER TABLE metrics ALTER COLUMN gpu_utilization_pct DROP DEFAULT;
ALTER TABLE metrics ALTER COLUMN gpu_memory_used_mb DROP DEFAULT;
ALTER TABLE metrics ALTER COLUMN gpu_memory_total_mb DROP DEFAULT;
