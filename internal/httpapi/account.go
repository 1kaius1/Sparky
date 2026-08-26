// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/1kaius1/Sparky/internal/rbac"
)

// selfAccountManager is the subset of *rbac.Service this package needs for
// the self-service account page - a distinct, narrowly-scoped interface
// from localAccountManager even though the same concrete *rbac.Service
// satisfies both in production: localAccountManager acts on another user
// (Admin-driven), this one only ever acts on the caller's own row.
type selfAccountManager interface {
	UpdateDisplayName(ctx context.Context, actor rbac.Actor, displayName string) error
	ChangeOwnPassword(ctx context.Context, actor rbac.Actor, currentPassword, newPassword string) error
}

// accountPageData is the self-service account page's view model - always
// mounted (GET/POST /account), but only functional for a local-account
// session (IsLocalAccount): a non-local session sees a minimal read-only
// notice instead of the edit forms, since an AD-backed display name is
// refreshed from AD at every login and Sparky never holds that password.
// The two forms (display name, change password) are independent - each
// POST redisplays this same page with its own Error/Success rather than
// redirecting, same reasoning as login.html's own redisplay-on-failure
// (this project has no flash-message/session-based mechanism to carry a
// message across a redirect).
type accountPageData struct {
	IsLocalAccount bool
	DisplayName    string

	DisplayNameError   string
	DisplayNameSuccess bool

	PasswordError   string
	PasswordSuccess bool
}

func (a *API) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	a.renderAccountPage(w, r, identity, accountPageData{})
}

// renderAccountPage fills in DisplayName from the current user row (for a
// local-account session) before rendering, so a redisplay after either
// form's failure still shows the caller's real current display name, not
// an empty field.
func (a *API) renderAccountPage(w http.ResponseWriter, r *http.Request, identity Identity, data accountPageData) {
	data.IsLocalAccount = identity.IsLocalAccount
	if identity.IsLocalAccount {
		user, err := a.users.FindByID(r.Context(), identity.UserID)
		if err != nil {
			a.logger.Printf("httpapi: look up user for account page: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data.DisplayName = user.DisplayName
	}
	a.render(w, r, "account", "My account", data)
}

func (a *API) handleUpdateDisplayName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if !identity.IsLocalAccount {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "not a local account")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderAccountPage(w, r, identity, accountPageData{DisplayNameError: "invalid form submission"})
		return
	}
	displayName := r.PostFormValue("display_name")
	if displayName == "" {
		a.renderAccountPage(w, r, identity, accountPageData{DisplayNameError: "display name is required"})
		return
	}

	err := a.selfAccount.UpdateDisplayName(ctx, rbac.Actor{UserID: identity.UserID}, displayName)
	switch {
	case errors.Is(err, rbac.ErrNotLocalAccount):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "not a local account")
		return
	case err != nil:
		a.logger.Printf("httpapi: update display name for %s: %v", identity.UserID, err)
		a.renderAccountPage(w, r, identity, accountPageData{DisplayNameError: "failed to update display name"})
		return
	}

	a.renderAccountPage(w, r, identity, accountPageData{DisplayNameSuccess: true})
}

func (a *API) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if !identity.IsLocalAccount {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "not a local account")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderAccountPage(w, r, identity, accountPageData{PasswordError: "invalid form submission"})
		return
	}
	currentPassword := r.PostFormValue("current_password")
	newPassword := r.PostFormValue("new_password")
	confirmPassword := r.PostFormValue("confirm_password")
	if currentPassword == "" || newPassword == "" || confirmPassword == "" {
		a.renderAccountPage(w, r, identity, accountPageData{PasswordError: "all fields are required"})
		return
	}
	if newPassword != confirmPassword {
		a.renderAccountPage(w, r, identity, accountPageData{PasswordError: "new password and confirmation do not match"})
		return
	}

	err := a.selfAccount.ChangeOwnPassword(ctx, rbac.Actor{UserID: identity.UserID}, currentPassword, newPassword)
	switch {
	case errors.Is(err, rbac.ErrWrongPassword):
		a.renderAccountPage(w, r, identity, accountPageData{PasswordError: "current password is incorrect"})
		return
	case errors.Is(err, rbac.ErrNotLocalAccount):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "not a local account")
		return
	case err != nil:
		a.logger.Printf("httpapi: change own password for %s: %v", identity.UserID, err)
		a.renderAccountPage(w, r, identity, accountPageData{PasswordError: "failed to change password"})
		return
	}

	a.renderAccountPage(w, r, identity, accountPageData{PasswordSuccess: true})
}
