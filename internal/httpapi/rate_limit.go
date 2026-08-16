// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginRateLimiter throttles POST attempts against an authentication endpoint
// to maxAttempts per window, per source IP - CLAUDE.md Security
// Considerations ("Rate limiting applied to all authentication endpoints").
// Hand-rolled rather than a new Go module dependency (CLAUDE.md Dependencies)
// - same "own it" precedent as csrf.go and breakglass_ip_whitelist.go. A
// fixed window, not a sliding window or token bucket - proportionate
// precision for this app's scale (CLAUDE.md: "a handful of nodes... not a
// multi-tenant or hyperscale product").
type loginRateLimiter struct {
	maxAttempts int
	window      time.Duration
	// nowFunc defaults to time.Now; tests in this package override it
	// directly for deterministic window-boundary behavior without
	// real sleeps.
	nowFunc func() time.Time

	mu        sync.Mutex
	buckets   map[string]*rateLimitBucket
	lastSweep time.Time
}

type rateLimitBucket struct {
	count       int
	windowStart time.Time
}

// newLoginRateLimiter constructs a limiter allowing maxAttempts requests per
// source IP within window before rejecting further ones until the window
// rolls over.
func newLoginRateLimiter(maxAttempts int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		maxAttempts: maxAttempts,
		window:      window,
		nowFunc:     time.Now,
		buckets:     make(map[string]*rateLimitBucket),
	}
}

// allow reports whether remoteAddr (an http.Request.RemoteAddr-shaped
// "host:port" string) may make another attempt right now, recording the
// attempt if so. Client IP is taken from remoteAddr (the direct TCP peer),
// never any X-Forwarded-* header - same reasoning as
// breakGlassIPWhitelist.allowed: an access-control decision must not trust a
// header the client can set directly.
func (l *loginRateLimiter) allow(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	now := l.nowFunc()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[host]
	if !ok || now.Sub(b.windowStart) >= l.window {
		l.buckets[host] = &rateLimitBucket{count: 1, windowStart: now}
		return true
	}
	if b.count >= l.maxAttempts {
		return false
	}
	b.count++
	return true
}

// sweepLocked removes buckets whose own window has already expired, once per
// window (gated by lastSweep) rather than on every call - bounds map growth
// to roughly the number of distinct source IPs seen within the last window
// instead of growing unboundedly for the life of the process, without
// needing a background goroutine or shutdown lifecycle. Caller must hold
// l.mu.
func (l *loginRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	for ip, b := range l.buckets {
		if now.Sub(b.windowStart) >= l.window {
			delete(l.buckets, ip)
		}
	}
	l.lastSweep = now
}

// middleware rejects a request with 429 RATE_LIMITED once its source IP has
// exceeded maxAttempts within window, same shape as
// breakGlassIPWhitelist.middleware/RequireCSRF.
func (l *loginRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(r.RemoteAddr) {
			writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts, try again later")
			return
		}
		next.ServeHTTP(w, r)
	})
}
