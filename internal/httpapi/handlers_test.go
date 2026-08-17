// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/session"
)

func newTestServer(t *testing.T, idp auth.IdentityProvider, store *fakeUserStore) (*httptest.Server, *API) {
	t.Helper()
	svc := NewLoginService(idp, store, testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
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
	users := newFakeUserLister()
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
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
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want %q", loc, "/login")
		}
	})

	t.Run("invalid cookie", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "garbage"})
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want %q", loc, "/login")
		}
	})

	t.Run("htmx request without session", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("HX-Request", "true")
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
		if hx := resp.Header.Get("HX-Redirect"); hx != "/login" {
			t.Errorf("HX-Redirect = %q, want %q", hx, "/login")
		}
	})

	t.Run("valid cookie", func(t *testing.T) {
		idp.isInAccessGroupCalls = nil
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
		// A freshly-minted session (LastVerifiedAt == now) is nowhere near
		// stale - the recheck must not have fired at all.
		if len(idp.isInAccessGroupCalls) != 0 {
			t.Errorf("IsInAccessGroup called %d times for a fresh session, want 0", len(idp.isInAccessGroupCalls))
		}
	})

	t.Run("stale session still in group refreshes the cookie", func(t *testing.T) {
		idp.isInAccessGroupCalls = nil
		idp.isInAccessGroupErr = nil
		idp.isInAccessGroupResult = true
		dn := "CN=stillmember,DC=example,DC=internal"
		users.byID["user-stale-ok"] = &db.User{ID: "user-stale-ok", LDAPDN: &dn}

		staleSess := session.Session{
			UserID:         "user-stale-ok",
			ExpiresAt:      time.Now().UTC().Add(sessionDuration),
			LastVerifiedAt: time.Now().UTC().Add(-2 * testAuthRecheckInterval),
		}
		cookieValue, err := session.Sign(testSessionSecret, staleSess)
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
		if gotIdentity.UserID != "user-stale-ok" {
			t.Errorf("Identity.UserID = %q, want %q", gotIdentity.UserID, "user-stale-ok")
		}
		if len(idp.isInAccessGroupCalls) != 1 || idp.isInAccessGroupCalls[0] != dn {
			t.Errorf("IsInAccessGroup calls = %v, want exactly one call with %q", idp.isInAccessGroupCalls, dn)
		}

		var newCookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookieName {
				newCookie = c
			}
		}
		if newCookie == nil {
			t.Fatal("no refreshed session cookie in the response")
		}
		refreshed, err := session.Verify(testSessionSecret, newCookie.Value)
		if err != nil {
			t.Fatalf("session.Verify() on the refreshed cookie error: %v", err)
		}
		if !refreshed.LastVerifiedAt.After(staleSess.LastVerifiedAt) {
			t.Errorf("refreshed LastVerifiedAt = %v, want after the stale value %v", refreshed.LastVerifiedAt, staleSess.LastVerifiedAt)
		}
		if !refreshed.ExpiresAt.Equal(staleSess.ExpiresAt) {
			t.Errorf("refreshed ExpiresAt = %v, want unchanged %v - a recheck must not extend the absolute session lifetime", refreshed.ExpiresAt, staleSess.ExpiresAt)
		}
	})

	t.Run("stale session no longer in group forces logout", func(t *testing.T) {
		idp.isInAccessGroupCalls = nil
		idp.isInAccessGroupErr = nil
		idp.isInAccessGroupResult = false
		dn := "CN=removed,DC=example,DC=internal"
		users.byID["user-stale-removed"] = &db.User{ID: "user-stale-removed", LDAPDN: &dn}

		staleSess := session.Session{
			UserID:         "user-stale-removed",
			ExpiresAt:      time.Now().UTC().Add(sessionDuration),
			LastVerifiedAt: time.Now().UTC().Add(-2 * testAuthRecheckInterval),
		}
		cookieValue, err := session.Sign(testSessionSecret, staleSess)
		if err != nil {
			t.Fatalf("session.Sign() error: %v", err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want %q", loc, "/login")
		}
		var cleared *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookieName {
				cleared = c
			}
		}
		if cleared == nil || cleared.MaxAge >= 0 {
			t.Errorf("session cookie = %+v, want a cleared (MaxAge < 0) cookie", cleared)
		}
	})

	t.Run("stale session, LDAP unreachable, fails open", func(t *testing.T) {
		idp.isInAccessGroupCalls = nil
		idp.isInAccessGroupErr = errors.New("LDAP unreachable")
		dn := "CN=cantverify,DC=example,DC=internal"
		users.byID["user-stale-ldapdown"] = &db.User{ID: "user-stale-ldapdown", LDAPDN: &dn}

		staleSess := session.Session{
			UserID:         "user-stale-ldapdown",
			ExpiresAt:      time.Now().UTC().Add(sessionDuration),
			LastVerifiedAt: time.Now().UTC().Add(-2 * testAuthRecheckInterval),
		}
		cookieValue, err := session.Sign(testSessionSecret, staleSess)
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
		// Fail open: an LDAP error must not force a logout - the request
		// proceeds on the existing session exactly as if no recheck had
		// been due, per PLANNING.md's Decisions Log.
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d (fail open on an LDAP error)", resp.StatusCode, http.StatusOK)
		}
		if gotIdentity.UserID != "user-stale-ldapdown" {
			t.Errorf("Identity.UserID = %q, want %q", gotIdentity.UserID, "user-stale-ldapdown")
		}
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookieName {
				t.Errorf("session cookie was rewritten (%+v) despite the recheck itself failing - nothing should change on fail-open", c)
			}
		}
	})

	t.Run("stale SuperAdmin session is never rechecked", func(t *testing.T) {
		idp.isInAccessGroupCalls = nil
		// If RequireSession mistakenly tried to recheck a SuperAdmin
		// session, this would force a lookup of an empty UserID, which
		// isn't in users.byID - findErr set here so any such lookup fails
		// loudly rather than silently succeeding by coincidence.
		users.findErr = errors.New("should not be called for a SuperAdmin session")
		defer func() { users.findErr = nil }()

		staleSess := session.Session{
			IsSuperAdmin:   true,
			ExpiresAt:      time.Now().UTC().Add(sessionDuration),
			LastVerifiedAt: time.Now().UTC().Add(-2 * testAuthRecheckInterval),
		}
		cookieValue, err := session.Sign(testSessionSecret, staleSess)
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
		if len(idp.isInAccessGroupCalls) != 0 {
			t.Errorf("IsInAccessGroup called %d times for a SuperAdmin session, want 0", len(idp.isInAccessGroupCalls))
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
