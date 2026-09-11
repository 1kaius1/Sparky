-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Theme system - see SCHEMA.md Users and Theme settings, ARCHITECTURE.md.
-- theme_preset is shared by both Users.theme_preset (a user's own choice,
-- nullable = inherit the system default below) and this singleton row's
-- default_theme (never null - always a concrete base preset, even when an
-- Admin has uploaded a fully custom default via YAML) - defined once here
-- since theme_settings is the "owning" table for the concept of a preset
-- catalog, same reasoning nodes.go owns runtime_backend even though other
-- tables reference it. theme_status_palette is the explicit, Slack-style
-- override a customizing user (or an Admin's uploaded default) can set
-- independent of the base preset's own light/dark family - see SCHEMA.md
-- Users.
CREATE TYPE theme_preset AS ENUM (
    'slate-light', 'linen-light', 'arctic-light', 'sand-light',
    'carbon-dark', 'matrix-dark', 'tron-dark', 'amethyst-dark'
);

CREATE TYPE theme_status_palette AS ENUM ('light', 'dark');

-- Singleton settings row, same boolean-primary-key-plus-CHECK pattern as
-- metrics_export_config/audit_settings - seeded here with carbon-dark (the
-- system-wide shipped default) so every page render always has an
-- effective default to fall back to for a user with no preference set.
-- default_custom_colors/default_status_palette mirror the same triple
-- shape users.theme_preset/theme_custom_colors/theme_status_palette use
-- (see migration 000027) - an Admin-uploaded custom default (SCHEMA.md
-- Theme settings) is represented identically to a user's own
-- customization, layered on default_theme the same way.
CREATE TABLE theme_settings (
    id boolean PRIMARY KEY DEFAULT true,
    default_theme theme_preset NOT NULL DEFAULT 'carbon-dark',
    default_custom_colors jsonb NOT NULL DEFAULT '{}'::jsonb,
    default_status_palette theme_status_palette,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT theme_settings_singleton CHECK (id)
);

INSERT INTO theme_settings (id) VALUES (true);
