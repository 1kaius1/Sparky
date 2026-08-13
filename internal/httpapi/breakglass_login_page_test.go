// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
)

// newBreakGlassTestServer is newTestServer's break-glass-focused
// equivalent, letting a test configure both the break-glass password and
// BREAKGLASS_ALLOWED_IPS - newTestServer itself always passes "" (allow
// all), which can't exercise the IP-blocked cases below.
func newBreakGlassTestServer(t *testing.T, password, allowedIPs string) *httptest.Server {
	t.Helper()
	var store breakGlassStore = newFakeBreakGlassStore()
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword() error: %v", err)
		}
		store = &fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}
	}
	loginSvc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(store, testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), allowedIPs, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return httptest.NewServer(api.Router())
}

func TestHandleBreakGlassLoginPage_RendersForm(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/login/break-glass")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "<form") || !strings.Contains(body, `name="password"`) {
		t.Errorf("response does not look like the break-glass login form: %s", body)
	}
	if strings.Contains(body, `name="username"`) {
		t.Error("break-glass form must not have a username field")
	}
}

func TestHandleBreakGlassLoginFormSubmit_Success_RedirectsAndSetsCookie(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "")
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login/break-glass", url.Values{"password": {"break-glass-password"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if got := resp.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want %q", got, "/dashboard")
	}
	var setCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			setCookie = c
		}
	}
	if setCookie == nil {
		t.Fatal("no session cookie in the response's Set-Cookie header")
	}
}

func TestHandleBreakGlassLoginFormSubmit_WrongPassword_RerendersWithError(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "")
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login/break-glass", url.Values{"password": {"wrong"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (a failed submission re-renders the page, not a redirect)", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "invalid password") {
		t.Errorf("response does not contain the break-glass error message: %s", buf[:n])
	}
}

func TestHandleBreakGlassLoginFormSubmit_MissingPassword_RerendersWithError(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "")
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login/break-glass", url.Values{"password": {""}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "required") {
		t.Errorf("response does not contain a missing-password error message: %s", buf[:n])
	}
}

func TestHandleBreakGlassLoginPage_IPBlocked_Returns403(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "203.0.113.0/24")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/login/break-glass")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestHandleBreakGlassLogin_IPBlocked_RejectsBeforeCredentialCheck proves
// the whitelist runs ahead of the password check - a wrong password from a
// non-whitelisted IP must still get 403 IP_NOT_ALLOWED, not 401
// INVALID_CREDENTIALS, confirming middleware ordering, not just that both
// checks individually work.
func TestHandleBreakGlassLogin_IPBlocked_RejectsBeforeCredentialCheck(t *testing.T) {
	srv := newBreakGlassTestServer(t, "break-glass-password", "203.0.113.0/24")
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "totally-wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (IP gate must run before the credential check)", resp.StatusCode, http.StatusForbidden)
	}
	var errResp errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Code != "IP_NOT_ALLOWED" {
		t.Errorf("Code = %q, want %q", errResp.Code, "IP_NOT_ALLOWED")
	}
}
