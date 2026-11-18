// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
)

// signInScope asks for an ID token (openid) carrying the person's object ID and
// name (profile - Microsoft documents that oid and name need it). It does not
// ask for email: the address comes from Microsoft Graph, not the token
// (docs/ENTRA_DESIGN.md decision D5), and it does not ask for offline_access, so
// no refresh token is ever issued.
const signInScope = "openid profile"

// Client is a configured, discovered sign-in client for one tenant. It is safe
// for concurrent use.
type Client struct {
	cfg      Config
	hc       *http.Client
	verifier *oidc.IDTokenVerifier
	authURL  string
	tokenURL string
}

// New reads the tenant's discovery document and prepares a Client. It needs the
// network, and fails with a *DiscoveryError if the tenant's metadata cannot be
// read or does not describe this tenant.
func New(ctx context.Context, cfg Config) (*Client, error) {
	cfg, err := cfg.Normalized()
	if err != nil {
		return nil, err
	}
	hc := cfg.httpClient()

	// go-oidc keeps the context it is given for the whole life of the provider's
	// key set, using it for every later key fetch, so it must not be one that is
	// cancelled when the caller returns. The client's own timeout bounds the
	// calls instead.
	oc := oidc.ClientContext(context.WithoutCancel(ctx), hc)
	provider, err := oidc.NewProvider(oc, cfg.IssuerURL())
	if err != nil {
		return nil, discoveryError(err)
	}

	ep := provider.Endpoint()
	for _, endpoint := range []string{ep.AuthURL, ep.TokenURL} {
		if !sameOrigin(cfg.AuthorityURL, endpoint) {
			// The client secret is sent to the token endpoint, so the document
			// is not trusted to point it somewhere else. Microsoft's own
			// examples keep all of a tenant's endpoints on its sign-in host.
			return nil, &DiscoveryError{Kind: "foreign_endpoint"}
		}
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID:             cfg.ClientID,
		SupportedSigningAlgs: []string{oidc.RS256},
		// go-oidc applies no tolerance to exp and five minutes to nbf. Shifting
		// its clock back by ClockSkew gives exp the small tolerance the design
		// asks for and leaves nbf's wider one in place.
		Now: func() time.Time { return cfg.now().Add(-ClockSkew) },
	})
	return &Client{cfg: cfg, hc: hc, verifier: verifier, authURL: ep.AuthURL, tokenURL: ep.TokenURL}, nil
}

var statusLineRE = regexp.MustCompile(`^\d{3} `)

func discoveryError(err error) error {
	var mismatch *oidc.IssuerMismatchError
	switch {
	case errors.As(err, &mismatch):
		return &DiscoveryError{Kind: "issuer_mismatch", err: err}
	case networkKind(err) != "other":
		return &DiscoveryError{Kind: "unreachable", err: err}
	case statusLineRE.MatchString(err.Error()):
		return &DiscoveryError{Kind: "unexpected_status", err: err}
	}
	return &DiscoveryError{Kind: "bad_document", err: err}
}

func sameOrigin(authority, endpoint string) bool {
	a, errA := url.Parse(authority)
	e, errE := url.Parse(endpoint)
	return errA == nil && errE == nil && a.Scheme == e.Scheme && a.Host == e.Host
}

// Begin starts a sign-in. It returns the address to send the person to and the
// state to keep (in a SealFlow cookie) until they come back. returnPath is kept
// with it and not interpreted here.
func (c *Client) Begin(returnPath string) (string, Flow, error) {
	state, err := randomToken(32)
	if err != nil {
		return "", Flow{}, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return "", Flow{}, err
	}
	verifier, err := randomToken(32)
	if err != nil {
		return "", Flow{}, err
	}
	f := Flow{State: state, Nonce: nonce, Verifier: verifier, ReturnPath: returnPath, Expires: c.cfg.now().Add(FlowTTL)}

	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("response_mode", "query")
	q.Set("scope", signInScope)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challengeS256(verifier))
	q.Set("code_challenge_method", "S256")
	return c.authURL + "?" + q.Encode(), f, nil
}

// Identity is who Entra says the person is. Nothing in it is a credential.
type Identity struct {
	// ObjectID is the person's object ID in the tenant, lower case: the key
	// that identifies them to Sparky. Never the subject claim (different for
	// the same person in every application) and never an address or user name
	// (they change and are not proof of identity).
	ObjectID string

	// TenantID is the tenant the token says it was issued for; it has already
	// been checked against the configured one.
	TenantID string

	// DisplayName is the person's name, cleaned of control characters and
	// limited in length. It is for display only.
	DisplayName string

	// Diagnostics describes the token without containing anything from it.
	Diagnostics Diagnostics
}

