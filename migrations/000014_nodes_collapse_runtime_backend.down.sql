-- SPDX-License-Identifier: AGPL-3.0-or-later

CREATE TYPE node_type AS ENUM ('spark', 'docker-gpu');
CREATE TYPE container_runtime AS ENUM ('docker', 'podman');

ALTER TABLE nodes ADD COLUMN node_type node_type;
ALTER TABLE nodes ADD COLUMN container_runtime container_runtime;

UPDATE nodes SET
    node_type = (CASE WHEN runtime_backend = 'bare-metal' THEN 'spark' ELSE 'docker-gpu' END)::node_type,
    container_runtime = CASE WHEN runtime_backend = 'bare-metal' THEN NULL ELSE runtime_backend::text::container_runtime END;

ALTER TABLE nodes ALTER COLUMN node_type SET NOT NULL;

ALTER TABLE nodes ADD CONSTRAINT nodes_container_runtime_matches_type CHECK (
    (node_type = 'docker-gpu' AND container_runtime IS NOT NULL) OR
    (node_type = 'spark' AND container_runtime IS NULL)
);

ALTER TABLE nodes DROP COLUMN runtime_backend;
DROP TYPE runtime_backend;
