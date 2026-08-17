// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// userRoster is the subset of *rbac.Service this package needs for the
// Users & permissions page - the RBAC-gated full roster read, distinct
// from userLister's FindByID/List (ungated, used only to resolve an
// already-permitted audit record's actor_id to a display name - see
// audit.go). Exposing the full roster itself needs its own check, which
// is why this page goes through rbac.Service.ListUsers rather than
// a.users.List directly.
type userRoster interface {
	ListUsers(ctx context.Context, actor rbac.Actor) ([]*db.User, error)
}

// userElevator is the subset of *rbac.Service this package needs for the
// tier-change form - a separate, narrowly-scoped interface from
// userRoster even though the same concrete *rbac.Service satisfies both
// in production (cmd/sparky-server/main.go passes it for both
// parameters), matching this codebase's existing one-interface-per-need
// pattern (e.g. auditLister/transferLister staying separate despite
// similar shapes).
type userElevator interface {
	ElevateTier(ctx context.Context, actor rbac.Actor, targetUserID string, toTier db.Tier) error
}

// allTiers is every tier a user row can be shown or moved to, low to
// high - CLAUDE.md's own tier ordering (SCHEMA.md Users). Package-level
// rather than importing rbac_test.go's identical unexported var (a
// different package's test-only value, not reusable here).
var allTiers = []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev, db.TierAdmin}

// reachableTiers returns, in allTiers order, every tier actor could move
// a user currently at fromTier to - computed by asking rbac.CanElevate
// about each candidate rather than reimplementing its rules here, so the
// dropdown offered can never drift from the rule that actually decides
// whether a submission succeeds. fromTier itself is excluded - a
// same-tier "change" is a no-op ElevateTier would still accept and audit
// (rbac.CanElevate never special-cases actor.IsSuperAdmin against a
// same-tier transition), but offering it in the UI has no purpose.
func reachableTiers(actor rbac.Actor, fromTier db.Tier) []string {
	var reachable []string
	for _, candidate := range allTiers {
		if candidate == fromTier {
			continue
		}
		if rbac.CanElevate(actor, fromTier, candidate) {
			reachable = append(reachable, string(candidate))
		}
	}
	return reachable
}

// usersPageData is the Users & permissions page's view model - CLAUDE.md
// Frontend Conventions' sidebar tier ("Admin"), same floor as Audit log.
type usersPageData struct {
	Users []userRow
}

type userRow struct {
	ID             string
	DisplayName    string
	Tier           string
	LastLoginAt    string
	ElevatedBy     string
	ElevatedAt     string
	ReachableTiers []string
}

func (a *API) handleUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for users page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	users, err := a.userRoster.ListUsers(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err != nil:
		a.logger.Printf("httpapi: list users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// elevatedBy resolves to a display name via a second pass over the
	// same roster rather than a separate lookup per row - the roster is
	// already small (a handful of users, per CLAUDE.md Project Overview:
	// "a single internal team"), same map-of-names pattern used for node
	// names on Model profiles/Transfers and actor names on Audit log.
	names := make(map[string]string, len(users))
	for _, u := range users {
		names[u.ID] = u.DisplayName
	}

	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		var lastLogin string
		if u.LastLoginAt != nil {
			lastLogin = u.LastLoginAt.Format("2006-01-02 15:04:05 MST")
		}
		var elevatedBy, elevatedAt string
		if u.ElevatedBy != nil {
			if name, ok := names[*u.ElevatedBy]; ok {
				elevatedBy = name
			} else {
				elevatedBy = *u.ElevatedBy
			}
		}
		if u.ElevatedAt != nil {
			elevatedAt = u.ElevatedAt.Format("2006-01-02 15:04:05 MST")
		}
		rows = append(rows, userRow{
			ID:             u.ID,
			DisplayName:    u.DisplayName,
			Tier:           string(u.Tier),
			LastLoginAt:    lastLogin,
			ElevatedBy:     elevatedBy,
			ElevatedAt:     elevatedAt,
			ReachableTiers: reachableTiers(actor, u.Tier),
		})
	}

	a.render(w, r, "users", "Users & permissions", usersPageData{Users: rows})
}

// handleElevateUser is POST /users/{id}/tier - the Users & permissions
// page's tier-change form, the first write/action form in the Dashboard
// UI (PLANNING.md's Phase 8). The RBAC decision (whether actor may move
// this particular user from their current tier to the requested one) is
// not re-checked here - it lives entirely inside rbac.Service.ElevateTier
// itself (which looks up the target's current tier fresh, not trusting
// any value posted by the form), matching every other write path in this
// codebase.
func (a *API) handleElevateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	targetUserID := chi.URLParam(r, "id")

	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid form submission")
		return
	}
	toTier := db.Tier(r.PostFormValue("tier"))
	if !isKnownTier(toTier) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unknown tier")
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for elevate user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err = a.elevator.ElevateTier(ctx, actor, targetUserID, toTier)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "not permitted to make this tier change")
		return
	case errors.Is(err, db.ErrUserNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	case err != nil:
		a.logger.Printf("httpapi: elevate user %s: %v", targetUserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Same blunt full-navigation pattern handleLogout already uses for
	// its own post-action redirect, rather than re-rendering just the
	// affected row - the simplest option, consistent with this being the
	// Dashboard UI's very first write action and there being no existing
	// per-row-partial-render path yet to extend instead.
	w.Header().Set("HX-Redirect", "/users")
	w.WriteHeader(http.StatusNoContent)
}

func isKnownTier(t db.Tier) bool {
	for _, candidate := range allTiers {
		if t == candidate {
			return true
		}
	}
	return false
}
