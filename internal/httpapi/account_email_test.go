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

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/email"
	"github.com/1kaius1/Sparky/internal/session"
)

// accountAPI is an API whose session is a local or an AD-backed account of the
// given tier, with fake as its email service.
func accountAPI(t *testing.T, tier db.Tier, local bool, fake *fakeEmail) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	u := &db.User{ID: "u-1", Tier: tier, DisplayName: "Ana"}
	if local {
		name := "ana"
		u.LocalUsername = &name
	}
	viewer.byID["u-1"] = u
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)
	api.email = fake
	return api, "u-1"
}

func TestAccountPage_ShowsALocalAccountsAddressAndLetsItChangeIt(t *testing.T) {
	fake := &fakeEmail{}
	fake.my = &email.AddressView{Email: "ana@example.org", Source: db.EmailSourceSelf, EmailOn: true, CanConfirm: true}
	api, uid := accountAPI(t, db.TierDeveloper, true, fake)
	body := getAs(t, api, uid, "/account").Body.String()
	for _, want := range []string{
		"<h2>Email</h2>", "ana@example.org", "confirmed by you", `action="/account/email"`, `name="email"`,
		"Send confirmation link", `action="/account/email/clear"`, "Remove my address", "Nothing changes until you open it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account page is missing %q", want)
		}
	}

	fake.my = &email.AddressView{EmailOn: true, CanConfirm: true, Pending: &email.PendingView{Email: "new@example.org", ExpiresAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), ByAdmin: true}}
	body = getAs(t, api, uid, "/account").Body.String()
	for _, want := range []string{"you have no email address", "Waiting for you to confirm", "new@example.org", "an Admin asked for it", "2026-10-28 09:00"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("account page with a pending change is missing %q", want)
		}
	}
	if strings.Contains(body, "Remove my address") {
		t.Error("offered to remove an address that does not exist")
	}
}

func TestAccountPage_ExplainsWhyAChangeCannotBeRequested(t *testing.T) {
	fake := &fakeEmail{}
	fake.my = &email.AddressView{EmailOn: false}
	api, uid := accountAPI(t, db.TierDeveloper, true, fake)
	body := getAs(t, api, uid, "/account").Body.String()
	if !strings.Contains(body, "Email is turned off on this server") || strings.Contains(body, `action="/account/email"`) {
		t.Errorf("email off: form shown or reason missing")
	}
	fake.my = &email.AddressView{EmailOn: true, CanConfirm: false}
	body = getAs(t, api, uid, "/account").Body.String()
	if !strings.Contains(body, "no public address configured") || strings.Contains(body, `action="/account/email"`) {
		t.Errorf("no public URL: form shown or reason missing")
	}
}

func TestAccountPage_AnADAccountSeesItsAddressButCannotEditIt(t *testing.T) {
	fake := &fakeEmail{}
	fake.my = &email.AddressView{Email: "jane@example.org", Source: db.EmailSourceDirectory, Directory: true, EmailOn: true, CanConfirm: true}
	api, uid := accountAPI(t, db.TierDeveloper, false, fake)
	body := getAs(t, api, uid, "/account").Body.String()
	if !strings.Contains(body, "jane@example.org") || !strings.Contains(body, "from your directory account") || !strings.Contains(body, "change it there") {
		t.Error("an AD account does not see its directory address")
	}
	if strings.Contains(body, `action="/account/email"`) || strings.Contains(body, "Remove my address") {
		t.Error("an AD account was offered a way to edit a directory address")
	}
}