// Diagnostics is what the probe reports about an ID token. It holds names,
// types and numbers - never a claim's value - so it is safe by construction to
// put in a report. The claim names themselves are not: a custom claim's name can
// contain an application ID, so a report must filter them.
type Diagnostics struct {
	// SigningAlg is the token's signature algorithm; always RS256 here, since
	// anything else is refused.
	SigningAlg string

	// HeaderClaims and ClaimTypes map each JWT header and payload claim name to
	// the JSON type of its value (string, number, boolean, array, object, null).
	HeaderClaims map[string]string
	ClaimTypes   map[string]string

	// TokenVersion is the "ver" claim when it is 1.0 or 2.0, "other" for any
	// other value and empty when the claim is absent.
	TokenVersion string

	// Lifetime is exp minus iat.
	Lifetime time.Duration

	// AudienceCount is how many audiences the token names; one is required.
	AudienceCount int
}

// Complete finishes a sign-in. f is the state saved by Begin, query the
// parameters Entra put on the redirect back. It checks, in this order: that the
// saved state is current; that the response carries the same state (before
// anything else, so a response for some other sign-in does nothing); whether
// Entra reported an error; then exchanges the code, verifies the ID token and
// returns who signed in.
func (c *Client) Complete(ctx context.Context, f Flow, query url.Values) (*Identity, error) {
	if f.State == "" || f.Nonce == "" || f.Verifier == "" || !c.cfg.now().Before(f.Expires) {
		return nil, ErrFlowInvalid
	}
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(f.State)) != 1 {
		return nil, ErrStateMismatch
	}
	if e := query.Get("error"); e != "" {
		return nil, &AuthorizationError{Code: cleanCode(e), Description: truncate(query.Get("error_description"), 1000)}
	}
	code := query.Get("code")
	if code == "" || len(code) > 4096 {
		return nil, ErrNoCode
	}

	rawIDToken, err := c.exchangeCode(ctx, code, f.Verifier)
	if err != nil {
		return nil, err
	}
	return c.validate(ctx, rawIDToken, f.Nonce)
}

// exchangeCode redeems the authorization code. Only the ID token is kept: the
// access token that comes with it is for a resource this server does not use,
// and is discarded unread.
func (c *Client) exchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret.Reveal())
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	form.Set("code_verifier", verifier)

	status, body, err := c.postForm(ctx, form)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", tokenFailure(status, body)
	}
	var resp struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", &ProtocolError{Reason: "malformed_body", Status: status}
	}
	if resp.IDToken == "" {
		return "", &ProtocolError{Reason: "missing_field", Status: status}
	}
	return resp.IDToken, nil
}

// AppToken gets a token for the app itself (the client credentials flow), with
// the application permissions granted to it for the resource in scope - for
// Microsoft Graph, GraphScope. It is not validated: a token for a resource this
// server does not own is that resource's to read, not ours.
func (c *Client) AppToken(ctx context.Context, scope string) (AccessToken, error) {
	if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
		return AccessToken{}, errors.New("entra: the scope must be a single resource scope")
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret.Reveal())
	form.Set("scope", scope)

	status, body, err := c.postForm(ctx, form)
	if err != nil {
		return AccessToken{}, err
	}
	if status != http.StatusOK {
		return AccessToken{}, tokenFailure(status, body)
	}
	var resp struct {
		AccessToken string      `json:"access_token"`
		TokenType   string      `json:"token_type"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return AccessToken{}, &ProtocolError{Reason: "malformed_body", Status: status}
	}
	seconds, err := resp.ExpiresIn.Int64()
	if resp.AccessToken == "" || !strings.EqualFold(resp.TokenType, "Bearer") || err != nil || seconds <= 0 {
		return AccessToken{}, &ProtocolError{Reason: "missing_field", Status: status}
	}
	return AccessToken{value: Secret(resp.AccessToken), ExpiresIn: time.Duration(seconds) * time.Second}, nil
}

// postForm sends a form to the token endpoint and returns the status and body.
// A redirect or an oversized answer is a protocol error rather than something
// followed or buffered.
func (c *Client) postForm(ctx context.Context, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("entra: build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("entra: token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, nil, &ProtocolError{Reason: "redirect", Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("entra: read the token response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return 0, nil, &ProtocolError{Reason: "too_large", Status: resp.StatusCode}
	}
	return resp.StatusCode, body, nil
}

// tokenFailure turns a non-200 token response into an error. The OAuth error
// value is reduced to a safe token; Entra's description, which can name the
// tenant and the application, is kept only in the Description field.
func tokenFailure(status int, body []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
		Codes       []int  `json:"error_codes"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return &ProtocolError{Reason: "unexpected_status", Status: status}
	}
	if len(e.Codes) > 8 {
		e.Codes = e.Codes[:8]
	}
	return &TokenError{Status: status, Code: cleanCode(e.Error), Codes: e.Codes, Description: truncate(e.Description, 1000)}
}

