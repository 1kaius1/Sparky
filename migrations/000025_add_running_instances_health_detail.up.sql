-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Adds a place to record the load-derived signal a periodic instance
-- health check can read alongside its plain healthy/unhealthy verdict -
-- see SCHEMA.md Running instances' health_detail and PLANNING.md's
-- Decisions Log for the full design (agent-side readiness + periodic
-- health-check feature). A flexible JSONB blob, not fixed columns, since
-- different engine types expose genuinely different metric names (or
-- none at all) via their own Prometheus-style /metrics endpoint - same
-- "deliberately opaque, engine-specific shape" reasoning as
-- model_profiles.engine_params.

ALTER TABLE running_instances ADD COLUMN health_detail jsonb;
