// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fakeentra is a fake Microsoft Entra ID tenant: the sign-in half of it
// (discovery, signing keys, authorize, token), served from httptest, for the
// tests of everything that talks to Entra. See docs/ENTRA_DESIGN.md section 8.
//
// It is written from Microsoft's public documentation, not from a real tenant,
// and says so: wherever a detail could not be taken from a page that was read,
// the comment says UNVERIFIED, and the sparky-entra-probe program is how those
// are checked against a real tenant. Anything that passes against this package
// has been "verified against a mock only" and may be described no more
// strongly.
//
// It is strict on purpose - a fake that accepts what Entra would refuse makes
// the code under test look better than it is. It refuses a redirect address
// that is not registered, a missing or wrong PKCE verifier, a reused or
// expired code, a wrong client secret, a code redeemed by another client or
// for another redirect address, and a wrong or missing tenant in the path. And
// it reproduces awkward cases on demand: key rotation, a key that is not
// published, the wrong signing algorithm, another tenant's token, a skewed
// clock, an endpoint that is down, slow, or redirects.
package fakeentra

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Names of the endpoints, for Requests.
const (
	EndpointDiscovery = "discovery"
	EndpointKeys      = "keys"
	EndpointAuthorize = "authorize"
	EndpointToken     = "token"
)

// graphAppID is Microsoft Graph's well-known application ID, the audience of a
// token issued for GraphScope.
const graphAppID = "00000003-0000-0000-c000-000000000000"

// codeLifetime is how long an authorization code is redeemable. Microsoft says
// codes "typically expire after about 1 minute".
const codeLifetime = time.Minute

// Person is someone the fake can sign in.
type Person struct {
	OID               string
	DisplayName       string
	PreferredUsername string
	Email             string
}

// Options configure a Server. The IDs, secret and redirect addresses are what
// would be on the app registration.
type Options struct {
	TenantID     string
	ClientID     string
	ClientSecret string

	// RedirectURIs are the registered redirect addresses. Matching follows
	// Microsoft's rules: exact and case-sensitive, except that the port of a
	// localhost address is ignored.
	RedirectURIs []string

	// GraphRoles are the application permissions the app has been granted,
	// listed in the roles claim of a client credentials token.
	GraphRoles []string

	// AllowNoPKCE lets an authorize request without a code challenge through.
	// Microsoft only recommends PKCE for confidential clients, so a real tenant
	// accepts that; this fake does not by default, because Sparky must always
	// send one.
	AllowNoPKCE bool
}

type grant struct {
	clientID    string
	redirectURI string
	challenge   string
	oid         string
	nonce       string
	scope       string
	issued      time.Time
	used        bool
}

// Server is a running fake tenant.
type Server struct {
	opts Options
	hs   *httptest.Server

	mu            sync.Mutex
	published     []*Key
	nextKeyIndex  int
	unpubIndex    int
	codes         map[string]*grant
	people        map[string]Person
	current       string
	offset        time.Duration
	down          bool
	delay         time.Duration
	discoveryHook func(map[string]any)
	idHook        func(*IDTokenSpec)
	tokenRedirect string
	counts        map[string]int
	tokenForms    []url.Values
	issued        []string
}

// New starts a fake tenant. Close it when done.
func New(opts Options) *Server {
	s := &Server{
		opts:   opts,
		codes:  map[string]*grant{},
		people: map[string]Person{},
		counts: map[string]int{},
	}
	s.published = []*Key{poolKey(0)}
	s.nextKeyIndex = 1
	s.unpubIndex = 1000

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", s.guard(EndpointDiscovery, s.handleDiscovery))
	mux.HandleFunc("GET /{tenant}/discovery/v2.0/keys", s.guard(EndpointKeys, s.handleKeys))
	mux.HandleFunc("GET /{tenant}/oauth2/v2.0/authorize", s.guard(EndpointAuthorize, s.handleAuthorize))
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", s.guard(EndpointToken, s.handleToken))
	s.hs = httptest.NewServer(mux)
	return s
}

// Close stops the server.
func (s *Server) Close() { s.hs.Close() }

// URL is the authority: the scheme and host to put in ENTRA_AUTHORITY_URL.
func (s *Server) URL() string { return s.hs.URL }

// Issuer is the issuer of the tenant's v2.0 ID tokens.
func (s *Server) Issuer() string { return s.base() + "/v2.0" }

func (s *Server) base() string { return s.hs.URL + "/" + strings.ToLower(s.opts.TenantID) }

// Options returns the options the server was started with.
func (s *Server) Options() Options { return s.opts }

