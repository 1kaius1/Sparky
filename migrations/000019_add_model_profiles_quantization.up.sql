-- SPDX-License-Identifier: AGPL-3.0-or-later

-- GGUF quantization selection - see SCHEMA.md Model profiles and
-- PLANNING.md's Decisions Log. NULL means "not applicable" (vLLM/Aphrodite
-- profiles, or a llama.cpp profile whose repo has only one .gguf file) -
-- today's behavior, unchanged. No CHECK constraint - any non-empty string
-- is accepted, and a bad value fails clearly at download/launch time
-- instead of at save time (confirmed with the user, matching
-- engine_version's own "attempt and report failure" philosophy - see
-- 000017_add_model_profiles_engine_version).

ALTER TABLE model_profiles ADD COLUMN quantization text;
