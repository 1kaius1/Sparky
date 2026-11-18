-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Per-profile environment variables and container-runtime options - see
-- SCHEMA.md Model profiles and internal/launchopts. Both are opaque to the
-- database and validated in Go, the same split as engine_params; the CHECKs
-- only keep each to a JSON object so a bad write cannot leave a value the
-- application cannot decode.
--
--   env                a JSON object of NAME -> text value, put in the engine
--                      process's environment on either runtime backend
--   container_options  a JSON object of docker/podman-run-style settings
--                      (agentproto.ContainerOptions), containers backend only
--
-- Kept apart from engine_params because they are not engine flags and have
-- different permission floors: the host-level part of container_options
-- (mounts, devices, privileged, ...) is an Admin's to set.

ALTER TABLE model_profiles
    ADD COLUMN env jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN container_options jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT model_profiles_env_is_object CHECK (jsonb_typeof(env) = 'object'),
    ADD CONSTRAINT model_profiles_container_options_is_object CHECK (jsonb_typeof(container_options) = 'object');

-- shm_size_gb was a vLLM engine_params key that implied the host IPC
-- namespace. It is a container setting, so it moves to container_options as
-- the pair it always meant (shm_size_gb plus ipc_mode "host"), and the engine
-- adapter stops accepting it.
UPDATE model_profiles
   SET container_options = jsonb_build_object(
           'shm_size_gb', engine_params -> 'shm_size_gb',
           'ipc_mode', 'host'),
       engine_params = engine_params - 'shm_size_gb'
 WHERE engine_params ? 'shm_size_gb';
