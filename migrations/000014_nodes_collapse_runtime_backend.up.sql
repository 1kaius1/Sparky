-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Collapses node_type + container_runtime into a single runtime_backend
-- enum - see SCHEMA.md Nodes. The two columns only ever jointly expressed
-- one 3-way choice (docker-gpu was never meaningful without a paired
-- container_runtime), enforced by nodes_container_runtime_matches_type
-- rather than made structurally impossible - one column makes the other
-- five combinations unrepresentable instead of merely rejected.
--
-- 'spark' also wrongly conflated a hardware label with a runtime choice:
-- a headless Spark can use Docker/Podman GPU passthrough fine; bare-metal
-- direct exec is actually needed for hardware without viable passthrough
-- (e.g. a single-GPU workstation already using that GPU for its own host
-- session), independent of whether the hardware happens to be a Spark.
-- See PLANNING.md's Decisions Log for the full correction.

CREATE TYPE runtime_backend AS ENUM ('docker', 'podman', 'bare-metal');

ALTER TABLE nodes ADD COLUMN runtime_backend runtime_backend;

UPDATE nodes SET runtime_backend = CASE
    WHEN node_type = 'docker-gpu' THEN container_runtime::text::runtime_backend
    ELSE 'bare-metal'
END;

ALTER TABLE nodes ALTER COLUMN runtime_backend SET NOT NULL;

ALTER TABLE nodes DROP CONSTRAINT nodes_container_runtime_matches_type;
ALTER TABLE nodes DROP COLUMN container_runtime;
ALTER TABLE nodes DROP COLUMN node_type;

DROP TYPE node_type;
DROP TYPE container_runtime;
