// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/email"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// accountEmailView is the Email section of the Account page.
type accountEmailView struct {
	// Directory: the address comes from AD at every sign-in and is not edited
	// here.
	Directory bool

	Address     string
	SourceLabel string

	PendingEmail   string
	PendingExpires string
	PendingByAdmin bool

	// EmailOn and CanConfirm say whether a change can be asked for now.
	EmailOn    bool
	CanConfirm bool

	// How this person wants alert email, and when a daily digest goes out.
	Delivery        string
	MinSeverity     string
	DailyDigestTime string
	Zone            string
}

func accountEmailViewOf(v *email.AddressView) *accountEmailView {
	out := &accountEmailView{
		Directory: v.Directory, Address: v.Email, EmailOn: v.EmailOn, CanConfirm: v.CanConfirm,
		Delivery: string(v.Delivery), MinSeverity: string(v.MinSeverity), DailyDigestTime: v.DailyDigestTime, Zone: v.Zone,
	}
	switch v.Source {
	case db.EmailSourceDirectory:
		out.SourceLabel = "from your directory account"
	case db.EmailSourceAdmin:
		out.SourceLabel = "set by an Admin"
	case db.EmailSourceSelf:
		out.SourceLabel = "confirmed by you"
	}
	if v.Pending != nil {
		out.PendingEmail = v.Pending.Email
		out.PendingExpires = v.Pending.ExpiresAt.Format("2006-01-02 15:04 MST")
		out.PendingByAdmin = v.Pending.ByAdmin
	}
	return out
}

// emailChangeMessage turns the email service's typed refusals into something a
// person can act on.
func emailChangeMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, email.ErrDirectoryManaged):
		return "This account's address comes from the directory and cannot be changed here.", true
	case errors.Is(err, email.ErrInvalidSettings):
		return strings.TrimPrefix(err.Error(), email.ErrInvalidSettings.Error()+": "), true
	case errors.Is(err, email.ErrUnchanged):
		return "That is already the address in use.", true
	case errors.Is(err, email.ErrTooSoon):
		return "A confirmation email was sent a moment ago. Wait a minute before asking again.", true
	case errors.Is(err, email.ErrDisabled):
		return "Email is turned off, so a confirmation link cannot be sent. Ask an Admin to turn it on, or to set the address for you.", true
	case errors.Is(err, email.ErrNoPublicURL):
		return "This server has no public address configured (SPARKY_PUBLIC_URL), so a confirmation link cannot be sent. Ask an Admin.", true
	}
	return "", false
}

// handleRequestOwnEmail is POST /account/email: a local account asks to change
// its address. A confirmation link goes to the NEW address; nothing changes
// until it is opened.
func (a *API) handleRequestOwnEmail(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "the break-glass account has no email address")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderAccountPage(w, r, identity, accountPageData{EmailError: "invalid form submission"})
		return
	}
	address := strings.TrimSpace(r.PostFormValue("email"))

	err := a.email.RequestMyAddress(r.Context(), identity.UserID, address)
	if msg, handled := emailChangeMessage(err); handled {
		a.renderAccountPage(w, r, identity, accountPageData{EmailError: msg})
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: request email change: %v", err)
		a.renderAccountPage(w, r, identity, accountPageData{EmailError: "the request could not be made"})
		return
	}
	a.renderAccountPage(w, r, identity, accountPageData{EmailSuccess: "A confirmation link was sent to " + address + ". Open it while you are signed in to finish; until then your current address stays in use."})
}

// handleClearOwnEmail is POST /account/email/clear.
func (a *API) handleClearOwnEmail(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "the break-glass account has no email address")
		return
	}
	err := a.email.ClearMyAddress(r.Context(), identity.UserID)
	if msg, handled := emailChangeMessage(err); handled {
		a.renderAccountPage(w, r, identity, accountPageData{EmailError: msg})
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: clear email address: %v", err)
		a.renderAccountPage(w, r, identity, accountPageData{EmailError: "the address could not be removed"})
		return
	}
	a.renderAccountPage(w, r, identity, accountPageData{EmailSuccess: "Your email address was removed. You will not get alert email."})
}

