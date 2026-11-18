// SPDX-License-Identifier: AGPL-3.0-or-later

// Package entra signs people in through Microsoft Entra ID: the OpenID Connect
// authorization code flow with PKCE, a confidential client, one tenant. See
// docs/ENTRA_DESIGN.md sections 3 and 8 for the design and for how it is tested.
//
// It knows nothing about Sparky's users, sessions or HTTP routes. It builds the
// address to send a person to, takes what Entra sends back, and returns who the
// person is (their object ID) or a typed error - the calling layer decides what
// to do with either. Group and person lookups through Microsoft Graph are a
// separate step and are not here.
//
// Verified against a mock only: the fake tenant in the fakeentra subpackage is
// written from Microsoft's documentation and has not been compared with a real
// tenant. The sparky-entra-probe program exists to do that comparison.
package entra

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// PublicCloudAuthority is the public cloud's sign-in host. Other clouds
	// (government, China) are not supported and not tested.
	PublicCloudAuthority = "https://login.microsoftonline.com"

	// GraphScope is the scope that asks for a token carrying the app's own
	// Microsoft Graph application permissions: the resource identifier plus
	// ".default". The identifier is the same in every cloud this package
	// supports, so it is a constant rather than derived from a configured URL.
	GraphScope = "https://graph.microsoft.com/.default"

	// ClockSkew is how far this server's clock may be ahead of Entra's before a
	// freshly issued token is refused as expired. The ID token is checked once,
	// at sign-in, and lives an hour, so a minute costs nothing.
	ClockSkew = time.Minute

	// FlowTTL is how long a person has to finish signing in at Entra.
	FlowTTL = 10 * time.Minute

	defaultHTTPTimeout = 15 * time.Second

	// maxResponseBytes bounds every response read, so a misbehaving endpoint
	// cannot make the server buffer without limit.
	maxResponseBytes = 1 << 20
)

var guidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsGUID reports whether s is a GUID in the 8-4-4-4-12 form Entra uses for
// tenant, application and object IDs.
func IsGUID(s string) bool { return guidRE.MatchString(s) }

// Config is everything the sign-in needs. The tenant and client IDs and the
// secret come from the environment (ENTRA_TENANT_ID, ENTRA_CLIENT_ID,
// ENTRA_CLIENT_SECRET); the redirect URL is built by the caller from
// SPARKY_PUBLIC_URL.
type Config struct {
	// TenantID is the one tenant that may sign in. It must be the tenant's
	// GUID, not a domain name: the issuer Entra puts in its tokens contains the
	// GUID, and the issuer is compared exactly.
	TenantID string

	// ClientID is the app registration's application (client) ID.
	ClientID string

	// ClientSecret is a client secret of that app registration.
	ClientSecret Secret

	// AuthorityURL is the sign-in host. Empty means PublicCloudAuthority. Plain
	// http is accepted only for a loopback host, the same rule the mailer
	// applies, so a test can point at a fake tenant but a typo cannot send the
	// secret across a network in clear text.
	AuthorityURL string

	// RedirectURL is where Entra sends the person back to, exactly as it is
	// registered on the app registration.
	RedirectURL string

	// HTTPClient is used for every request to Entra. Nil means a client with a
	// 15 second timeout. Its redirect policy is replaced: a redirect from
	// Entra's endpoints is never followed, because a 307 would re-send the
	// client secret to wherever it points.
	HTTPClient *http.Client

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// Normalized checks the configuration and returns a copy with the defaults
// filled in and the IDs in lower case. The errors name fields, never values.
func (c Config) Normalized() (Config, error) {
	c.TenantID = strings.ToLower(strings.TrimSpace(c.TenantID))
	c.ClientID = strings.ToLower(strings.TrimSpace(c.ClientID))
	if !IsGUID(c.TenantID) {
		return Config{}, errors.New("entra: the tenant ID must be the tenant's GUID")
	}
	if !IsGUID(c.ClientID) {
		return Config{}, errors.New("entra: the client ID must be the application's GUID")
	}
	if strings.TrimSpace(c.ClientSecret.Reveal()) == "" {
		return Config{}, errors.New("entra: the client secret is empty")
	}

	authority := strings.TrimSpace(c.AuthorityURL)
	if authority == "" {
		authority = PublicCloudAuthority
	}
	authority, err := checkAuthority(authority)
	if err != nil {
		return Config{}, err
	}
	c.AuthorityURL = authority

	redirect, err := url.Parse(strings.TrimSpace(c.RedirectURL))
	if err != nil || (redirect.Scheme != "http" && redirect.Scheme != "https") || redirect.Host == "" ||
		redirect.User != nil || redirect.Fragment != "" {
		return Config{}, errors.New("entra: the redirect URL must be an absolute http or https URL without a fragment")
	}
	c.RedirectURL = redirect.String()
	return c, nil
}

// IssuerURL is the issuer Entra puts in this tenant's v2.0 ID tokens, and the
// base of its discovery document. It is only meaningful on a normalized Config.
func (c Config) IssuerURL() string { return c.AuthorityURL + "/" + c.TenantID + "/v2.0" }

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// httpClient returns the configured client with redirects disabled. The caller's
// client is copied, not modified.
func (c Config) httpClient() *http.Client {
	var hc http.Client
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
	}
	if hc.Timeout == 0 {
		hc.Timeout = defaultHTTPTimeout
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// checkAuthority validates an authority URL and returns it without a trailing
// slash.
func checkAuthority(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", errors.New("entra: the authority URL must be a scheme and host only")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", errors.New("entra: the authority URL may use plain http only for a loopback host")
		}
	default:
		return "", fmt.Errorf("entra: the authority URL scheme must be https")
	}
	return u.Scheme + "://" + u.Host, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
