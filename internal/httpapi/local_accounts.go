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

// localAccountManager is the subset of *rbac.Service this package needs for
// the create-local-account form and the Users & permissions page's
// per-row password-reset action - a distinct, narrowly-scoped interface
// from userRoster/userElevator even though the same concrete *rbac.Service
// satisfies all of them in production, matching this codebase's existing
// one-interface-per-need pattern.
type localAccountManager interface {
	CreateLocalAccount(ctx context.Context, actor rbac.Actor, username, password, displayName string, tier db.Tier) (*db.User, error)
	ResetLocalAccountPassword(ctx context.Context, actor rbac.Actor, targetUserID, newPassword string) error
}

// createLocalAccountPageData is the create-local-account form's view model -
// same Error/Form-redisplay-on-failure shape as registerNodePageData.
type createLocalAccountPageData struct {
	Error string
	Form  createLocalAccountFormValues
}

type createLocalAccountFormValues struct {
	Username    string
	DisplayName string
	Tier        string
}

func (a *API) handleNewLocalAccountForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for create-local-account form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !rbac.CanCreateLocalAccount(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	}

	a.render(w, r, "create_local_account", "Create local account",
		createLocalAccountPageData{Form: createLocalAccountFormValues{Tier: string(db.TierReadOnly)}})
}

func (a *API) handleCreateLocalAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderCreateLocalAccountError(w, r, "invalid form submission", createLocalAccountFormValues{})
		return
	}
	form := createLocalAccountFormValues{
		Username:    r.PostFormValue("username"),
		DisplayName: r.PostFormValue("display_name"),
		Tier:        r.PostFormValue("tier"),
	}
	password := r.PostFormValue("password")
	tier := db.Tier(form.Tier)
	if form.Username == "" || form.DisplayName == "" || password == "" || !isKnownTier(tier) {
		a.renderCreateLocalAccountError(w, r, "all fields are required", form)
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for create local account: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	_, err = a.localAccounts.CreateLocalAccount(ctx, actor, form.Username, password, form.DisplayName, tier)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, db.ErrLocalUsernameTaken):
		a.renderCreateLocalAccountError(w, r, "that username is already taken", form)
		return
	case err != nil:
		a.logger.Printf("httpapi: create local account: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// No one-time secret to display here, unlike node registration's
	// bearer token - the Admin chose the password directly.
	http.Redirect(w, r, "/users", http.StatusFound)
}

func (a *API) renderCreateLocalAccountError(w http.ResponseWriter, r *http.Request, errMsg string, form createLocalAccountFormValues) {
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "create_local_account", "Create local account", createLocalAccountPageData{Error: errMsg, Form: form})
}

// handleResetLocalAccountPassword is POST /users/{id}/local/reset-password -
// the Users & permissions page's per-row password-reset action, shown only
// for rows with a local_username - mirrors handleElevateUser's own
// inline-form/HX-Redirect shape.
func (a *API) handleResetLocalAccountPassword(w http.ResponseWriter, r *http.Request) {
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
	newPassword := r.PostFormValue("password")
	if newPassword == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "password is required")
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for reset local account password: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err = a.localAccounts.ResetLocalAccountPassword(ctx, actor, targetUserID, newPassword)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, rbac.ErrNotLocalAccount):
		writeError(w, r, http.StatusBadRequest, "NOT_LOCAL_ACCOUNT", "this user has no local password to reset")
		return
	case errors.Is(err, db.ErrUserNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	case err != nil:
		a.logger.Printf("httpapi: reset local account password for %s: %v", targetUserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/users")
	w.WriteHeader(http.StatusNoContent)
}