// handleUpdateEmailPreferences is POST /account/email/preferences: how the
// signed-in person wants alert email - off, immediately, or in a digest - and
// the lowest severity they want. Any tier, any account with an address.
func (a *API) handleUpdateEmailPreferences(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "the break-glass account has no email address")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderAccountPage(w, r, identity, accountPageData{EmailPrefsError: "invalid form submission"})
		return
	}
	err := a.email.SetMyPreferences(r.Context(), identity.UserID, email.MyPreferences{
		Delivery:    db.EmailDelivery(r.PostFormValue("email_delivery")),
		MinSeverity: db.AlertSeverity(r.PostFormValue("email_min_severity")),
	})
	if errors.Is(err, email.ErrInvalidSettings) {
		a.renderAccountPage(w, r, identity, accountPageData{EmailPrefsError: strings.TrimPrefix(err.Error(), email.ErrInvalidSettings.Error()+": ")})
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: update email preferences: %v", err)
		a.renderAccountPage(w, r, identity, accountPageData{EmailPrefsError: "the settings could not be saved"})
		return
	}
	a.renderAccountPage(w, r, identity, accountPageData{EmailPrefsSuccess: true})
}

// emailConfirmData is the view model of the confirm page.
type emailConfirmData struct {
	Token     string
	Email     string
	ExpiresAt string
	ByAdmin   bool
	Error     string
}

func confirmErrorMessage(err error) string {
	switch {
	case errors.Is(err, email.ErrConfirmationExpired):
		return "This confirmation link has expired. Ask for a new one from My account."
	case errors.Is(err, email.ErrConfirmationNotFound):
		return "This confirmation link is not valid for the account you are signed in as. It may have been used already, replaced by a newer one, or be meant for someone else. If it is yours, sign in as that account and open it again."
	case errors.Is(err, email.ErrDirectoryManaged):
		return "This account's address comes from the directory and cannot be changed here."
	}
	return "The address could not be confirmed. Try again, or ask for a new link."
}

// handleEmailConfirmPage is GET /account/email/confirm?token=...: it shows what
// the link would confirm and a button. It changes nothing, because a link
// opened by a mail scanner or a browser's preview must not confirm an address -
// only the signed-in owner pressing the button does.
func (a *API) handleEmailConfirmPage(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		a.render(w, r, "email_confirm", "Confirm email address", emailConfirmData{Error: confirmErrorMessage(email.ErrConfirmationNotFound)})
		return
	}
	token := r.URL.Query().Get("token")
	view, err := a.email.PreviewConfirmation(r.Context(), identity.UserID, token)
	if err != nil {
		if !errors.Is(err, email.ErrConfirmationNotFound) && !errors.Is(err, email.ErrConfirmationExpired) {
			a.logger.Printf("httpapi: preview email confirmation: %v", err)
		}
		a.render(w, r, "email_confirm", "Confirm email address", emailConfirmData{Error: confirmErrorMessage(err)})
		return
	}
	a.render(w, r, "email_confirm", "Confirm email address", emailConfirmData{
		Token: token, Email: view.Email, ByAdmin: view.ByAdmin, ExpiresAt: view.ExpiresAt.Format("2006-01-02 15:04 MST"),
	})
}

// handleEmailConfirm is POST /account/email/confirm: the signed-in owner
// pressed the button.
func (a *API) handleEmailConfirm(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if identity.IsSuperAdmin {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "the break-glass account has no email address")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.render(w, r, "email_confirm", "Confirm email address", emailConfirmData{Error: "invalid form submission"})
		return
	}
	address, err := a.email.ConfirmAddress(r.Context(), identity.UserID, r.PostFormValue("token"))
	if err != nil {
		if !errors.Is(err, email.ErrConfirmationNotFound) && !errors.Is(err, email.ErrConfirmationExpired) && !errors.Is(err, email.ErrDirectoryManaged) {
			a.logger.Printf("httpapi: confirm email address: %v", err)
		}
		a.render(w, r, "email_confirm", "Confirm email address", emailConfirmData{Error: confirmErrorMessage(err)})
		return
	}
	a.renderAccountPage(w, r, identity, accountPageData{EmailSuccess: "Your email address is now " + address + "."})
}

// handleSetUserEmail is POST /users/{id}/email: an Admin sets, changes or
// clears a local account's address, with or without asking its owner to confirm
// it. Same inline-form shape as the tier and password actions beside it.
func (a *API) handleSetUserEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "set user email")
	if !ok {
		return
	}
	targetUserID := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid form submission")
		return
	}

	_, err := a.email.SetUserAddress(ctx, actor, targetUserID, strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("require_confirmation") != "")
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, db.ErrUserNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	case errors.Is(err, email.ErrDirectoryManaged):
		writeError(w, r, http.StatusBadRequest, "NOT_LOCAL_ACCOUNT", "this account's address comes from the directory")
		return
	}
	if msg, handled := emailChangeMessage(err); handled {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", msg)
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: set email for %s: %v", targetUserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/users")
	w.WriteHeader(http.StatusNoContent)
}
