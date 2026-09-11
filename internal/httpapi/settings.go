// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"gopkg.in/yaml.v3"
)

// settingsViewer is the subset of *settings.Service this package needs -
// see that package's own doc comment for why the RBAC-gated read and
// (for the theme default) write for all three singleton config rows
// lives there rather than in internal/metrics or internal/audit. Returns
// plain *db types, not a settings-package type, so this package doesn't
// need to import internal/settings at all - same reasoning as
// auditLister/transferLister/userRoster.
type settingsViewer interface {
	Get(ctx context.Context, actor rbac.Actor) (*db.MetricsExportConfig, *db.AuditSettings, *db.ThemeSettings, error)
	UpdateDefaultTheme(ctx context.Context, actor rbac.Actor, preset db.ThemePreset, customColors map[string]string, statusPalette *db.ThemeStatusPalette) error
}

// settingsPageData is the Settings page's view model - CLAUDE.md
// Frontend Conventions' sidebar tier ("Admin"), same floor as Audit log
// and Users & permissions.
type settingsPageData struct {
	MetricsExportBackend   string
	MetricsExportUpdatedBy string
	MetricsExportUpdatedAt string

	AuditRetentionMonths      int
	AuditForwardingEnabled    bool
	AuditForwardingProtocol   string
	AuditForwardingHost       string
	AuditForwardingPort       string
	AuditForwardingTLSEnabled bool
	AuditUpdatedBy            string
	AuditUpdatedAt            string

	DefaultTheme          string
	DefaultThemeUpdatedBy string
	DefaultThemeUpdatedAt string
	AvailablePresets      []themePresetOption
	ThemeError            string
	ThemeSuccess          bool
}

// resolveUserName resolves userID to a display name via FindByID,
// falling back to the raw ID if the lookup fails - same fallback every
// other page's map-of-names resolution already uses (Audit log, Users &
// permissions) for a since-deleted or otherwise unresolvable reference.
// A single FindByID rather than a full List, unlike those two pages: at
// most three IDs need resolving here (one per config row), not a whole
// table's worth.
func (a *API) resolveUserName(ctx context.Context, userID *string) string {
	if userID == nil {
		return ""
	}
	user, err := a.users.FindByID(ctx, *userID)
	if err != nil {
		return *userID
	}
	return user.DisplayName
}

func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for settings page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.renderSettingsPage(w, r, actor, settingsPageData{})
}

// renderSettingsPage assembles the full view model and renders the
// Settings page - factored out of handleSettings so the two new theme
// write handlers below (the plain dropdown and the YAML upload) can share
// the exact same redisplay-on-error/success path, mirroring account.go's
// own renderAccountPage. extra carries only the ThemeError/ThemeSuccess a
// caller wants layered on top of the freshly-read config rows.
func (a *API) renderSettingsPage(w http.ResponseWriter, r *http.Request, actor rbac.Actor, extra settingsPageData) {
	ctx := r.Context()

	metricsExport, auditSettings, themeSettings, err := a.settings.Get(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err != nil:
		a.logger.Printf("httpapi: get settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var forwardingHost, forwardingPort string
	if auditSettings.ForwardingHost != nil {
		forwardingHost = *auditSettings.ForwardingHost
	}
	if auditSettings.ForwardingPort != nil {
		forwardingPort = strconv.Itoa(*auditSettings.ForwardingPort)
	}

	data := extra
	data.MetricsExportBackend = string(metricsExport.BackendType)
	data.MetricsExportUpdatedBy = a.resolveUserName(ctx, metricsExport.UpdatedBy)
	data.MetricsExportUpdatedAt = metricsExport.UpdatedAt.Format("2006-01-02 15:04:05 MST")

	data.AuditRetentionMonths = auditSettings.RetentionMonths
	data.AuditForwardingEnabled = auditSettings.ForwardingEnabled
	data.AuditForwardingProtocol = string(auditSettings.ForwardingProtocol)
	data.AuditForwardingHost = forwardingHost
	data.AuditForwardingPort = forwardingPort
	data.AuditForwardingTLSEnabled = auditSettings.ForwardingTLSEnabled
	data.AuditUpdatedBy = a.resolveUserName(ctx, auditSettings.UpdatedBy)
	data.AuditUpdatedAt = auditSettings.UpdatedAt.Format("2006-01-02 15:04:05 MST")

	data.DefaultTheme = string(themeSettings.DefaultTheme)
	data.DefaultThemeUpdatedBy = a.resolveUserName(ctx, themeSettings.UpdatedBy)
	data.DefaultThemeUpdatedAt = themeSettings.UpdatedAt.Format("2006-01-02 15:04:05 MST")
	data.AvailablePresets = themePresetOptions()

	a.render(w, r, "settings", "Settings", data)
}

// handleUpdateDefaultTheme sets the system-wide default theme to a plain
// stock preset, with no customization - see settings.Service.
// UpdateDefaultTheme. The YAML upload path below (handleUploadDefaultTheme)
// is the alternative entry point for a fully custom default.
func (a *API) handleUpdateDefaultTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for default theme update: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "invalid form submission"})
		return
	}
	preset := db.ThemePreset(r.PostFormValue("default_theme"))

	err = a.settings.UpdateDefaultTheme(ctx, actor, preset, nil, nil)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, rbac.ErrInvalidTheme):
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: update default theme: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "failed to update default theme"})
		return
	}

	a.renderSettingsPage(w, r, actor, settingsPageData{ThemeSuccess: true})
}