// validate verifies the ID token and extracts the identity. go-oidc checks the
// signature (RS256 only, key chosen by kid, fetched again once if the kid is
// unknown), the issuer, that our client ID is among the audiences, and exp and
// nbf. This adds what it does not: exactly one audience, the nonce, and that
// the tenant claim is the configured tenant.
func (c *Client) validate(ctx context.Context, raw, nonce string) (*Identity, error) {
	tok, err := c.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, classifyVerify(err)
	}
	if len(tok.Audience) != 1 {
		return nil, &ValidationError{Reason: "audience"}
	}
	if subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(nonce)) != 1 {
		return nil, &ValidationError{Reason: "nonce"}
	}

	var claims map[string]json.RawMessage
	if err := tok.Claims(&claims); err != nil {
		return nil, &ValidationError{Reason: "malformed", err: err}
	}
	str := func(name string) string {
		var s string
		if json.Unmarshal(claims[name], &s) != nil {
			return ""
		}
		return s
	}

	// The issuer already names the configured tenant (go-oidc compared it), so
	// the tenant claim is a second, independent statement of the same thing.
	if !strings.EqualFold(str("tid"), c.cfg.TenantID) {
		return nil, &ValidationError{Reason: "tenant"}
	}
	oid := strings.ToLower(str("oid"))
	if !IsGUID(oid) {
		return nil, &ValidationError{Reason: "subject"}
	}

	name := CleanDisplayName(str("name"))
	if name == "" {
		name = CleanDisplayName(str("preferred_username"))
	}

	diag := Diagnostics{
		SigningAlg:    "RS256",
		HeaderClaims:  jwtHeaderTypes(raw),
		ClaimTypes:    map[string]string{},
		AudienceCount: len(tok.Audience),
		Lifetime:      tok.Expiry.Sub(tok.IssuedAt),
	}
	for k, v := range claims {
		diag.ClaimTypes[k] = jsonType(v)
	}
	switch v := str("ver"); v {
	case "":
	case "1.0", "2.0":
		diag.TokenVersion = v
	default:
		diag.TokenVersion = "other"
	}
	return &Identity{ObjectID: oid, TenantID: strings.ToLower(str("tid")), DisplayName: name, Diagnostics: diag}, nil
}

// classifyVerify maps a go-oidc error to a fixed reason. It matches on the text
// of go-oidc's errors because it does not export types for most of them; the
// tests pin each message so a library upgrade that changes one fails loudly
// instead of silently turning a specific reason into "malformed".
func classifyVerify(err error) *ValidationError {
	var expired *oidc.TokenExpiredError
	msg := err.Error()
	reason := "malformed"
	switch {
	case errors.As(err, &expired):
		reason = "expired"
	case strings.Contains(msg, "before the nbf"):
		reason = "not_yet_valid"
	case strings.HasPrefix(msg, "oidc: id token issued by a different provider"):
		reason = "issuer"
	case strings.HasPrefix(msg, "oidc: expected audience"):
		reason = "audience"
	case strings.HasPrefix(msg, "failed to verify signature"):
		reason = "signature"
	case strings.Contains(msg, "unexpected signature algorithm"):
		reason = "algorithm"
	}
	return &ValidationError{Reason: reason, err: err}
}

// jwtHeaderTypes returns the JWT header's claim names and value types, or an
// empty map if the token is not three dot-separated parts with a JSON header.
func jwtHeaderTypes(raw string) map[string]string {
	out := map[string]string{}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return out
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return out
	}
	var h map[string]json.RawMessage
	if json.Unmarshal(b, &h) != nil {
		return out
	}
	for k, v := range h {
		out[k] = jsonType(v)
	}
	return out
}

// jsonType names the JSON type of a raw value.
func jsonType(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "":
		return "null"
	case s[0] == '"':
		return "string"
	case s[0] == '{':
		return "object"
	case s[0] == '[':
		return "array"
	case s == "true" || s == "false":
		return "boolean"
	case s == "null":
		return "null"
	}
	return "number"
}

// maxDisplayNameRunes caps a display name; Entra allows far longer, and the
// name is only shown.
const maxDisplayNameRunes = 200

// CleanDisplayName makes a directory-supplied name safe to store and show:
// control and format characters removed, runs of whitespace collapsed, length
// limited. HTML escaping is the template layer's job, as for every other
// value.
func CleanDisplayName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if utf8.RuneCountInString(out) > maxDisplayNameRunes {
		out = string([]rune(out)[:maxDisplayNameRunes])
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