func TestAccountEmail_RequestChangeMapsEveryOutcomeToSomethingActionable(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"sent":           {nil, "A confirmation link was sent to ana@example.org"},
		"directory":      {email.ErrDirectoryManaged, "comes from the directory"},
		"invalid":        {fmt.Errorf("%w: the address is not valid", email.ErrInvalidSettings), "the address is not valid"},
		"already in use": {email.ErrUnchanged, "already the address in use"},
		"too soon":       {email.ErrTooSoon, "Wait a minute"},
		"email off":      {email.ErrDisabled, "Email is turned off"},
		"no public URL":  {email.ErrNoPublicURL, "SPARKY_PUBLIC_URL"},
		"something else": {errors.New("queue the confirmation email: pq: boom at 10.0.0.5"), "could not be made"},
	}
	for name, c := range cases {
		fake := &fakeEmail{}
		fake.requestErr = c.err
		api, uid := accountAPI(t, db.TierDeveloper, true, fake)
		rec := postFormAs(t, api, uid, "/account/email", url.Values{"email": {" ana@example.org "}})
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, c.want) {
			t.Errorf("%s: status %d, body lacks %q", name, rec.Code, c.want)
		}
		if strings.Contains(body, "10.0.0.5") {
			t.Errorf("%s: an internal error leaked", name)
		}
		if len(fake.requested) != 1 || fake.requested[0] != "ana@example.org" {
			t.Errorf("%s: service asked for %v, want the trimmed address", name, fake.requested)
		}
		if (c.err == nil) != strings.Contains(body, `<div class="form-success">`) {
			t.Errorf("%s: success styling does not match the outcome", name)
		}
	}
}

func TestAccountEmail_Clear(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := accountAPI(t, db.TierDeveloper, true, fake)
	body := postFormAs(t, api, uid, "/account/email/clear", url.Values{}).Body.String()
	if !strings.Contains(body, "address was removed") || fake.cleared != 1 {
		t.Errorf("clear: %.200s / %d", body, fake.cleared)
	}
	fake = &fakeEmail{}
	fake.clearErr = email.ErrDirectoryManaged
	api, uid = accountAPI(t, db.TierDeveloper, false, fake)
	if body := postFormAs(t, api, uid, "/account/email/clear", url.Values{}).Body.String(); !strings.Contains(body, "comes from the directory") {
		t.Errorf("AD clear: %.200s", body)
	}
}

func TestAccountEmail_ConfirmLinkOnlyShowsUntilTheOwnerPressesTheButton(t *testing.T) {
	fake := &fakeEmail{}
	fake.previewView = &email.ConfirmationView{Email: "new@example.org", ExpiresAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
	fake.confirmAddr = "new@example.org"
	api, uid := accountAPI(t, db.TierDeveloper, true, fake)

	rec := getAs(t, api, uid, "/account/email/confirm?token=abc123")
	body := rec.Body.String()
	for _, want := range []string{"new@example.org", `action="/account/email/confirm"`, `name="token" value="abc123"`, "Confirm this address", "2026-10-28 09:00"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page is missing %q", want)
		}
	}
	if fake.previewed != 1 || len(fake.confirmed) != 0 {
		t.Errorf("opening the link: previewed %d, confirmed %v; it must change nothing", fake.previewed, fake.confirmed)
	}

	rec = postFormAs(t, api, uid, "/account/email/confirm", url.Values{"token": {"abc123"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Your email address is now new@example.org") {
		t.Errorf("confirm: status %d body %.200s", rec.Code, rec.Body.String())
	}
	if len(fake.confirmed) != 1 || fake.confirmed[0] != "abc123" {
		t.Errorf("confirmed %v", fake.confirmed)
	}
}

func TestAccountEmail_ConfirmFailuresAreExplained(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"not valid for this account": {email.ErrConfirmationNotFound, "not valid for the account you are signed in as"},
		"expired":                    {email.ErrConfirmationExpired, "has expired"},
		"directory account":          {email.ErrDirectoryManaged, "comes from the directory"},
		"something else":             {errors.New("record audit: boom at 10.0.0.5"), "could not be confirmed"},
	} {
		fake := &fakeEmail{}
		fake.previewErr, fake.confirmErr = c.err, c.err
		api, uid := accountAPI(t, db.TierDeveloper, true, fake)
		for _, body := range []string{
			getAs(t, api, uid, "/account/email/confirm?token=x").Body.String(),
			postFormAs(t, api, uid, "/account/email/confirm", url.Values{"token": {"x"}}).Body.String(),
		} {
			if !strings.Contains(body, c.want) || strings.Contains(body, "10.0.0.5") || strings.Contains(body, "Confirm this address") {
				t.Errorf("%s: body = %.300s", name, body)
			}
		}
	}
}

func TestAccountEmail_TheBreakGlassAccountHasNoAddressAndCannotChangeOne(t *testing.T) {
	fake := &fakeEmail{}
	api, _ := accountAPI(t, db.TierAdmin, true, fake)
	cookieValue, err := session.Sign(testSessionSecret, session.Session{UserID: "", IsSuperAdmin: true, ExpiresAt: time.Now().UTC().Add(sessionDuration), LastVerifiedAt: time.Now().UTC()})
	if err != nil {
		t.Skipf("cannot build a break-glass session in this test: %v", err)
	}
	get := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var req *http.Request
		if form != nil {
			req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			addValidCSRF(req)
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		return rec
	}
	if rec := get(http.MethodGet, "/account", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<h2>Email</h2>") {
		t.Errorf("account page for break-glass: %d, shows an email section: %v", rec.Code, strings.Contains(rec.Body.String(), "<h2>Email</h2>"))
	}
	for _, path := range []string{"/account/email", "/account/email/clear", "/account/email/confirm"} {
		if rec := get(http.MethodPost, path, url.Values{"email": {"x@example.org"}, "token": {"t"}}); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as break-glass = %d, want 403", path, rec.Code)
		}
	}
	if len(fake.requested) != 0 || fake.cleared != 0 || len(fake.confirmed) != 0 {
		t.Error("the break-glass account reached the email service")
	}
}

