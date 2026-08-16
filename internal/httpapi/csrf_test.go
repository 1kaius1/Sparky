// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testCSRFToken is a fixed value used by addValidCSRF - any value works as
// long as the cookie and the submitted value match, so tests that aren't
// specifically about CSRF don't need to go through ensureCSRFToken's real
// cookie-issuing flow at all.
const testCSRFToken = "test-csrf-token"

// addValidCSRF sets a matching CSRF cookie and header on req, satisfying
// RequireCSRF - used throughout this package's other test files for
// requests to write routes that aren't themselves testing CSRF behavior.
func addValidCSRF(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: testCSRFToken})
	req.Header.Set(csrfHeaderName, testCSRFToken)
}

func TestGenerateCSRFToken_Unique(t *testing.T) {
	a, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken() error: %v", err)
	}
	b, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken() error: %v", err)
	}
	if a == "" {
		t.Error("generateCSRFToken() returned an empty string")
	}
	if a == b {
		t.Errorf("two calls to generateCSRFToken() returned the same value: %q", a)
	}
}

func testAPIForCSRF(t *testing.T) *API {
	t.Helper()
	return &API{logger: testLogger()}
}

func TestEnsureCSRFToken_SetsCookieWhenMissing(t *testing.T) {
	api := testAPIForCSRF(t)
	var gotContextToken string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContextToken = csrfTokenFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	api.ensureCSRFToken(next).ServeHTTP(rec, req)

	var setCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			setCookie = c
		}
	}
	if setCookie == nil {
		t.Fatal("no sparky_csrf cookie was set")
	}
	if setCookie.Value == "" {
		t.Error("sparky_csrf cookie value is empty")
	}
	if gotContextToken != setCookie.Value {
		t.Errorf("context token = %q, want it to match the set cookie %q", gotContextToken, setCookie.Value)
	}
	if !setCookie.HttpOnly {
		t.Error("sparky_csrf cookie is not HttpOnly")
	}
}

func TestEnsureCSRFToken_PreservesExistingCookie(t *testing.T) {
	api := testAPIForCSRF(t)
	var gotContextToken string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContextToken = csrfTokenFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token"})
	rec := httptest.NewRecorder()
	api.ensureCSRFToken(next).ServeHTTP(rec, req)

	if gotContextToken != "existing-token" {
		t.Errorf("context token = %q, want the existing cookie's value %q", gotContextToken, "existing-token")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Errorf("a new sparky_csrf cookie was set (%q) despite a valid one already present", c.Value)
		}
	}
}

func TestRequireCSRF_MatchingCookieAndHeader_Passes(t *testing.T) {
	api := testAPIForCSRF(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/users/target-1/tier", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addValidCSRF(req)
	rec := httptest.NewRecorder()
	api.RequireCSRF(next).ServeHTTP(rec, req)

	if !called {
		t.Errorf("next handler was not called, response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequireCSRF_MatchingCookieAndFormField_Passes(t *testing.T) {
	api := testAPIForCSRF(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/nodes/register", strings.NewReader(csrfFormFieldName+"="+testCSRFToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	api.RequireCSRF(next).ServeHTTP(rec, req)

	if !called {
		t.Errorf("next handler was not called, response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequireCSRF_MissingToken_Rejected(t *testing.T) {
	api := testAPIForCSRF(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/users/target-1/tier", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.RequireCSRF(next).ServeHTTP(rec, req)

	if called {
		t.Error("next handler was called despite no CSRF token being present at all")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRequireCSRF_MismatchedToken_Rejected(t *testing.T) {
	api := testAPIForCSRF(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/users/target-1/tier", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "cookie-value"})
	req.Header.Set(csrfHeaderName, "a-different-value")
	rec := httptest.NewRecorder()
	api.RequireCSRF(next).ServeHTTP(rec, req)

	if called {
		t.Error("next handler was called despite a mismatched CSRF token")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRequireCSRF_JSONRequest_Skipped(t *testing.T) {
	api := testAPIForCSRF(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	// No cookie, no header, no form field at all - a JSON API caller isn't
	// exploitable by a naive cross-site form submission (Content-Type:
	// application/json can't be set by one without JS + a CORS preflight),
	// so RequireCSRF must let it through untouched.
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.RequireCSRF(next).ServeHTTP(rec, req)

	if !called {
		t.Errorf("next handler was not called for a JSON request, response = %d %s", rec.Code, rec.Body.String())
	}
}
