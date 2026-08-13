// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// settingsViewer is the subset of *settings.Service this package needs -
// see that package's own doc comment for why the RBAC-gated read for
// both singleton config rows lives there rather than in
// internal/metrics or internal/audit. Returns plain *db types, not a
// settings-package type, so this package doesn't need to import
// internal/settings at all - same reasoning as auditLister/
// transferLister/userRoster.
type settingsViewer interface {
	Get(ctx context.Context, actor rbac.Actor) (*db.MetricsExportConfig, *db.AuditSettings, error)
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
}

// resolveUserName resolves userID to a display name via FindByID,
// falling back to the raw ID if the lookup fails - same fallback every
// other page's map-of-names resolution already uses (Audit log, Users &
// permissions) for a since-deleted or otherwise unresolvable reference.
// A single FindByID rather than a full List, unlike those two pages: at
// most two IDs need resolving here (one per config row), not a whole
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

	metricsExport, auditSettings, err := a.settings.Get(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
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

	data := settingsPageData{
		MetricsExportBackend:   string(metricsExport.BackendType),
		MetricsExportUpdatedBy: a.resolveUserName(ctx, metricsExport.UpdatedBy),
		MetricsExportUpdatedAt: metricsExport.UpdatedAt.Format("2006-01-02 15:04:05 MST"),

		AuditRetentionMonths:      auditSettings.RetentionMonths,
		AuditForwardingEnabled:    auditSettings.ForwardingEnabled,
		AuditForwardingProtocol:   string(auditSettings.ForwardingProtocol),
		AuditForwardingHost:       forwardingHost,
		AuditForwardingPort:       forwardingPort,
		AuditForwardingTLSEnabled: auditSettings.ForwardingTLSEnabled,
		AuditUpdatedBy:            a.resolveUserName(ctx, auditSettings.UpdatedBy),
		AuditUpdatedAt:            auditSettings.UpdatedAt.Format("2006-01-02 15:04:05 MST"),
	}

	a.render(w, r, "settings", "Settings", data)
}