// themeYAMLFile is the expected shape of an uploaded custom-theme file -
// see SCHEMA.md Theme settings for the documented example. KnownFields
// strict decoding (below) rejects any key outside these three, so a typo
// in an Admin's file fails clearly rather than being silently ignored.
type themeYAMLFile struct {
	BasePreset    string            `yaml:"base_preset"`
	StatusPalette string            `yaml:"status_palette,omitempty"`
	Colors        map[string]string `yaml:"colors,omitempty"`
}

// maxThemeYAMLBytes bounds the uploaded file regardless of what the YAML
// library does internally - generous for a file that's just a couple
// dozen flat key/value lines.
const maxThemeYAMLBytes = 16 << 10

// handleUploadDefaultTheme sets the system-wide default theme to a fully
// custom color set, layered on a required base preset - the YAML
// alternative to handleUpdateDefaultTheme's plain dropdown. A decode or
// validation failure always redisplays the Settings page with a
// ThemeError, never a 500, since this is untrusted admin-supplied file
// content.
func (a *API) handleUploadDefaultTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for default theme upload: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// r.Body is already wrapped in http.MaxBytesReader by the route's
	// limitBody middleware (router.go) - RequireCSRF's own multipart
	// parse needs that cap in place too, not just this handler's, so it
	// lives ahead of both rather than being set here.
	if err := r.ParseMultipartForm(maxThemeYAMLBytes); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "theme file is invalid or too large"})
		return
	}
	file, _, err := r.FormFile("theme_file")
	if err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "no theme file provided"})
		return
	}
	defer file.Close()

	var parsed themeYAMLFile
	dec := yaml.NewDecoder(file)
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "could not parse theme file: " + err.Error()})
		return
	}

	colors := make(map[string]string, len(parsed.Colors))
	for slug, value := range parsed.Colors {
		cssVar, ok := rbac.CSSVarForThemeColorSlug(slug)
		if !ok {
			a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "unknown color key: " + slug})
			return
		}
		colors[cssVar] = value
	}

	var statusPalette *db.ThemeStatusPalette
	if parsed.StatusPalette != "" {
		sp := db.ThemeStatusPalette(parsed.StatusPalette)
		statusPalette = &sp
	}

	err = a.settings.UpdateDefaultTheme(ctx, actor, db.ThemePreset(parsed.BasePreset), colors, statusPalette)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, rbac.ErrInvalidTheme):
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: upload default theme: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{ThemeError: "failed to apply uploaded theme"})
		return
	}

	a.renderSettingsPage(w, r, actor, settingsPageData{ThemeSuccess: true})
}
