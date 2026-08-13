// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

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

// usersPageData is the Users & permissions page's view model - CLAUDE.md
// Frontend Conventions' sidebar tier ("Admin"), same floor as Audit log.
type usersPageData struct {
	Users []userRow
}

type userRow struct {
	DisplayName string
	Tier        string
	LastLoginAt string
	ElevatedBy  string
	ElevatedAt  string
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
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
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
			DisplayName: u.DisplayName,
			Tier:        string(u.Tier),
			LastLoginAt: lastLogin,
			ElevatedBy:  elevatedBy,
			ElevatedAt:  elevatedAt,
		})
	}

	a.render(w, r, "users", "Users & permissions", usersPageData{Users: rows})
}
