-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Adds a free-text operator note to a node - e.g. which team/model is
-- currently using it - ahead of (and independent from) the future
-- profile-locking/scheduling system (see PLANNING.md Future Ideas).
-- Deliberately opaque to the database: no structured meaning, validated
-- only for a max length, at the Go service layer
-- (internal/nodes.Service.SetComment), same "validate in Go" split as
-- engine_params/theme_custom_colors.
--
-- comment_updated_by/comment_updated_at record who last changed it and
-- when, stamped on every change including clearing it back to empty - same
-- null-only-for-the-break-glass-SuperAdmin convention as nodes'
-- registered_by.
ALTER TABLE nodes ADD COLUMN comment text;
ALTER TABLE nodes ADD COLUMN comment_updated_by uuid REFERENCES users (id);
ALTER TABLE nodes ADD COLUMN comment_updated_at timestamptz;
