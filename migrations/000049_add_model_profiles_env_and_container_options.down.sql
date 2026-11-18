-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Put the shared-memory size back where the old vLLM adapter reads it. Only
-- that one setting has somewhere to go: every other container option, and all
-- environment variables, are lost with the columns.
UPDATE model_profiles
   SET engine_params = engine_params || jsonb_build_object('shm_size_gb', container_options -> 'shm_size_gb')
 WHERE engine_type = 'vllm'
   AND container_options ? 'shm_size_gb';

ALTER TABLE model_profiles
    DROP CONSTRAINT model_profiles_container_options_is_object,
    DROP CONSTRAINT model_profiles_env_is_object,
    DROP COLUMN container_options,
    DROP COLUMN env;
