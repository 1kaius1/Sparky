// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/email"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/session"
)

// fakeEmail implements emailManager with the same tier rule the real service
// applies, so these tests exercise the handlers' handling of the typed errors
// rather than a rule of their own.
type fakeEmail struct {
	fakeAddresses
	view      *email.SettingsView
	updateErr error
	updated   []email.SettingsInput
	testErr   error
	tested    []string
}

func (f *fakeEmail) Settings(_ context.Context, actor rbac.Actor) (*email.SettingsView, error) {
	if !rbac.CanManageEmail(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if f.view != nil {
		return f.view, nil
	}
	return &email.SettingsView{
		Settings: &db.EmailSettings{Port: 587, TLSMode: db.EmailTLSStartTLS, FromName: "Sparky", UpdatedAt: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)},
		Stats:    &db.EmailQueueStats{},
	}, nil
}

func (f *fakeEmail) UpdateSettings(_ context.Context, actor rbac.Actor, in email.SettingsInput) error {
	if !rbac.CanManageEmail(actor) {
		return rbac.ErrNotPermitted
	}
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, in)
	return nil
}

func (f *fakeEmail) SendTest(_ context.Context, actor rbac.Actor, to string) error {
	if !rbac.CanManageEmail(actor) {
		return rbac.ErrNotPermitted
	}
	f.tested = append(f.tested, to)
	return f.testErr
}

// The address-handling half of fakeEmail: what MyAddress returns, what the
// Admin-facing calls return, and a record of each call so a test can see what
// the handlers passed on.
type fakeAddresses struct {
	my          *email.AddressView
	myErr       error
	requestErr  error
	requested   []string
	clearErr    error
	cleared     int
	previewView *email.ConfirmationView
	previewErr  error
	previewed   int
	confirmed   []string
	confirmAddr string
	confirmErr  error
	addresses   map[string]*db.UserEmail
	setErr      error
	setResult   email.SetUserResult
	setCalls    []string
}

func (f *fakeEmail) MyAddress(_ context.Context, _ string) (*email.AddressView, error) {
	if f.my != nil || f.myErr != nil {
		return f.my, f.myErr
	}
	return &email.AddressView{EmailOn: true, CanConfirm: true}, nil
}

func (f *fakeEmail) RequestMyAddress(_ context.Context, _, address string) error {
	f.requested = append(f.requested, address)
	return f.requestErr
}

func (f *fakeEmail) ClearMyAddress(context.Context, string) error {
	f.cleared++
	return f.clearErr
}

func (f *fakeEmail) PreviewConfirmation(context.Context, string, string) (*email.ConfirmationView, error) {
	f.previewed++
	return f.previewView, f.previewErr
}

func (f *fakeEmail) ConfirmAddress(_ context.Context, _, token string) (string, error) {
	f.confirmed = append(f.confirmed, token)
	return f.confirmAddr, f.confirmErr
}

func (f *fakeEmail) Addresses(_ context.Context, actor rbac.Actor) (map[string]*db.UserEmail, error) {
	if !rbac.CanSetUserEmail(actor) {
		return nil, rbac.ErrNotPermitted
	}
	return f.addresses, nil
}

func (f *fakeEmail) SetUserAddress(_ context.Context, actor rbac.Actor, target, address string, require bool) (email.SetUserResult, error) {
	if !rbac.CanSetUserEmail(actor) {
		return email.SetUserResult{}, rbac.ErrNotPermitted
	}
	f.setCalls = append(f.setCalls, fmt.Sprintf("%s|%s|%v", target, address, require))
	return f.setResult, f.setErr
}

func emailAPI(t *testing.T, tier db.Tier, fake *fakeEmail) (*API, string) {
	t.Helper()
	api, uid := settingsAPI(t, tier, &fakeContainerLogs{})
	api.email = fake
	return api, uid
}

func emailForm(extra url.Values) url.Values {
	v := url.Values{
		"email_enabled": {"yes"}, "email_host": {"relay.example.com"}, "email_port": {"587"}, "email_tls_mode": {"starttls"},
		"email_username": {"svc"}, "email_from_address": {"sparky@example.com"}, "email_from_name": {"Sparky"},
	}
	for k, vals := range extra {
		v[k] = vals
	}
	return v
}

func TestSettings_EmailIsShownWithoutTheSecret(t *testing.T) {
	fake := &fakeEmail{view: &email.SettingsView{
		Settings:    &db.EmailSettings{Enabled: true, Host: "relay.example.com", Port: 465, TLSMode: db.EmailTLSImplicit, Username: "svc", FromAddress: "sparky@example.com", FromName: "Sparky", UpdatedAt: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)},
		PasswordSet: true, CAFileSet: true, PublicURL: "https://sparky.example.com",
		Stats:  &db.EmailQueueStats{Pending: 2, SentLastDay: 9, FailedLastWeek: 1, LastError: "550 no such user", LastErrorAt: time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)},
		Recent: []*db.Email{{Kind: db.EmailKindAlert, ToAddress: "ana@example.org", Subject: "[Sparky] <b>critical</b>", Status: db.EmailFailed, Attempts: 1, CreatedAt: time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC), LastError: strPtr("550 no such user")}},
	}}
	api, uid := emailAPI(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/settings").Body.String()
	for _, want := range []string{
		"Email", `action="/settings/email"`, `action="/settings/email/test"`, `name="email_host"`, `value="relay.example.com"`, `value="465"`,
		`<option value="tls" selected>`, `value="svc"`, `value="sparky@example.com"`, `name="email_enabled" value="yes" checked`,
		"set in the server's environment", "https://sparky.example.com", "550 no such user", "ana@example.org",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}
	if strings.Contains(body, "<b>critical</b>") || !strings.Contains(body, "&lt;b&gt;critical&lt;/b&gt;") {
		t.Error("a queued email's subject was not escaped")
	}
}

