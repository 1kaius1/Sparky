// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/session"
)

func newTestServer(t *testing.T, idp auth.IdentityProvider, store *fakeUserStore) (*httptest.Server, *API) {
	t.Helper()
	svc := NewLoginService(idp, store, testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), testSessionSecret, nil, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(api.Router())
	return srv, api
}

// newBrowserClient simulates a real browser: it carries cookies across
// requests via a cookiejar, same as net/http/cookiejar's RFC 6265
// handling (including honoring the Secure attribute, which is why
// isSecureRequest must correctly report false for these plain-HTTP test
// requests, or the session cookie set on login would never be sent back).
func newBrowserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error: %v", err)
	}
	return &http.Client{Jar: jar}
}

func postJSON(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s error: %v", url, err)
	}
	return resp
}

func TestLoginLogout_FullBrowserRoundTrip(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID:         "S-1-5-21-1",
		DisplayName:   "Jane Smith",
		InAccessGroup: true,
	}}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	// 1. Log in.
	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "password"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /login status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header missing on /login response")
	}

	var loginResp loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginResp.DisplayName != "Jane Smith" {
		t.Errorf("DisplayName = %q, want %q", loginResp.DisplayName, "Jane Smith")
	}

	// 2a. The Set-Cookie header on the login response itself is where
	// HttpOnly/Secure/SameSite actually live - resp.Cookies() parses that
	// header directly. A cookiejar's Cookies() method, checked next,
	// returns only what belongs in an outgoing Cookie: header (RFC 6265
	// SS4.2), which never carries those response-only attributes - even a
	// correctly HttpOnly cookie would read back as false there.
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

	// 2b. The cookiejar should now hold that cookie for the test server's
	// origin, ready to be sent on the next request - this is the "browser
	// received a valid session cookie" bar from PLANNING.md's Phase 3
	// criteria.
	reqURL, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	cookies := client.Jar.Cookies(reqURL.URL)
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie was stored after login")
	}
	sess, err := session.Verify(testSessionSecret, sessionCookie.Value)
	if err != nil {
		t.Fatalf("session.Verify() on the browser's stored cookie: %v", err)
	}
	if sess.UserID == "" {
		t.Error("session UserID is empty")
	}

	// 3. Log out - the cookie should be cleared.
	resp2, err := client.Post(srv.URL+"/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /logout error: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /logout status = %d, want %d", resp2.StatusCode, http.StatusNoContent)
	}

	cookiesAfterLogout := client.Jar.Cookies(reqURL.URL)
	for _, c := range cookiesAfterLogout {
		if c.Name == sessionCookieName {
			t.Error("session cookie is still present after logout")
		}
	}
}

func TestHandleLogin_WrongPassword(t *testing.T) {
	idp := &fakeIdentityProvider{err: auth.ErrInvalidCredentials}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "wrong"})
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
	if errResp.RequestID == "" {
		t.Error("RequestID is empty in error response")
	}
}

func TestHandleLogin_AccessDenied(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", InAccessGroup: false,
	}}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "password"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandleLogin_MissingFields(t *testing.T) {
	idp := &fakeIdentityProvider{}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "", Password: ""})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandleLogin_MalformedBody(t *testing.T) {
	idp := &fakeIdentityProvider{}
	srv, _ := newTestServer(t, idp, newFakeUserStore())
	defer srv.Close()
	client := newBrowserClient(t)

	resp, err := client.Post(srv.URL+"/login", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("POST /login error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestRequireSession(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", InAccessGroup: true,
	}}
	svc := NewLoginService(idp, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), testSessionSecret, nil, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var gotIdentity Identity
	protected := api.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity, _ = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(protected)
	defer srv.Close()

	t.Run("no cookie", func(t *testing.T) {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("invalid cookie", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "garbage"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("valid cookie", func(t *testing.T) {
		cookieValue, err := session.Sign(testSessionSecret, session.New("user-42", sessionDuration))
		if err != nil {
			t.Fatalf("session.Sign() error: %v", err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if gotIdentity.UserID != "user-42" {
			t.Errorf("Identity.UserID = %q, want %q", gotIdentity.UserID, "user-42")
		}
		if gotIdentity.IsSuperAdmin {
			t.Error("Identity.IsSuperAdmin = true, want false for a regular user session")
		}
	})

	t.Run("valid SuperAdmin cookie", func(t *testing.T) {
		cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
		if err != nil {
			t.Fatalf("session.Sign() error: %v", err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if !gotIdentity.IsSuperAdmin {
			t.Error("Identity.IsSuperAdmin = false, want true")
		}
		if gotIdentity.UserID != "" {
			t.Errorf("Identity.UserID = %q, want empty for a SuperAdmin session", gotIdentity.UserID)
		}
	})
}
