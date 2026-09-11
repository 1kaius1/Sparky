// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
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
	UpdateOwnTheme(ctx context.Context, actor rbac.Actor, preset *db.ThemePreset, customColors map[string]string, statusPalette *db.ThemeStatusPalette) error
}

// accountPageData is the self-service account page's view model - always
// mounted (GET/POST /account), but only functional for a local-account
// session (IsLocalAccount): a non-local session sees a minimal read-only
// notice instead of the display-name/password edit forms, since an
// AD-backed display name is refreshed from AD at every login and Sparky
// never holds that password. The theme section, by contrast, is shown for
// every session except the break-glass SuperAdmin (IsSuperAdmin) - theme
// is a preference on the Users row, applying to AD-backed and local
// accounts alike, and the SuperAdmin identity has no Users row to persist
// one to. Each form is independent - every POST redisplays this same page
// with its own Error/Success rather than redirecting, same reasoning as
// login.html's own redisplay-on-failure (this project has no
// flash-message/session-based mechanism to carry a message across a
// redirect).
type accountPageData struct {
	IsLocalAccount bool
	DisplayName    string

	DisplayNameError   string
	DisplayNameSuccess bool

	PasswordError   string
	PasswordSuccess bool

	IsSuperAdmin     bool
	AvailablePresets []themePresetOption
	SelectedPreset   string
	IsCustomizing    bool
	CustomColors     []themeColorField
	StatusPalette    string // "" (auto) / "light" / "dark"
	ThemeError       string
	ThemeSuccess     bool
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

// renderAccountPage fills in DisplayName and theme fields from the
// current user row before rendering, so a redisplay after any form's
// failure still shows the caller's real current state, not empty fields.
// The user row is fetched whenever the session isn't the break-glass
// SuperAdmin (not only for a local account) - the theme section needs it
// for AD-backed sessions too.
func (a *API) renderAccountPage(w http.ResponseWriter, r *http.Request, identity Identity, data accountPageData) {
	data.IsLocalAccount = identity.IsLocalAccount
	data.IsSuperAdmin = identity.IsSuperAdmin
	if identity.IsSuperAdmin {
		a.render(w, r, "account", "My account", data)
		return
	}

	user, err := a.users.FindByID(r.Context(), identity.UserID)
	if err != nil {
		a.logger.Printf("httpapi: look up user for account page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if identity.IsLocalAccount {
		data.DisplayName = user.DisplayName
	}

	data.AvailablePresets = themePresetOptions()
	if user.ThemePreset != nil {
		data.SelectedPreset = string(*user.ThemePreset)
	}
	var customColors map[string]string
	if err := json.Unmarshal(user.ThemeCustomColors, &customColors); err != nil {
		a.logger.Printf("httpapi: parse theme custom colors for %s: %v", identity.UserID, err)
	}
	data.IsCustomizing = len(customColors) > 0
	data.CustomColors = themeColorFields(cssVarMapToSlugMap(customColors))
	if user.ThemeStatusPalette != nil {
		data.StatusPalette = string(*user.ThemeStatusPalette)
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

// handleUpdateTheme changes the caller's own theme preference - see
// rbac.Service.UpdateOwnTheme. The "customize" checkbox gates whether
// per-color overrides and the status-palette radio are read at all: an
// unchecked box means "use the stock preset as-is," clearing any prior
// customization, matching UpdateOwnTheme's own "customColors/
// statusPalette empty means no customization" semantics.
func (a *API) handleUpdateTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "break-glass session has no theme preference")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderAccountPage(w, r, identity, accountPageData{ThemeError: "invalid form submission"})
		return
	}

	var preset *db.ThemePreset
	if v := r.PostFormValue("theme_preset"); v != "" {
		p := db.ThemePreset(v)
		preset = &p
	}

	colors := map[string]string{}
	var statusPalette *db.ThemeStatusPalette
	if r.PostFormValue("customize") == "on" {
		for _, k := range rbac.ThemeColorKeys {
			if v := r.PostFormValue("color_" + k.Slug); v != "" {
				colors[k.CSSVar] = v
			}
		}
		if v := r.PostFormValue("status_palette"); v != "" {
			sp := db.ThemeStatusPalette(v)
			statusPalette = &sp
		}
	}

	err := a.selfAccount.UpdateOwnTheme(ctx, rbac.Actor{UserID: identity.UserID}, preset, colors, statusPalette)
	switch {
	case errors.Is(err, rbac.ErrInvalidTheme):
		a.renderAccountPage(w, r, identity, accountPageData{ThemeError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: update theme for %s: %v", identity.UserID, err)
		a.renderAccountPage(w, r, identity, accountPageData{ThemeError: "failed to update theme"})
		return
	}

	a.renderAccountPage(w, r, identity, accountPageData{ThemeSuccess: true})
}
