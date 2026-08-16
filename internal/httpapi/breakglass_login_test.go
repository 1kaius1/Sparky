// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/session"
)

// testBreakGlassLoginPath is the default used by every other test file's
// New() call - matches the real BREAKGLASS_LOGIN_PATH default, so existing
// tests unrelated to path configurability keep working against the
// well-known route.
const testBreakGlassLoginPath = "/login/break-glass"

// fakeBreakGlassStore implements breakGlassStore for tests without a real
// Postgres instance.
type fakeBreakGlassStore struct {
	cred *db.BreakGlassCredential
	err  error
}

// newFakeBreakGlassStore defaults to "never configured" - most tests that
// build a *API don't care about the break-glass path at all.
func newFakeBreakGlassStore() *fakeBreakGlassStore {
	return &fakeBreakGlassStore{}
}

// newConfiguredFakeBreakGlassStore reports setup as already complete -
// used as the setupGate's store in tests that aren't specifically about
// the gate, so it never blocks them. See setup_gate_test.go for tests of
// the gate's own not-configured/blocking behavior.
func newConfiguredFakeBreakGlassStore() *fakeBreakGlassStore {
	return &fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: "unused-in-gate-tests"}}
}

func (f *fakeBreakGlassStore) Get(_ context.Context) (*db.BreakGlassCredential, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.cred == nil {
		return nil, db.ErrBreakGlassNotSet
	}
	return f.cred, nil
}

func TestBreakGlassLoginService_Login_Success(t *testing.T) {
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	store := &fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}
	svc := NewBreakGlassLoginService(store, testSessionSecret)

	cookieValue, err := svc.Login(context.Background(), "break-glass-password")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}

	sess, err := session.Verify(testSessionSecret, cookieValue)
	if err != nil {
		t.Fatalf("session.Verify() error: %v", err)
	}
	if !sess.IsSuperAdmin {
		t.Error("IsSuperAdmin = false, want true")
	}
	if sess.UserID != "" {
		t.Errorf("UserID = %q, want empty", sess.UserID)
	}
}

func TestBreakGlassLoginService_Login_WrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	store := &fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}
	svc := NewBreakGlassLoginService(store, testSessionSecret)

	_, err = svc.Login(context.Background(), "wrong-password")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want auth.ErrInvalidCredentials", err)
	}
}

func TestBreakGlassLoginService_Login_NeverConfigured(t *testing.T) {
	svc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)

	_, err := svc.Login(context.Background(), "anything")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want auth.ErrInvalidCredentials", err)
	}
}

func TestBreakGlassLoginService_Login_StoreError(t *testing.T) {
	store := &fakeBreakGlassStore{err: errors.New("database unreachable")}
	svc := NewBreakGlassLoginService(store, testSessionSecret)

	_, err := svc.Login(context.Background(), "anything")
	if err == nil {
		t.Fatal("Login() succeeded despite a store failure")
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		t.Error("Login() returned ErrInvalidCredentials for an infrastructure failure")
	}
}

func TestHandleBreakGlassLogin_FullRoundTrip(t *testing.T) {
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	loginSvc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(&fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}, testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "break-glass-password"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	var setCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			setCookie = c
		}
	}
	if setCookie == nil {
		t.Fatal("no session cookie in the login response's Set-Cookie header")
	}
	if !setCookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}

	sess, err := session.Verify(testSessionSecret, setCookie.Value)
	if err != nil {
		t.Fatalf("session.Verify() error: %v", err)
	}
	if !sess.IsSuperAdmin {
		t.Error("IsSuperAdmin = false, want true")
	}
}

func TestHandleBreakGlassLogin_WrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	loginSvc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(&fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}, testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	var errResp errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Code != "INVALID_CREDENTIALS" {
		t.Errorf("Code = %q, want %q", errResp.Code, "INVALID_CREDENTIALS")
	}
}

func TestHandleBreakGlassLogin_MissingPassword(t *testing.T) {
	loginSvc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: ""})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// newConfigurableBreakGlassTestAPI builds a real *API with loginPath as its
// BREAKGLASS_LOGIN_PATH, so the route-configurability behavior itself (not
// just the credential check) can be exercised against Router()'s real
// wiring.
func newConfigurableBreakGlassTestAPI(t *testing.T, loginPath string) *API {
	t.Helper()
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	loginSvc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(&fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}, testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", loginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return api
}

func TestBreakGlassLoginPath_ConfiguredPath_ServesPageAndAcceptsLogin(t *testing.T) {
	const customPath = "/login/battery/stapler/horse/towel"
	api := newConfigurableBreakGlassTestAPI(t, customPath)
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	getResp, err := client.Get(srv.URL + customPath)
	if err != nil {
		t.Fatalf("GET %s error: %v", customPath, err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", customPath, getResp.StatusCode, http.StatusOK)
	}

	postResp := postJSON(t, client, srv.URL+customPath, breakGlassLoginRequest{Password: "break-glass-password"})
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST %s status = %d, want %d", customPath, postResp.StatusCode, http.StatusNoContent)
	}
}

func TestBreakGlassLoginPath_ConfiguredPath_OldDefaultPath404s(t *testing.T) {
	api := newConfigurableBreakGlassTestAPI(t, "/login/battery/stapler/horse/towel")
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	resp, err := client.Get(srv.URL + "/login/break-glass")
	if err != nil {
		t.Fatalf("GET /login/break-glass error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /login/break-glass status = %d, want %d - the well-known default must not still resolve once a custom path is configured", resp.StatusCode, http.StatusNotFound)
	}
}

func TestBreakGlassLoginPage_FormAction_ReflectsConfiguredPath(t *testing.T) {
	const customPath = "/login/battery/stapler/horse/towel"
	api := newConfigurableBreakGlassTestAPI(t, customPath)
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	resp, err := client.Get(srv.URL + customPath)
	if err != nil {
		t.Fatalf("GET %s error: %v", customPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !strings.Contains(string(body), `action="`+customPath+`"`) {
		t.Errorf("rendered page does not contain action=%q - the form still points at the old default", customPath)
	}
}