func TestAccountEmail_PostsNeedACSRFToken(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := accountAPI(t, db.TierDeveloper, true, fake)
	cookieValue, _ := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	for _, path := range []string{"/account/email", "/account/email/clear", "/account/email/confirm"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"email": {"x@example.org"}, "token": {"t"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a CSRF token = %d, want 403", path, rec.Code)
		}
	}
	if len(fake.requested) != 0 || fake.cleared != 0 || len(fake.confirmed) != 0 {
		t.Error("a request without a CSRF token reached the service")
	}
}

// ---- the Users page ----

func usersAPI(t *testing.T, tier db.Tier, fake *fakeEmail) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	roster := &fakeUserRoster{users: []*db.User{
		{ID: "u-local", DisplayName: "Local Lou", Tier: db.TierDeveloper, LocalUsername: strPtr("lou")},
		{ID: "u-ad", DisplayName: "AD Ada", Tier: db.TierDeveloper, ADSID: strPtr("S-1-5-21-1")},
	}}
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, roster, settingsSvc)
	api.email = fake
	return api, "u-1"
}

func TestUsersPage_ShowsEachAddressAndOffersSetEmailOnlyForLocalAccounts(t *testing.T) {
	fake := &fakeEmail{}
	fake.addresses = map[string]*db.UserEmail{
		"u-local": {UserID: "u-local", Email: strPtr("lou@example.org"), Source: srcPtrHTTP(db.EmailSourceAdmin)},
		"u-ad":    {UserID: "u-ad", Directory: true, Email: strPtr("ada@example.org"), Source: srcPtrHTTP(db.EmailSourceDirectory)},
	}
	api, uid := usersAPI(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/users").Body.String()
	for _, want := range []string{"lou@example.org", "set by an Admin", "ada@example.org", "from the directory", `hx-post="/users/u-local/email"`, "ask them to confirm"} {
		if !strings.Contains(body, want) {
			t.Errorf("users page is missing %q", want)
		}
	}
	if strings.Contains(body, `hx-post="/users/u-ad/email"`) {
		t.Error("the Set email form is offered for an AD-backed account")
	}

	// If the addresses cannot be read the roster still shows.
	fake.addresses = nil
	if rec := getAs(t, api, uid, "/users"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Local Lou") {
		t.Errorf("users page without addresses: %d", rec.Code)
	}
}

func srcPtrHTTP(s db.EmailSource) *db.EmailSource { return &s }

func TestSetUserEmail(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := usersAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/users/u-local/email", url.Values{"email": {" lou@example.org "}, "require_confirmation": {"yes"}})
	if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/users" {
		t.Errorf("status %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	postFormAs(t, api, uid, "/users/u-local/email", url.Values{"email": {"lou@example.org"}})
	postFormAs(t, api, uid, "/users/u-local/email", url.Values{"email": {""}, "require_confirmation": {"yes"}})
	want := []string{"u-local|lou@example.org|true", "u-local|lou@example.org|false", "u-local||true"}
	if strings.Join(fake.setCalls, ",") != strings.Join(want, ",") {
		t.Errorf("service got %v, want %v", fake.setCalls, want)
	}

	for name, c := range map[string]struct {
		err    error
		status int
	}{
		"directory account": {email.ErrDirectoryManaged, http.StatusBadRequest},
		"unknown user":      {db.ErrUserNotFound, http.StatusNotFound},
		"invalid":           {fmt.Errorf("%w: the address is not valid", email.ErrInvalidSettings), http.StatusBadRequest},
		"email off":         {email.ErrDisabled, http.StatusBadRequest},
		"no public URL":     {email.ErrNoPublicURL, http.StatusBadRequest},
		"internal":          {errors.New("update the address: pq: boom at 10.0.0.5"), http.StatusInternalServerError},
	} {
		f := &fakeEmail{}
		f.setErr = c.err
		api, uid := usersAPI(t, db.TierAdmin, f)
		rec := postFormAs(t, api, uid, "/users/u-local/email", url.Values{"email": {"lou@example.org"}})
		if rec.Code != c.status || strings.Contains(rec.Body.String(), "10.0.0.5") {
			t.Errorf("%s: status %d (want %d), body %.200s", name, rec.Code, c.status, rec.Body.String())
		}
	}
}

func TestSetUserEmail_ForbiddenBelowAdminAndNeedsCSRF(t *testing.T) {
	fake := &fakeEmail{}
	api, uid := usersAPI(t, db.TierPowerDev, fake)
	if rec := postFormAs(t, api, uid, "/users/u-local/email", url.Values{"email": {"x@example.org"}}); rec.Code != http.StatusForbidden {
		t.Errorf("PowerDev: %d, want 403", rec.Code)
	}
	if len(fake.setCalls) != 0 {
		t.Error("a non-admin set an address")
	}

	fake = &fakeEmail{}
	api, uid = usersAPI(t, db.TierAdmin, fake)
	cookieValue, _ := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	req := httptest.NewRequest(http.MethodPost, "/users/u-local/email", strings.NewReader(url.Values{"email": {"x@example.org"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(fake.setCalls) != 0 {
		t.Errorf("without CSRF: %d, %d calls", rec.Code, len(fake.setCalls))
	}
}

// ---- the sign-in hook ----

type directoryEmailFake struct{ calls []string }

func (d *directoryEmailFake) RecordDirectoryEmail(_ context.Context, userID, mail string) {
	d.calls = append(d.calls, userID+"|"+mail)
}

func TestLoginService_RecordsTheDirectoryAddressAfterASignIn(t *testing.T) {
	dn := "CN=jsmith,DC=example,DC=internal"
	newSvc := func(inGroup bool, err error) (*LoginService, *directoryEmailFake) {
		idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{ADSID: "S-1-5-21-1", DisplayName: "J", InAccessGroup: inGroup, DN: dn, Email: "jane@example.org"}, err: err}
		rec := &directoryEmailFake{}
		return NewLoginService(idp, newFakeUserStore(), testSessionSecret).WithDirectoryEmailRecorder(rec), rec
	}

	svc, rec := newSvc(true, nil)
	user, _, err := svc.Login(context.Background(), "jsmith", "pw")
	if err != nil || len(rec.calls) != 1 || rec.calls[0] != user.ID+"|jane@example.org" {
		t.Fatalf("a sign-in: %v, calls %v", err, rec.calls)
	}

	// Nobody who is refused a login has their address recorded.
	svc, rec = newSvc(false, nil)
	if _, _, err := svc.Login(context.Background(), "jsmith", "pw"); !errors.Is(err, ErrAccessDenied) || len(rec.calls) != 0 {
		t.Errorf("outside the access group: %v, calls %v", err, rec.calls)
	}
	svc, rec = newSvc(true, auth.ErrInvalidCredentials)
	if _, _, err := svc.Login(context.Background(), "jsmith", "pw"); err == nil || len(rec.calls) != 0 {
		t.Errorf("bad credentials: %v, calls %v", err, rec.calls)
	}

	// Without email wired in, sign-in works as before.
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{ADSID: "S-1-5-21-2", DisplayName: "K", InAccessGroup: true, DN: dn}}
	if _, _, err := NewLoginService(idp, newFakeUserStore(), testSessionSecret).Login(context.Background(), "k", "pw"); err != nil {
		t.Errorf("Login() with no recorder: %v", err)
	}
}
