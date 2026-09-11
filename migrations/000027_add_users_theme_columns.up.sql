-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Per-user theme preference - see SCHEMA.md Users. theme_preset NULL means
-- "inherit theme_settings.default_theme" (SCHEMA.md Theme settings), not a
-- hardcoded fallback in Go, per the Admin-configurable-default requirement.
-- theme_custom_colors is a sparse allowlisted-key JSONB map (validated at
-- the rbac service layer, not here - same reasoning as model_profiles'
-- engine_params), empty object meaning "no customization active," so a
-- user who has never customized never needs a NULL-vs-empty distinction.
-- theme_status_palette NULL means "auto-derive from the resolved preset's
-- family" (SCHEMA.md Theme settings); non-null is an explicit override,
-- only ever set once a user is customizing.
ALTER TABLE users ADD COLUMN theme_preset theme_preset;
ALTER TABLE users ADD COLUMN theme_custom_colors jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE users ADD COLUMN theme_status_palette theme_status_palette;
