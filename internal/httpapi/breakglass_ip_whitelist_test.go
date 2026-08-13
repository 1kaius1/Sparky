// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewBreakGlassIPWhitelist_InvalidEntry_ReturnsError(t *testing.T) {
	if _, err := newBreakGlassIPWhitelist("not-an-ip", testLogger()); err == nil {
		t.Fatal("newBreakGlassIPWhitelist() succeeded on a malformed entry, want an error")
	}
	if _, err := newBreakGlassIPWhitelist("10.0.0.0/99", testLogger()); err == nil {
		t.Fatal("newBreakGlassIPWhitelist() succeeded on an invalid CIDR, want an error")
	}
}

func TestBreakGlassIPWhitelist_Empty_AllowsAnyIP(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	if !g.allowed("203.0.113.5:12345") {
		t.Error("allowed() = false with an empty whitelist, want true")
	}
}

func TestBreakGlassIPWhitelist_SingleIP(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("127.0.0.1", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	if !g.allowed("127.0.0.1:5555") {
		t.Error("allowed(127.0.0.1) = false, want true")
	}
	if g.allowed("203.0.113.5:5555") {
		t.Error("allowed(203.0.113.5) = true, want false")
	}
}

func TestBreakGlassIPWhitelist_CIDRRange(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("10.0.0.0/24", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	if !g.allowed("10.0.0.42:1") {
		t.Error("allowed(10.0.0.42) = false, want true (inside /24)")
	}
	if g.allowed("10.0.1.42:1") {
		t.Error("allowed(10.0.1.42) = true, want false (outside /24)")
	}
}

func TestBreakGlassIPWhitelist_IPv6(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("::1,2001:db8::/32", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	if !g.allowed("[::1]:1234") {
		t.Error("allowed(::1) = false, want true")
	}
	if !g.allowed("[2001:db8::1]:1234") {
		t.Error("allowed(2001:db8::1) = false, want true (inside /32)")
	}
	if g.allowed("[2001:db9::1]:1234") {
		t.Error("allowed(2001:db9::1) = true, want false")
	}
}

func TestBreakGlassIPWhitelist_MalformedRemoteAddr_NoPort(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("203.0.113.5", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	if !g.allowed("203.0.113.5") {
		t.Error("allowed() with no port suffix = false, want true (defensive host fallback)")
	}
}

func newWhitelistTestServer(g *breakGlassIPWhitelist) *httptest.Server {
	handler := g.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	return httptest.NewServer(handler)
}

func TestBreakGlassIPWhitelistMiddleware_Rejected_Returns403IPNotAllowed(t *testing.T) {
	// httptest's real client dials from 127.0.0.1, so a whitelist that
	// excludes it proves the middleware runs and blocks before next.
	g, err := newBreakGlassIPWhitelist("203.0.113.0/24", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	srv := newWhitelistTestServer(g)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	var errResp errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Code != "IP_NOT_ALLOWED" {
		t.Errorf("Code = %q, want %q", errResp.Code, "IP_NOT_ALLOWED")
	}
}

func TestBreakGlassIPWhitelistMiddleware_Allowed_CallsNext(t *testing.T) {
	g, err := newBreakGlassIPWhitelist("", testLogger())
	if err != nil {
		t.Fatalf("newBreakGlassIPWhitelist() error: %v", err)
	}
	srv := newWhitelistTestServer(g)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