// AddPerson registers someone who can sign in. The first person added signs in
// until SignInAs says otherwise.
func (s *Server) AddPerson(p Person) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.people[p.OID] = p
	if s.current == "" {
		s.current = p.OID
	}
}

// SignInAs picks who the next sign-ins are for, as a real tenant's login page
// would by whoever is typing into it.
func (s *Server) SignInAs(oid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = oid
}

// RotateKeys makes a new key the signing key. With keepOld the previous keys
// stay in the key set (as a real tenant keeps them for a while); without it
// they are removed, so a client that cached the old set sees a token it can
// only verify after fetching the set again.
func (s *Server) RotateKeys(keepOld bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := poolKey(s.nextKeyIndex)
	s.nextKeyIndex++
	if keepOld {
		s.published = append(s.published, k)
	} else {
		s.published = []*Key{k}
	}
}

// CurrentKey is the key new tokens are signed with.
func (s *Server) CurrentKey() *Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published[len(s.published)-1]
}

// NewUnpublishedKey returns a signing key that is not in the key set.
func (s *Server) NewUnpublishedKey() *Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := poolKey(s.unpubIndex)
	s.unpubIndex++
	return k
}

// OnIDToken changes the next ID token the server issues, and only that one.
func (s *Server) OnIDToken(fn func(*IDTokenSpec)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idHook = fn
}

// MutateDiscovery changes the discovery document before it is served.
func (s *Server) MutateDiscovery(fn func(doc map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveryHook = fn
}

// SetRedirectURIs replaces the registered redirect addresses.
func (s *Server) SetRedirectURIs(uris []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.RedirectURIs = append([]string(nil), uris...)
}

// SetDown makes every endpoint answer 503.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetDelay makes every endpoint wait before answering.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetClockOffset moves the server's clock, for skew tests: tokens are issued
// with times computed from it.
func (s *Server) SetClockOffset(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset = d
}

// RedirectTokenEndpoint makes the token endpoint answer 307 to location, as a
// misbehaving proxy might. A client that follows it sends its secret there.
func (s *Server) RedirectTokenEndpoint(location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenRedirect = location
}

// Requests is how many times an endpoint (EndpointDiscovery, ...) was called.
func (s *Server) Requests(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[endpoint]
}

// TokenForms returns the form bodies sent to the token endpoint, oldest first.
func (s *Server) TokenForms() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.tokenForms...)
}

// IssuedTokens returns every ID token and app token the server has issued, oldest
// first, so a test can check that none of them ends up somewhere it must not.
func (s *Server) IssuedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

func (s *Server) record(token string) string {
	s.mu.Lock()
	s.issued = append(s.issued, token)
	s.mu.Unlock()
	return token
}

func (s *Server) now() time.Time { return time.Now().Add(s.offset) }

// guard counts the call and applies the down and delay switches.
func (s *Server) guard(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.counts[name]++
		down, delay := s.down, s.delay
		s.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if down {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
			return
		}
		h(w, r)
	}
}

// tenantOK reports whether the {tenant} in the path is this tenant.
func (s *Server) tenantOK(r *http.Request) bool {
	return strings.EqualFold(r.PathValue("tenant"), s.opts.TenantID)
}

