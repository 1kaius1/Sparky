// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ThemeSettings mirrors the theme_settings table - see SCHEMA.md Theme
// settings. A singleton row, always present as of migration 000026
// (seeded with default_theme = 'carbon-dark') - same always-present
// reasoning as MetricsExportConfig. DefaultCustomColors/
// DefaultStatusPalette mirror User's own ThemeCustomColors/
// ThemeStatusPalette fields exactly - the system-wide default is
// representable as the identical (preset, custom colors, status palette)
// triple as a user's own preference, which is what lets an Admin's
// YAML-uploaded custom default (internal/httpapi's theme upload handler)
// reuse the same shape and validation as a user's own customization.
type ThemeSettings struct {
	DefaultTheme         ThemePreset
	DefaultCustomColors  json.RawMessage
	DefaultStatusPalette *ThemeStatusPalette
	UpdatedBy            *string
	UpdatedAt            time.Time
}

// ThemeSettingsRepository is the only component that queries the
// theme_settings table directly.
type ThemeSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewThemeSettingsRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewThemeSettingsRepository(pool *pgxpool.Pool) *ThemeSettingsRepository {
	return &ThemeSettingsRepository{pool: pool}
}

// Get returns the current system-wide default theme. Unlike
// MetricsExportConfig/AuditSettings, this is read on every full page
// render for every viewer (to resolve a user with no preference set), not
// just the Settings page - see internal/httpapi/render.go's
// resolveTheme - so, deliberately, no RBAC gate belongs here or in any
// caller of this method: knowing the system default theme's value is not
// privileged information, since every viewer already sees its effect.
func (r *ThemeSettingsRepository) Get(ctx context.Context) (*ThemeSettings, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT default_theme, default_custom_colors, default_status_palette, updated_by, updated_at FROM theme_settings WHERE id = true`)

	var s ThemeSettings
	if err := row.Scan(&s.DefaultTheme, &s.DefaultCustomColors, &s.DefaultStatusPalette, &s.UpdatedBy, &s.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get theme settings: %w", err)
	}
	return &s, nil
}

// Update sets the system-wide default theme - the Settings page's write
// path (both the plain preset dropdown and the YAML custom-theme upload
// go through this one method), gated by rbac.CanViewSettings in
// internal/settings.Service, not here. updatedBy is nil when the
// break-glass SuperAdmin makes the change - same reasoning as
// MetricsExportConfig/AuditSettings.
func (r *ThemeSettingsRepository) Update(ctx context.Context, defaultTheme ThemePreset, defaultCustomColors json.RawMessage, defaultStatusPalette *ThemeStatusPalette, updatedBy *string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE theme_settings SET default_theme = $1, default_custom_colors = $2, default_status_palette = $3, updated_by = $4, updated_at = now() WHERE id = true`,
		defaultTheme, defaultCustomColors, defaultStatusPalette, updatedBy)
	if err != nil {
		return fmt.Errorf("update theme settings: %w", err)
	}
	return nil
}
