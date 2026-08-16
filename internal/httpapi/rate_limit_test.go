// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
)

// testAuthRateLimitMaxAttempts/testAuthRateLimitWindow are generous defaults
// used to build an *API in every other test file's New() call - large enough
// that no test unrelated to rate limiting itself ever trips the limiter.
const testAuthRateLimitMaxAttempts = 1000

const testAuthRateLimitWindow = time.Hour

func TestLoginRateLimiter_AllowsUpToMaxAttempts(t *testing.T) {
	l := newLoginRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !l.allow("203.0.113.1:5000") {
			t.Fatalf("allow() call %d = false, want true (within maxAttempts)", i+1)
		}
	}
}

func TestLoginRateLimiter_RejectsOverMaxAttempts(t *testing.T) {
	l := newLoginRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		l.allow("203.0.113.1:5000")
	}
	if l.allow("203.0.113.1:5000") {
		t.Error("allow() = true on the attempt past maxAttempts, want false")
	}
}

func TestLoginRateLimiter_AllowsAgainAfterWindowRollsOver(t *testing.T) {
	now := time.Now()
	l := newLoginRateLimiter(1, time.Minute)
	l.nowFunc = func() time.Time { return now }

	if !l.allow("203.0.113.1:5000") {
		t.Fatal("first allow() = false, want true")
	}
	if l.allow("203.0.113.1:5000") {
		t.Fatal("second allow() within the same window = true, want false")
	}

	now = now.Add(time.Minute)
	if !l.allow("203.0.113.1:5000") {
		t.Error("allow() after the window rolled over = false, want true")
	}
}

func TestLoginRateLimiter_IndependentBucketsPerIP(t *testing.T) {
	l := newLoginRateLimiter(1, time.Minute)

	if !l.allow("203.0.113.1:5000") {
		t.Fatal("allow() for first IP = false, want true")
	}
	if !l.allow("203.0.113.2:5000") {
		t.Error("allow() for a different IP = false, want true - buckets should be independent per source IP")
	}
	if l.allow("203.0.113.1:5000") {
		t.Error("second allow() for the first IP = true, want false - its own bucket should still be exhausted")
	}
}

func TestLoginRateLimiter_Allow_MalformedRemoteAddr_FallsBackToWholeString(t *testing.T) {
	l := newLoginRateLimiter(1, time.Minute)

	if !l.allow("not-a-host-port") {
		t.Fatal("first allow() = false, want true")
	}
	if l.allow("not-a-host-port") {
		t.Error("second allow() for the same malformed remoteAddr = true, want false")
	}
}

func TestLoginRateLimiter_Sweep_RemovesExpiredBuckets(t *testing.T) {
	now := time.Now()
	l := newLoginRateLimiter(1, time.Minute)
	l.nowFunc = func() time.Time { return now }

	l.allow("203.0.113.1:5000")
	if len(l.buckets) != 1 {
		t.Fatalf("buckets after one allow() = %d, want 1", len(l.buckets))
	}

	// Past the window, and past the sweep's own gate (>= window since
	// lastSweep) - the next allow() call, for a different IP, should sweep
	// the first IP's now-expired bucket away rather than keeping it
	// forever.
	now = now.Add(2 * time.Minute)
	l.allow("203.0.113.2:5000")

	if _, exists := l.buckets["203.0.113.1"]; exists {
		t.Error("expired bucket for 203.0.113.1 was not swept")
	}
	if len(l.buckets) != 1 {
		t.Errorf("buckets after sweep = %d, want 1 (only the still-live IP)", len(l.buckets))
	}
}

func TestLoginRateLimiter_Middleware_UnderThreshold_CallsNext(t *testing.T) {
	l := newLoginRateLimiter(2, time.Minute)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.1:5000"
	rec := httptest.NewRecorder()

	l.middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Error("middleware did not call next for a request under the threshold")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoginRateLimiter_Middleware_OverThreshold_Returns429(t *testing.T) {
	l := newLoginRateLimiter(1, time.Minute)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.1:5000"
		return req
	}

	l.middleware(next).ServeHTTP(httptest.NewRecorder(), newReq())

	rec := httptest.NewRecorder()
	l.middleware(next).ServeHTTP(rec, newReq())

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if !contentTypeIsJSON(rec) {
		t.Error("429 response Content-Type is not application/json")
	}
}

func contentTypeIsJSON(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get("Content-Type") == "application/json"
}

// newRateLimitTestAPI builds a real *API (not the generous-default
// newTestServer helper) with a low, test-controlled maxAttempts/window, so
// POST /login and POST /login/break-glass's real rate-limit middleware
// wiring in Router() can be exercised end-to-end rather than just
// loginRateLimiter in isolation.
func newRateLimitTestAPI(t *testing.T, maxAttempts int, window time.Duration) *API {
	t.Helper()
	loginSvc := NewLoginService(&fakeIdentityProvider{err: auth.ErrInvalidCredentials}, newFakeUserStore(), testSessionSecret)
	hash, err := auth.HashPassword("break-glass-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	breakGlassSvc := NewBreakGlassLoginService(&fakeBreakGlassStore{cred: &db.BreakGlassCredential{PasswordHash: hash}}, testSessionSecret)
	api, err := New(loginSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, maxAttempts, window, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return api
}

func TestHandleLogin_RateLimited_Returns429AfterMaxAttempts(t *testing.T) {
	api := newRateLimitTestAPI(t, 2, time.Minute)
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	for i := 0; i < 2; i++ {
		resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "wrong"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was rate-limited already, want it to succeed through to the credential check", i+1)
		}
	}

	resp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding maxAttempts = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
}

func TestHandleBreakGlassLogin_RateLimited_Returns429AfterMaxAttempts(t *testing.T) {
	api := newRateLimitTestAPI(t, 2, time.Minute)
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	for i := 0; i < 2; i++ {
		resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "wrong"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was rate-limited already, want it to succeed through to the credential check", i+1)
		}
	}

	resp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding maxAttempts = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
}

func TestLoginAndBreakGlassRateLimiters_AreIndependent(t *testing.T) {
	api := newRateLimitTestAPI(t, 1, time.Minute)
	srv := httptest.NewServer(api.Router())
	defer srv.Close()
	client := newBrowserClient(t)

	loginResp := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "wrong"})
	loginResp.Body.Close()
	if loginResp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("first /login attempt was already rate-limited")
	}
	exhausted := postJSON(t, client, srv.URL+"/login", loginRequest{Username: "jsmith", Password: "wrong"})
	exhausted.Body.Close()
	if exhausted.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("/login status after exhausting its budget = %d, want %d", exhausted.StatusCode, http.StatusTooManyRequests)
	}

	// /login/break-glass, same client (same source IP), should still have
	// its own untouched budget - the two endpoints protect different
	// credentials and must not share a limiter.
	breakGlassResp := postJSON(t, client, srv.URL+"/login/break-glass", breakGlassLoginRequest{Password: "wrong"})
	defer breakGlassResp.Body.Close()
	if breakGlassResp.StatusCode == http.StatusTooManyRequests {
		t.Error("/login/break-glass was rate-limited by /login's own exhausted budget, want independent limiters")
	}
}