func strPtr(s string) *string { return &s }

func TestSettings_EmailFormIsParsedForTheService(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := emailAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/settings/email", emailForm(nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Email settings updated") {
		t.Fatalf("status %d body %.200s", rec.Code, rec.Body.String())
	}
	want := email.SettingsInput{Enabled: true, Host: "relay.example.com", Port: 587, TLSMode: db.EmailTLSStartTLS, Username: "svc", FromAddress: "sparky@example.com", FromName: "Sparky"}
	if len(fake.updated) != 1 || fake.updated[0] != want {
		t.Errorf("service got %+v, want %+v", fake.updated, want)
	}

	// An unticked box is off.
	fake.updated = nil
	postFormAs(t, api, uid, "/settings/email", url.Values{"email_port": {"25"}, "email_tls_mode": {"none"}})
	if len(fake.updated) != 1 || fake.updated[0].Enabled || fake.updated[0].Port != 25 || fake.updated[0].TLSMode != db.EmailTLSNone {
		t.Errorf("service got %+v", fake.updated)
	}
}

func TestSettings_EmailRefusalsAreShownNotHidden(t *testing.T) {
	fake := &fakeEmail{updateErr: fmt.Errorf("%w: a relay host is needed to turn email on", email.ErrInvalidSettings)}
	api, uid := emailAPI(t, db.TierAdmin, fake)
	body := postFormAs(t, api, uid, "/settings/email", emailForm(nil)).Body.String()
	if !strings.Contains(body, "a relay host is needed to turn email on") || strings.Contains(body, "invalid email settings: ") || strings.Contains(body, "Email settings updated") {
		t.Errorf("refusal body wrong: %.300s", body)
	}

	body = postFormAs(t, api, uid, "/settings/email", emailForm(url.Values{"email_port": {"smtp"}})).Body.String()
	if !strings.Contains(body, "port must be a whole number") {
		t.Errorf("a non-numeric port: %.300s", body)
	}

	fake = &fakeEmail{updateErr: errors.New("update email settings: pq: connection refused at 10.0.0.5")}
	api, uid = emailAPI(t, db.TierAdmin, fake)
	body = postFormAs(t, api, uid, "/settings/email", emailForm(nil)).Body.String()
	if !strings.Contains(body, "failed to update the email settings") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal failure leaks or lacks the generic message")
	}
}

func TestSettings_EmailTest(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := emailAPI(t, db.TierAdmin, fake)
	body := postFormAs(t, api, uid, "/settings/email/test", url.Values{"test_to": {" ana@example.org "}}).Body.String()
	if !strings.Contains(body, "Test email sent to ana@example.org") || len(fake.tested) != 1 || fake.tested[0] != "ana@example.org" {
		t.Errorf("sent: %.300s / %v", body, fake.tested)
	}

	// The relay's own reason is shown to the Admin in full.
	fake = &fakeEmail{testErr: fmt.Errorf("%w: connect to relay.example.com:587: connection refused", email.ErrSendFailed)}
	api, uid = emailAPI(t, db.TierAdmin, fake)
	body = postFormAs(t, api, uid, "/settings/email/test", url.Values{"test_to": {"ana@example.org"}}).Body.String()
	if !strings.Contains(body, "connection refused") || strings.Contains(body, "Test email sent") {
		t.Errorf("failure: %.300s", body)
	}

	fake = &fakeEmail{testErr: fmt.Errorf("%w: the address: the address is not valid", email.ErrInvalidSettings)}
	api, uid = emailAPI(t, db.TierAdmin, fake)
	body = postFormAs(t, api, uid, "/settings/email/test", url.Values{"test_to": {"nope"}}).Body.String()
	if !strings.Contains(body, "the address is not valid") {
		t.Errorf("bad address: %.300s", body)
	}

	fake = &fakeEmail{testErr: errors.New("get email settings: pq: boom at 10.0.0.5")}
	api, uid = emailAPI(t, db.TierAdmin, fake)
	body = postFormAs(t, api, uid, "/settings/email/test", url.Values{"test_to": {"ana@example.org"}}).Body.String()
	if !strings.Contains(body, "could not be sent") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal failure: %.300s", body)
	}
}

func TestSettings_EmailIsForbiddenBelowAdminAndNeedsCSRF(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := emailAPI(t, db.TierPowerDev, fake)
	for _, path := range []string{"/settings/email", "/settings/email/test"} {
		if rec := postFormAs(t, api, uid, path, emailForm(url.Values{"test_to": {"ana@example.org"}})); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", path, rec.Code)
		}
	}
	if len(fake.updated) != 0 || len(fake.tested) != 0 {
		t.Error("a non-admin changed email settings or sent a test")
	}

	fake = &fakeEmail{}
	api, uid = emailAPI(t, db.TierAdmin, fake)
	cookieValue, err := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/settings/email", "/settings/email/test"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(emailForm(url.Values{"test_to": {"ana@example.org"}}).Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a CSRF token = %d, want 403", path, rec.Code)
		}
	}
	if len(fake.updated) != 0 || len(fake.tested) != 0 {
		t.Error("a request without a CSRF token reached the service")
	}
}
