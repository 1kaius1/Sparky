// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// userLister is the subset of *db.UserRepository this package needs:
// FindByID to resolve the current session's own tier for an RBAC
// decision (the session cookie deliberately doesn't carry it - see
// internal/session's doc comment), and List to resolve every audit
// record's actor_id to a display name, the same map-of-names pattern
// already used for node names on the Model profiles and Transfers pages.
type userLister interface {
	FindByID(ctx context.Context, id string) (*db.User, error)
	List(ctx context.Context) ([]*db.User, error)
}

// auditLister is the subset of *audit.Recorder this package needs.
type auditLister interface {
	List(ctx context.Context, actor rbac.Actor) ([]*db.AuditRecord, error)
}

// auditPageData is the Audit log page's view model - CLAUDE.md Frontend
// Conventions' Audit log sidebar tier ("Admin"), the first Dashboard UI
// page whose floor sits above every other read-only page's Read-only
// floor.
type auditPageData struct {
	Records []auditRow
}

type auditRow struct {
	Actor      string
	Action     string
	ObjectType string
	ObjectID   string
	Detail     string
	CreatedAt  string
}

// actorFromIdentity resolves a full rbac.Actor from the session's
// Identity (UserID + IsSuperAdmin only) by looking up the user's current
// tier - the session cookie deliberately doesn't carry it, so every
// RBAC-gated handler resolves it fresh here rather than trusting a
// cached value that could be stale after an elevation.
func (a *API) actorFromIdentity(ctx context.Context, identity Identity) (rbac.Actor, error) {
	if identity.IsSuperAdmin {
		return rbac.Actor{IsSuperAdmin: true}, nil
	}
	user, err := a.users.FindByID(ctx, identity.UserID)
	if err != nil {
		return rbac.Actor{}, fmt.Errorf("look up user %s: %w", identity.UserID, err)
	}
	return rbac.Actor{UserID: user.ID, Tier: user.Tier}, nil
}

func (a *API) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for audit log: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	records, err := a.audit.List(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case err != nil:
		a.logger.Printf("httpapi: list audit records: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	users, err := a.users.List(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list users for audit log: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		names[u.ID] = u.DisplayName
	}

	rows := make([]auditRow, 0, len(records))
	for _, rec := range records {
		actorLabel := "SuperAdmin"
		if !rec.IsSuperAdminAction && rec.ActorID != nil {
			if name, ok := names[*rec.ActorID]; ok {
				actorLabel = name
			} else {
				actorLabel = *rec.ActorID
			}
		}
		var detail string
		if len(rec.Detail) > 0 {
			detail = string(rec.Detail)
		}
		rows = append(rows, auditRow{
			Actor:      actorLabel,
			Action:     rec.Action,
			ObjectType: rec.ObjectType,
			ObjectID:   rec.ObjectID,
			Detail:     detail,
			CreatedAt:  rec.CreatedAt.Format("2006-01-02 15:04:05 MST"),
		})
	}

	a.render(w, r, "audit", "Audit log", auditPageData{Records: rows})
}
