-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A container's output and exit reason, saved by the agent before it removes
-- the container (docs/AGENT.md Container log archive), so why an engine
-- failed or died is still readable after the container is gone. One row per
-- archived container. The log is gzip text in a bytea column, sent by the
-- agent in chunks and stored whole once the central app has verified it.
--
-- upload_id is the agent's own random id for one upload and is unique, so an
-- agent that retries an upload it never saw confirmed cannot store it twice.
--
-- instance_id and profile_id are plain uuids with no foreign key, and
-- profile_name is a copy: a profile can be renamed or deleted, an instance
-- row can be cleaned up, and the archive must outlive both. Nothing
-- references this table, so it can be moved or expired independently of the
-- rest of the schema. node_id does have a foreign key - an archive without
-- its node would be unplaceable.
--
-- lines_requested is NULL when every line was asked for. started_at,
-- finished_at and exit_code are NULL when the backend could not tell (a
-- container that never started, a bare-metal process still running when its
-- output was read). truncated means older output was lost to a size cap or a
-- full in-memory buffer, not merely that fewer lines were requested.
CREATE TABLE container_log_archives (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    upload_id text NOT NULL UNIQUE,
    node_id uuid NOT NULL REFERENCES nodes (id),
    instance_id uuid,
    profile_id uuid,
    profile_name text NOT NULL DEFAULT '',
    container_name text NOT NULL DEFAULT '',
    container_id text NOT NULL DEFAULT '',
    reason text NOT NULL,
    container_state text NOT NULL DEFAULT '',
    exit_code integer,
    oom_killed boolean NOT NULL DEFAULT false,
    started_at timestamptz,
    finished_at timestamptz,
    lines_requested integer,
    lines_kept integer NOT NULL,
    truncated boolean NOT NULL DEFAULT false,
    size_bytes bigint NOT NULL,
    log_gz bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX container_log_archives_created_at_idx ON container_log_archives (created_at DESC);
CREATE INDEX container_log_archives_instance_id_idx ON container_log_archives (instance_id);