// pageError is the plain error page Entra shows in the browser for a problem it
// will not redirect back to the app for (an unregistered redirect address, an
// unknown application): it never redirects there, since the address cannot be
// trusted.
func pageError(w http.ResponseWriter, status, aadsts int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "AADSTS%d: %s\n", aadsts, text)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleDiscovery serves the OpenID Connect discovery document.
//
// Source: https://learn.microsoft.com/en-us/entra/identity-platform/v2-protocols-oidc
// documents authorization_endpoint, token_endpoint,
// token_endpoint_auth_methods_supported, jwks_uri, userinfo_endpoint and
// subject_types_supported, and says its sample is "truncated for brevity".
//
// UNVERIFIED: issuer, response_types_supported and
// id_token_signing_alg_values_supported are not in that sample. They are the
// standard OpenID Connect members, and the issuer form
// https://login.microsoftonline.com/{tenantid}/v2.0 is from
// https://learn.microsoft.com/en-us/entra/identity-platform/access-tokens
// (validation section).
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.tenantOK(r) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 90002, "Tenant not found."))
		return
	}
	base := s.base()
	doc := map[string]any{
		"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
		"token_endpoint":                        base + "/oauth2/v2.0/token",
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt"},
		"jwks_uri":                              base + "/discovery/v2.0/keys",
		"userinfo_endpoint":                     "https://graph.microsoft.com/oidc/userinfo",
		"subject_types_supported":               []string{"pairwise"},
		"issuer":                                s.Issuer(),
		"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	s.mu.Lock()
	hook := s.discoveryHook
	s.mu.Unlock()
	if hook != nil {
		hook(doc)
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleKeys serves the signing keys.
//
// Source: the jwks_uri path /{tenant}/discovery/v2.0/keys is from the OpenID
// Connect page above. UNVERIFIED: the document's shape (see Key.jwk).
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if !s.tenantOK(r) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 90002, "Tenant not found."))
		return
	}
	s.mu.Lock()
	keys := make([]map[string]string, 0, len(s.published))
	for _, k := range s.published {
		keys = append(keys, k.jwk())
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// handleAuthorize plays the browser side of the sign-in: where a real tenant
// would show a login page, it signs in as the chosen person at once and
// redirects back with a code.
//
// Source: https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow
// for the parameters (client_id, response_type=code, redirect_uri, scope,
// response_mode, state, code_challenge, code_challenge_method) and the redirect
// (code, state; or error and error_description).
//
// UNVERIFIED: the page does not say whether state is returned on an error
// redirect; this fake returns it, as the OAuth specification requires.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.tenantOK(r) {
		pageError(w, http.StatusBadRequest, 90002, "Tenant not found.")
		return
	}
	q := r.URL.Query()

	// An unknown application or an unregistered redirect address is an error
	// page, never a redirect: the address is not trusted yet.
	if !strings.EqualFold(q.Get("client_id"), s.opts.ClientID) {
		pageError(w, http.StatusBadRequest, 700016, "Application not found in the directory.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if !s.redirectRegistered(redirectURI) {
		pageError(w, http.StatusBadRequest, 50011, "The redirect URI specified in the request does not match the redirect URIs configured for the application.")
		return
	}
	state := q.Get("state")
	fail := func(code, desc string) { redirectError(w, r, redirectURI, state, code, desc) }

	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "The response_type must be code for this fake.")
		return
	}
	if mode := q.Get("response_mode"); mode != "" && mode != "query" {
		fail("invalid_request", "Only response_mode=query is supported by this fake.")
		return
	}
	scope := q.Get("scope")
	if scope == "" {
		fail("invalid_request", "The scope parameter is required.")
		return
	}

	challenge, method := q.Get("code_challenge"), q.Get("code_challenge_method")
	switch {
	case challenge == "" && !s.opts.AllowNoPKCE:
		fail("invalid_request", "This fake requires PKCE.")
		return
	case challenge != "" && method != "S256":
		fail("invalid_request", "This fake accepts only code_challenge_method=S256.")
		return
	case challenge != "" && !validChallenge(challenge):
		fail("invalid_request", "The code_challenge is not a base64url SHA-256 value.")
		return
	}

	s.mu.Lock()
	person, ok := s.people[s.current]
	s.mu.Unlock()
	if !ok {
		fail("access_denied", "No one is signed in on the fake.")
		return
	}

	code := randomHex(24)
	s.mu.Lock()
	s.codes[code] = &grant{
		clientID: strings.ToLower(q.Get("client_id")), redirectURI: redirectURI, challenge: challenge,
		oid: person.OID, nonce: q.Get("nonce"), scope: scope, issued: s.now(),
	}
	s.mu.Unlock()

	loc, _ := url.Parse(redirectURI)
	lq := loc.Query()
	lq.Set("code", code)
	if state != "" {
		lq.Set("state", state)
	}
	loc.RawQuery = lq.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	loc, _ := url.Parse(redirectURI)
	lq := loc.Query()
	lq.Set("error", code)
	lq.Set("error_description", desc)
	if state != "" {
		lq.Set("state", state)
	}
	loc.RawQuery = lq.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

