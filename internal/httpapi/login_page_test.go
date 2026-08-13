// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/session"
)

// noRedirectClient behaves like newBrowserClient but stops at the first
// redirect instead of following it, so a test can inspect the redirect
// response itself (Location header, Set-Cookie) rather than whatever it
// points at.
func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(target, values)
	if err != nil {
		t.Fatalf("POST %s error: %v", target, err)
	}
	return resp
}

func TestHandleLoginPage_RendersForm(t *testing.T) {
	srv, _ := newTestServer(t, &fakeIdentityProvider{}, newFakeUserStore())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "<form") || !strings.Contains(body, `name="username"`) {
		t.Errorf("response does not look like the login form: %s", body)
	}
}

func TestHandleLoginPage_AlreadyAuthenticated_RedirectsToDashboard(t *testing.T) {
	srv, _ := newTestServer(t, &fakeIdentityProvider{}, newFakeUserStore())
	defer srv.Close()
	client := noRedirectClient(t)

	cookieValue, err := session.Sign(testSessionSecret, session.New("user-1", sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/login", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /login error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if got := resp.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want %q", got, "/dashboard")
	}
}

func TestHandleLoginFormSubmit_Success_RedirectsAndSetsCookie(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID:         "S-1-5-21-1",
		DisplayName:   "Jane Smith",
		InAccessGroup: true,
	}}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login", url.Values{"username": {"jsmith"}, "password": {"password"}})
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

func TestHandleLoginFormSubmit_WrongPassword_RerendersWithError(t *testing.T) {
	idp := &fakeIdentityProvider{err: auth.ErrInvalidCredentials}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login", url.Values{"username": {"jsmith"}, "password": {"wrong"}})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (a failed form submission re-renders the page, not a redirect)", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "invalid username or password") {
		t.Errorf("response does not contain the login error message: %s", body)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a session cookie was set despite a failed login")
	}
}

func TestHandleLoginFormSubmit_AccessDenied_RerendersWithError(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", InAccessGroup: false,
	}}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login", url.Values{"username": {"jsmith"}, "password": {"password"}})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "not authorized") {
		t.Errorf("response does not contain the access-denied error message: %s", buf[:n])
	}
}

func TestHandleLoginFormSubmit_MissingFields_RerendersWithError(t *testing.T) {
	srv, _ := newTestServer(t, &fakeIdentityProvider{}, newFakeUserStore())
	defer srv.Close()
	client := noRedirectClient(t)

	resp := postForm(t, client, srv.URL+"/login", url.Values{"username": {""}, "password": {""}})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "required") {
		t.Errorf("response does not contain a missing-fields error message: %s", buf[:n])
	}
}

func TestHandleLogout_SetsHXRedirectHeader(t *testing.T) {
	srv, _ := newTestServer(t, &fakeIdentityProvider{}, newFakeUserStore())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /logout error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want %q", got, "/login")
	}
}

func TestHandleLogin_JSONRequest_UnaffectedByFormBranch(t *testing.T) {
	// Regression check: adding the form-submission branch to handleLogin
	// must not change behavior for the existing JSON API contract - see
	// TestLoginLogout_FullBrowserRoundTrip in handlers_test.go for the
	// full round trip this only spot-checks the routing side of.
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", InAccessGroup: true,
	}}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "password"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (JSON login must still return 200 with a JSON body, not a redirect)", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}
