// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
)

// breakGlassIPWhitelist gates the break-glass login route (both the GUI page
// and the JSON endpoint, wherever BREAKGLASS_LOGIN_PATH mounts them - see
// router.go) to a configured set of source IPs/CIDR ranges. Client IP is
// taken from r.RemoteAddr (the direct TCP peer), not
// any X-Forwarded-* header - confirmed with the user (PLANNING.md Decisions
// Log): unlike isSecureRequest's X-Forwarded-Proto trust (a value only the
// reverse proxy itself sets, and only ever affects a cookie flag), a
// whitelist is an access-control decision and must not trust a header the
// client can set directly. This means RemoteAddr is the proxy's own address
// in the reverse-proxy topology ARCHITECTURE.md's Request Lifecycle
// documents for production - an accepted, documented tradeoff for this
// control's actual motivating use case (direct-connection local/break-glass
// testing), not something a trusted-proxy config concept is being
// introduced to solve here.
//
// An empty/unset BREAKGLASS_ALLOWED_IPS allows from anywhere - same
// off-by-default shape as AuditForwardEnabled - so upgrading to a version
// with this middleware never silently locks out an existing break-glass API
// caller that hasn't configured the new variable.
type breakGlassIPWhitelist struct {
	nets   []*net.IPNet // empty => allow every source IP
	logger *log.Logger
}

// newBreakGlassIPWhitelist parses rawList (BREAKGLASS_ALLOWED_IPS - a
// comma-separated list of IPs and/or CIDR ranges, e.g.
// "127.0.0.1,10.0.0.0/24") once at construction, same as loadPageTemplates
// parsing templates once at startup rather than per request. A single IP
// (no "/") is treated as a /32 (IPv4) or /128 (IPv6) host route. Returns an
// error on any malformed entry - a startup-time config mistake here should
// fail fast, matching config.Load()'s own fail-fast validation, not
// silently exclude an operator's intended IP.
func newBreakGlassIPWhitelist(rawList string, logger *log.Logger) (*breakGlassIPWhitelist, error) {
	trimmed := strings.TrimSpace(rawList)
	if trimmed == "" {
		return &breakGlassIPWhitelist{logger: logger}, nil
	}

	var nets []*net.IPNet
	for _, entry := range strings.Split(trimmed, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", entry)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			entry = fmt.Sprintf("%s/%d", entry, bits)
		}
		_, ipNet, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", entry, err)
		}
		nets = append(nets, ipNet)
	}
	return &breakGlassIPWhitelist{nets: nets, logger: logger}, nil
}

// allowed reports whether remoteAddr (an http.Request.RemoteAddr-shaped
// "host:port" string) is permitted. A malformed remoteAddr with no port
// (not expected from net/http itself, but defended against rather than
// panicking or 500ing) falls back to treating the whole string as the
// host.
func (g *breakGlassIPWhitelist) allowed(remoteAddr string) bool {
	if len(g.nets) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range g.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (g *breakGlassIPWhitelist) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.allowed(r.RemoteAddr) {
			if g.logger != nil {
				g.logger.Printf("httpapi: break-glass login rejected: source %s not in BREAKGLASS_ALLOWED_IPS", r.RemoteAddr)
			}
			writeError(w, r, http.StatusForbidden, "IP_NOT_ALLOWED", "not allowed from this network")
			return
		}
		next.ServeHTTP(w, r)
	})
}
