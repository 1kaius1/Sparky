// SPDX-License-Identifier: AGPL-3.0-or-later

// Package settings is the RBAC-gated read/write path for the Settings
// page's singleton config rows - Metrics export config, Audit settings,
// and Theme settings (see SCHEMA.md for all three). None of these rows
// belongs to an existing Service's domain: internal/metrics.Service is
// scoped to telemetry ingestion only (its own doc comment defers NFS/S3
// export itself to the v0.4.0 Historical metrics milestone),
// internal/audit.Recorder is scoped to the audit_log table, not the
// separate audit_settings table that configures its optional forwarding,
// and the Admin-configurable default theme is Settings-page state, not
// internal/rbac's concern (that package owns a user's own theme
// preference instead - see UpdateDefaultTheme below). This package exists
// so the single RBAC decision behind the Settings page ("may this actor
// view/change it at all") has one home, rather than being split across
// unrelated packages for one page's data.
package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// metricsExportStore is the subset of *db.MetricsExportConfigRepository
// this package needs, narrow enough to fake in tests without a real
// Postgres instance - same pattern used throughout internal/httpapi and
// internal/rbac.
type metricsExportStore interface {
	Get(ctx context.Context) (*db.MetricsExportConfig, error)
}

// auditSettingsStore is the subset of *db.AuditSettingsRepository this
// package needs.
type auditSettingsStore interface {
	Get(ctx context.Context) (*db.AuditSettings, error)
}

// themeSettingsStore is the subset of *db.ThemeSettingsRepository this
// package needs for the Admin-configurable default theme's write path -
// see UpdateDefaultTheme. The read path used to resolve every viewer's
// own effective theme (internal/httpapi/render.go's resolveTheme) goes
// directly to *db.ThemeSettingsRepository instead, deliberately bypassing
// this package's RBAC gate - see that repository's own Get doc comment.
type themeSettingsStore interface {
	Get(ctx context.Context) (*db.ThemeSettings, error)
	Update(ctx context.Context, defaultTheme db.ThemePreset, defaultCustomColors json.RawMessage, defaultStatusPalette *db.ThemeStatusPalette, updatedBy *string) error
}

// auditRecorder is the subset of *audit.Recorder this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/rbac's own local copy of the same shape. This
// package had no write path before UpdateDefaultTheme, so no audit
// dependency existed here until now.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is the Settings page's orchestration layer - callers never
// access *db.MetricsExportConfigRepository/*db.AuditSettingsRepository/
// *db.ThemeSettingsRepository directly, matching the Handler -> Service
// Layer -> Repository pattern CLAUDE.md establishes elsewhere.
type Service struct {
	metricsExport metricsExportStore
	auditSettings auditSettingsStore
	themeSettings themeSettingsStore
	audit         auditRecorder
}

// NewService constructs a Service.
func NewService(metricsExport metricsExportStore, auditSettings auditSettingsStore, themeSettings themeSettingsStore, audit auditRecorder) *Service {
	return &Service{metricsExport: metricsExport, auditSettings: auditSettings, themeSettings: themeSettings, audit: audit}
}

// Get returns all three singleton config rows if actor is permitted to
// view the Settings page - see rbac.CanViewSettings. The RBAC check lives
// here, not only at the HTTP layer, matching audit.Recorder.List's and
// rbac.Service.ListUsers's own reasoning: the guarantee travels with the
// method regardless of caller. Returns rbac.ErrNotPermitted if actor is
// not permitted; the rows are returned as plain *db types rather than a
// wrapping struct so internal/httpapi doesn't need to import this
// package's own types, matching how auditLister/transferLister/userRoster
// are defined there against db types alone.
func (s *Service) Get(ctx context.Context, actor rbac.Actor) (*db.MetricsExportConfig, *db.AuditSettings, *db.ThemeSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, nil, nil, rbac.ErrNotPermitted
	}

	metricsExport, err := s.metricsExport.Get(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get metrics export config: %w", err)
	}
	auditSettings, err := s.auditSettings.Get(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get audit settings: %w", err)
	}
	themeSettings, err := s.themeSettings.Get(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get theme settings: %w", err)
	}
	return metricsExport, auditSettings, themeSettings, nil
}

// themeSettingsAuditObjectID stands in for theme_settings' own row in an
// audit_log entry - that table's object_id column is a real uuid (SCHEMA.md
// Audit log), not free text, so a descriptive string like "singleton"
// isn't a valid value; the conventional all-zero nil UUID is used instead,
// same as any other singleton-shaped resource with no natural row ID of
// its own.
const themeSettingsAuditObjectID = "00000000-0000-0000-0000-000000000000"

// UpdateDefaultTheme changes the system-wide default theme (the
// theme_settings singleton row) - Admin/SuperAdmin only, reusing
// rbac.CanViewSettings as the write gate too: the Settings page has
// always been one all-or-nothing Admin gate, with no separate "manage"
// tier the way, say, Nodes distinguishes read-only view from Admin edit.
//
// Serves both the plain preset-dropdown case (customColors/statusPalette
// nil/empty) and the YAML-upload case (populated) through one path - see
// internal/httpapi/settings.go's two handlers. Deliberately the same
// shape as rbac.Service.UpdateOwnTheme, including the same
// capture-before/revert-on-audit-failure pattern as ElevateTier
// (internal/rbac/service.go): every state-changing action must be
// audited, no exceptions, so an audit-write failure here reverts the
// persisted default rather than leaving an unaudited change in place.
func (s *Service) UpdateDefaultTheme(ctx context.Context, actor rbac.Actor, preset db.ThemePreset, customColors map[string]string, statusPalette *db.ThemeStatusPalette) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if !rbac.ValidThemePreset(preset) {
		return fmt.Errorf("%w: unknown preset %q", rbac.ErrInvalidTheme, preset)
	}
	if statusPalette != nil && !rbac.ValidThemeStatusPalette(*statusPalette) {
		return fmt.Errorf("%w: unknown status palette %q", rbac.ErrInvalidTheme, *statusPalette)
	}
	colorsJSON, err := rbac.ValidateThemeCustomColors(customColors)
	if err != nil {
		return err
	}

	current, err := s.themeSettings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current theme settings: %w", err)
	}
	fromPreset := current.DefaultTheme
	fromCustomColors := current.DefaultCustomColors
	fromStatusPalette := current.DefaultStatusPalette

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}

	if err := s.themeSettings.Update(ctx, preset, colorsJSON, statusPalette, actorID); err != nil {
		return fmt.Errorf("update theme settings: %w", err)
	}

	detail := map[string]any{
		"default_theme":      string(preset),
		"custom_color_count": len(customColors),
	}
	if statusPalette != nil {
		detail["status_palette"] = string(*statusPalette)
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_default_theme", "theme_settings", themeSettingsAuditObjectID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.themeSettings.Update(ctx, fromPreset, fromCustomColors, fromStatusPalette, current.UpdatedBy); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}