// validChallenge reports whether s looks like a base64url SHA-256: 43 characters
// that decode to 32 bytes.
func validChallenge(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// redirectRegistered applies Microsoft's matching rules: exact and
// case-sensitive, except that the port of a localhost address is ignored.
//
// Source: https://learn.microsoft.com/en-us/entra/identity-platform/reply-url
// ("Localhost exceptions" and "Redirect URI restrictions").
func (s *Server) redirectRegistered(given string) bool {
	g, err := url.Parse(given)
	if err != nil {
		return false
	}
	s.mu.Lock()
	registered := append([]string(nil), s.opts.RedirectURIs...)
	s.mu.Unlock()
	for _, reg := range registered {
		if reg == given {
			return true
		}
		rg, err := url.Parse(reg)
		if err != nil {
			continue
		}
		if rg.Scheme == "http" && g.Scheme == "http" && rg.Hostname() == "localhost" && g.Hostname() == "localhost" &&
			rg.Path == g.Path && rg.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

// errorBody is the token endpoint's error shape.
//
// Source: https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow
// and the client credentials page: error, error_description, error_codes,
// timestamp, trace_id, correlation_id.
func errorBody(errCode string, aadsts int, text string) map[string]any {
	trace, corr := randomGUID(), randomGUID()
	ts := time.Now().UTC().Format("2006-01-02 15:04:05Z")
	return map[string]any{
		"error":             errCode,
		"error_description": fmt.Sprintf("AADSTS%d: %s\r\nTrace ID: %s\r\nCorrelation ID: %s\r\nTimestamp: %s", aadsts, text, trace, corr, ts),
		"error_codes":       []int{aadsts},
		"timestamp":         ts,
		"trace_id":          trace,
		"correlation_id":    corr,
	}
}

// handleToken redeems an authorization code or issues an app token.
//
// Source for the request and response shapes:
// https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow
// and .../v2-oauth2-client-creds-grant-flow. Source for the AADSTS numbers:
// https://learn.microsoft.com/en-us/entra/identity-platform/reference-error-codes
// where each is cited below. That page also says applications should not depend
// on them, and the code under test does not.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	redirect := s.tokenRedirect
	s.mu.Unlock()
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if !s.tenantOK(r) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 90002, "Tenant not found."))
		return
	}

	// Only the body counts: a parameter in the query string (a secret in the
	// URL, say) is a missing parameter, which is how a real endpoint treats it.
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 900144, "The request body must contain the following parameter: 'grant_type'."))
		return
	}
	form := r.PostForm
	s.mu.Lock()
	s.tokenForms = append(s.tokenForms, cloneValues(form))
	s.mu.Unlock()

	for _, name := range []string{"grant_type", "client_id"} {
		if form.Get(name) == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 900144, fmt.Sprintf("The request body must contain the following parameter: '%s'.", name)))
			return
		}
	}
	// 700016: "The application wasn't found in the directory/tenant."
	if !strings.EqualFold(form.Get("client_id"), s.opts.ClientID) {
		writeJSON(w, http.StatusBadRequest, errorBody("unauthorized_client", 700016, fmt.Sprintf("Application with identifier '%s' was not found in the directory '%s'.", form.Get("client_id"), s.opts.TenantID)))
		return
	}
	// 7000218: "The request body must contain ... 'client_assertion' or
	// 'client_secret'." 7000215: "Invalid client secret is provided."
	secret := form.Get("client_secret")
	if secret == "" {
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid_client", 7000218, "The request body must contain the following parameter: 'client_assertion' or 'client_secret'."))
		return
	}
	if secret != s.opts.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid_client", 7000215, "Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, not the client secret ID, for a secret added to app '"+s.opts.ClientID+"'."))
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		s.redeemCode(w, form)
	case "client_credentials":
		s.appToken(w, form)
	default:
		// UNVERIFIED: the AADSTS number for an unsupported grant type is not on
		// the pages read.
		writeJSON(w, http.StatusBadRequest, errorBody("unsupported_grant_type", 70003, "The app requested an unsupported grant type."))
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// redeemCode exchanges an authorization code for tokens, strictly.
func (s *Server) redeemCode(w http.ResponseWriter, form url.Values) {
	for _, name := range []string{"code", "redirect_uri"} {
		if form.Get(name) == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 900144, fmt.Sprintf("The request body must contain the following parameter: '%s'.", name)))
			return
		}
	}

	s.mu.Lock()
	g, known := s.codes[form.Get("code")]
	var used bool
	var issued time.Time
	var person Person
	if known {
		used, issued = g.used, g.issued
		person = s.people[g.oid]
		// Consumed by the first redemption attempt that gets this far, success
		// or not, so a code that was tried and failed cannot be tried again.
		g.used = true
	}
	now := s.now()
	s.mu.Unlock()

	switch {
	case !known:
		// UNVERIFIED: the AADSTS number for a code that was never issued.
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 70000, "The provided authorization code is not valid."))
		return
	case used:
		// 54005: "OAuth2 Authorization code was already redeemed."
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 54005, "OAuth2 Authorization code was already redeemed, please retry with a new valid code or use an existing refresh token."))
		return
	case now.Sub(issued) > codeLifetime:
		// 50089: "Authentication failed due to flow token expired" - the closest
		// documented entry for an expired code.
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 50089, "Authentication failed due to flow token expired."))
		return
	case g.clientID != strings.ToLower(form.Get("client_id")):
		// UNVERIFIED: the AADSTS number for a code issued to another client.
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 70000, "The authorization code was issued to a different application."))
		return
	case g.redirectURI != form.Get("redirect_uri"):
		// 50011: "The reply address is missing, misconfigured, or doesn't match
		// reply addresses configured for the app." UNVERIFIED that the token
		// endpoint uses it for a mismatch between the two requests.
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 50011, "The redirect URI does not match the one the code was issued for."))
		return
	}

	if g.challenge != "" {
		verifier := form.Get("code_verifier")
		if verifier == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid_request", 900144, "The request body must contain the following parameter: 'code_verifier'."))
			return
		}
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			// 501481: "The Code_Verifier doesn't match the code_challenge supplied
			// in the authorization request."
			writeJSON(w, http.StatusBadRequest, errorBody("invalid_grant", 501481, "The Code_Verifier does not match the code_challenge supplied in the authorization request."))
			return
		}
	}

	resp := map[string]any{
		"token_type":   "Bearer",
		"scope":        g.scope,
		"expires_in":   3599,
		"access_token": "EwB" + randomHex(40),
	}
	if hasScope(g.scope, "openid") {
		resp["id_token"] = s.issueIDToken(person, g)
	}
	writeJSON(w, http.StatusOK, resp)
}

