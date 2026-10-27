// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/email"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// emailManager is the subset of *email.Service the Settings page needs: the
// SMTP relay settings and the test send. Every method is gated to Admin inside
// the service.
type emailManager interface {
	Settings(ctx context.Context, actor rbac.Actor) (*email.SettingsView, error)
	UpdateSettings(ctx context.Context, actor rbac.Actor, in email.SettingsInput) error
	SendTest(ctx context.Context, actor rbac.Actor, to string) error

	// A person's own address (Account page) and the Admin's view and edit of
	// everyone's (Users page).
	MyAddress(ctx context.Context, userID string) (*email.AddressView, error)
	RequestMyAddress(ctx context.Context, userID, address string) error
	ClearMyAddress(ctx context.Context, userID string) error
	PreviewConfirmation(ctx context.Context, userID, token string) (*email.ConfirmationView, error)
	ConfirmAddress(ctx context.Context, userID, token string) (string, error)
	Addresses(ctx context.Context, actor rbac.Actor) (map[string]*db.UserEmail, error)
	SetUserAddress(ctx context.Context, actor rbac.Actor, targetUserID, address string, requireConfirmation bool) (email.SetUserResult, error)
}

// emailSettingsView is the Email section of the Settings page.
type emailSettingsView struct {
	Enabled     bool
	Host        string
	Port        int
	TLSMode     string
	Username    string
	FromAddress string
	FromName    string

	// What the environment provides; the password itself is never shown.
	PasswordSet bool
	CAFileSet   bool
	PublicURL   string

	Pending        int
	SentLastDay    int
	FailedLastWeek int
	LastError      string
	LastErrorAt    string

	Recent    []emailRecentRow
	UpdatedBy string
	UpdatedAt string
}

type emailRecentRow struct {
	When     string
	Kind     string
	To       string
	Subject  string
	Status   string
	Attempts int
	Error    string
}

func (a *API) emailSettingsViewOf(ctx context.Context, v *email.SettingsView) *emailSettingsView {
	cfg := v.Settings
	out := &emailSettingsView{
		Enabled: cfg.Enabled, Host: cfg.Host, Port: cfg.Port, TLSMode: string(cfg.TLSMode), Username: cfg.Username,
		FromAddress: cfg.FromAddress, FromName: cfg.FromName,
		PasswordSet: v.PasswordSet, CAFileSet: v.CAFileSet, PublicURL: v.PublicURL,
		Pending: v.Stats.Pending, SentLastDay: v.Stats.SentLastDay, FailedLastWeek: v.Stats.FailedLastWeek, LastError: v.Stats.LastError,
		UpdatedBy: a.resolveUserName(ctx, cfg.UpdatedBy), UpdatedAt: cfg.UpdatedAt.Format("2006-01-02 15:04:05 MST"),
	}
	if !v.Stats.LastErrorAt.IsZero() {
		out.LastErrorAt = v.Stats.LastErrorAt.Format("2006-01-02 15:04:05 MST")
	}
	for _, e := range v.Recent {
		row := emailRecentRow{
			When: e.CreatedAt.Format("2006-01-02 15:04"), Kind: string(e.Kind), To: e.ToAddress, Subject: e.Subject,
			Status: string(e.Status), Attempts: e.Attempts,
		}
		if e.LastError != nil {
			row.Error = *e.LastError
		}
		out.Recent = append(out.Recent, row)
	}
	return out
}

// handleUpdateEmailSettings saves the SMTP relay settings - see
// email.Service.UpdateSettings. A refused change redisplays the page with the
// reason, never a 500.
func (a *API) handleUpdateEmailSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "email settings update")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: "invalid form submission"})
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("email_port")))
	if err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: "the port must be a whole number"})
		return
	}
	in := email.SettingsInput{
		Enabled:     r.PostFormValue("email_enabled") != "",
		Host:        r.PostFormValue("email_host"),
		Port:        port,
		TLSMode:     db.EmailTLSMode(r.PostFormValue("email_tls_mode")),
		Username:    r.PostFormValue("email_username"),
		FromAddress: r.PostFormValue("email_from_address"),
		FromName:    r.PostFormValue("email_from_name"),
	}

	err = a.email.UpdateSettings(ctx, actor, in)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, email.ErrInvalidSettings):
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: strings.TrimPrefix(err.Error(), email.ErrInvalidSettings.Error()+": ")})
		return
	case err != nil:
		a.logger.Printf("httpapi: update email settings: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: "failed to update the email settings"})
		return
	}
	a.renderSettingsPage(w, r, actor, settingsPageData{EmailSuccess: "Email settings updated."})
}

// handleSendTestEmail sends one email straight to the address given, so the
// Admin sees the relay's own answer. Unlike the other settings forms the
// failure text is shown in full: it is how an Admin finds out why the relay
// will not take mail, and only an Admin can reach it.
func (a *API) handleSendTestEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "test email")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: "invalid form submission"})
		return
	}
	to := strings.TrimSpace(r.PostFormValue("test_to"))

	err := a.email.SendTest(ctx, actor, to)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, email.ErrInvalidSettings):
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: strings.TrimPrefix(err.Error(), email.ErrInvalidSettings.Error()+": ")})
		return
	case errors.Is(err, email.ErrSendFailed):
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: send test email: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{EmailError: "the test email could not be sent"})
		return
	}
	a.renderSettingsPage(w, r, actor, settingsPageData{EmailSuccess: "Test email sent to " + to + ". Check that it arrived."})
}
