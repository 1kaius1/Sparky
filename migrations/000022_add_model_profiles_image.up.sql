-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Per-profile container image override - see SCHEMA.md Model profiles and
-- PLANNING.md's Decisions Log. NULL means "use the engine type's own
-- default image" (today's unchanged behavior - each internal/engines
-- adapter's hardcoded image constant). A non-empty value overrides that
-- default for the containers (Docker/Podman) runtime backend only - a
-- bare-metal launch never uses a container image at all, so this column
-- is silently inert there, same "harmless when inapplicable" precedent as
-- quantization on a vLLM/Aphrodite profile. No CHECK constraint - any
-- non-empty string is accepted, and a bad image reference fails clearly
-- at container pull/create time instead of at save time, the same
-- "attempt and report failure" philosophy already established for
-- engine_version/quantization (see 000017_add_model_profiles_engine_version,
-- 000019_add_model_profiles_quantization).

ALTER TABLE model_profiles ADD COLUMN image text;
