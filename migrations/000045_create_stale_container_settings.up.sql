-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Stale container monitoring settings - see SCHEMA.md Stale container
-- settings. Singleton settings row, same pattern as container_log_settings:
-- seeded here with monitoring off, so the scheduler always has a setting to
-- read and "not configured yet" is never a state distinct from "None"
-- (PLANNING.md Decisions Log, 2026-10-26).
--
-- mode: none (the default) does nothing; report scans on the schedule and
-- raises alerts; prune scans on the schedule and removes stale containers
-- after archiving their logs.
--
-- schedule is an internal/schedule Spec as JSON, opaque to the database and
-- validated in Go like model_profiles.engine_params. It is required whenever
-- the mode is not none. schedule_set_at is when it was last saved, which a
-- weekly schedule counts from (it never runs an occurrence earlier than its
-- own save) and an every-N-days schedule runs its first scan at.
--
-- last_run_at is stamped when a scheduled run begins, before it does anything,
-- so a crash mid-run cannot make it run twice. last_run_summary is what that
-- run found and did, for the Settings page.
--
-- min_age_hours is how long a stopped container must have been stopped before
-- it counts as stale: the grace period that keeps a crash's evidence in place
-- for whoever is looking into it. archive_lines is how many trailing lines of
-- a container's log a scheduled prune archives before removing it; 0 means
-- all that is held.
CREATE TYPE stale_container_mode AS ENUM ('none', 'report', 'prune');

CREATE TABLE stale_container_settings (
    id boolean PRIMARY KEY DEFAULT true,
    mode stale_container_mode NOT NULL DEFAULT 'none',
    schedule jsonb,
    schedule_set_at timestamptz NOT NULL DEFAULT now(),
    last_run_at timestamptz,
    last_run_summary jsonb,
    min_age_hours integer NOT NULL DEFAULT 24,
    archive_lines integer NOT NULL DEFAULT 2000,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT stale_container_settings_singleton CHECK (id),
    CONSTRAINT stale_container_settings_schedule_required CHECK (mode = 'none' OR schedule IS NOT NULL),
    CONSTRAINT stale_container_settings_min_age_range CHECK (min_age_hours >= 0 AND min_age_hours <= 720),
    CONSTRAINT stale_container_settings_archive_lines_range CHECK (archive_lines >= 0 AND archive_lines <= 100000)
);

INSERT INTO stale_container_settings (id) VALUES (true);