func hasScope(scope, want string) bool {
	for _, f := range strings.Fields(scope) {
		if f == want {
			return true
		}
	}
	return false
}

// issueIDToken builds, optionally lets the test alter, and signs the ID token.
//
// Source for the claims: https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference
// (aud, iss, iat, nbf, exp, name, nonce, oid, preferred_username, sub, tid, ver,
// uti; email only with the email scope; name, oid and preferred_username need
// the profile scope; sub is pairwise per application).
func (s *Server) issueIDToken(p Person, g *grant) string {
	now := s.now()
	claims := map[string]any{
		"aud": s.opts.ClientID,
		"iss": s.Issuer(),
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
		"tid": strings.ToLower(s.opts.TenantID),
		"ver": "2.0",
		"sub": pairwiseSub(s.opts.ClientID, p.OID),
		"uti": randomHex(11),
		"sid": randomGUID(),
	}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	if hasScope(g.scope, "profile") {
		claims["oid"] = p.OID
		claims["name"] = p.DisplayName
		claims["preferred_username"] = p.PreferredUsername
	}
	if hasScope(g.scope, "email") && p.Email != "" {
		claims["email"] = p.Email
	}

	spec := &IDTokenSpec{Header: map[string]any{}, Claims: claims}
	s.mu.Lock()
	hook := s.idHook
	s.idHook = nil
	current := s.published[len(s.published)-1]
	s.mu.Unlock()
	if hook != nil {
		hook(spec)
	}
	return s.record(signJWT(spec, current))
}

// pairwiseSub is a stand-in for the subject Entra derives per application.
func pairwiseSub(clientID, oid string) string {
	sum := sha256.Sum256([]byte(clientID + "/" + oid))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// appToken issues a client credentials token.
//
// The scope must be the Graph resource plus ".default", as the client
// credentials page requires; any other resource is invalid_scope (70011,
// "InvalidScope - The scope requested by the app is invalid"). The token is a
// JWT signed with a key that is not published, as a real Graph token cannot be
// checked with the tenant's keys: Microsoft says not to validate or read tokens
// for an API you do not own.
//
// UNVERIFIED: the claims of a real Graph app-only token (the v1.0-style issuer,
// the appid claim, roles as an array of permission names) are from general
// knowledge, not a page that was read. The probe's app-token step records the
// real claim names.
func (s *Server) appToken(w http.ResponseWriter, form url.Values) {
	if form.Get("scope") != "https://graph.microsoft.com/.default" {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid_scope", 70011, "The provided value for the input parameter 'scope' is not valid. The scope "+form.Get("scope")+" is not valid."))
		return
	}
	now := s.now()
	tid := strings.ToLower(s.opts.TenantID)
	claims := map[string]any{
		"aud":   graphAppID,
		"iss":   "https://sts.windows.net/" + tid + "/",
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"tid":   tid,
		"appid": s.opts.ClientID,
		"oid":   randomGUID(),
		"ver":   "1.0",
		"roles": s.opts.GraphRoles,
	}
	token := s.record(signJWT(&IDTokenSpec{Claims: claims, SignWith: s.NewUnpublishedKey()}, nil))
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3599,
		"access_token": token,
	})
}
