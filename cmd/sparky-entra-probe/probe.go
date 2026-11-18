// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra"
)

const (
	callbackPath = "/sparky-probe/callback"

	// graphWellKnownAppID is Microsoft Graph's application ID, the audience of an
	// app-only token for it. It is public.
	graphWellKnownAppID = "00000003-0000-0000-c000-000000000000"
)

// probeConfig is what a run needs beyond the Entra credentials.
type probeConfig struct {
	Entra         entra.Config
	SignIn        bool
	ListenPort    int
	SignInTimeout time.Duration
}

// probe runs the checks and records what it sees in a Report. It never prints an
// error message into the report: every failure is described by entra.ErrorKind
// or by a fixed phrase.
type probe struct {
	cfg     probeConfig
	report  *Report
	console io.Writer
	hc      *http.Client

	// open lets a test play the browser. Nil means the address is only printed.
	open func(authURL string) error

	jwksURI string
}

func newProbe(cfg probeConfig, console io.Writer) *probe {
	ec := cfg.Entra
	p := &probe{
		cfg:     cfg,
		report:  NewReport(ec.TenantID, ec.ClientID, ec.ClientSecret.Reveal()),
		console: console,
		hc: &http.Client{
			Timeout: 20 * time.Second,
			// A redirect is a finding, not something to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return p
}

// say writes a line to the console, which is not part of the report. The one
// thing printed there that the report must never contain is the sign-in address.
func (p *probe) say(format string, args ...any) {
	fmt.Fprintf(p.console, format+"\n", args...)
}

// Run performs every step. A step that cannot run because an earlier one failed
// is skipped, with the reason.
func (p *probe) Run(ctx context.Context) {
	p.stepDiscovery(ctx)
	p.stepKeys(ctx)
	p.stepAppToken(ctx)
	p.stepGraph()
	p.stepSignIn(ctx)
}

type rawResponse struct {
	status      int
	contentType string
	body        []byte
}

const maxProbeBody = 1 << 20

func (p *probe) get(ctx context.Context, rawURL string) (*rawResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, err
	}
	return &rawResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}, nil
}

func mediaType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt == "" {
		return "(none)"
	}
	return mt
}

// sameOrigin reports whether raw is on the same scheme and host as the authority.
func (p *probe) sameOrigin(raw string) bool {
	a, errA := url.Parse(p.cfg.Entra.AuthorityURL)
	u, errU := url.Parse(raw)
	return errA == nil && errU == nil && a.Scheme == u.Scheme && a.Host == u.Host
}

func pathHasSuffix(raw, suffix string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(u.Path, suffix)
}

// stepDiscovery fetches the discovery document and records its shape: which
// members it has, whether its endpoints are where Sparky expects them, and
// whether the sign-in client accepts it.
func (p *probe) stepDiscovery(ctx context.Context) {
	s := p.report.Begin("Discovery document")
	defer p.progress(s)

	resp, err := p.get(ctx, p.cfg.Entra.IssuerURL()+"/.well-known/openid-configuration")
	if err != nil {
		s.Fail(entra.ErrorKind(err))
		return
	}
	s.Count("http status", resp.status)
	s.Fact("content type", mediaType(resp.contentType))
	var doc map[string]any
	if resp.status != http.StatusOK || json.Unmarshal(resp.body, &doc) != nil {
		s.Fail(fmt.Sprintf("unexpected answer (HTTP %d)", resp.status))
		return
	}

	s.Shape("members", typesOf(doc), discoveryVocab)
	str := func(k string) string { v, _ := doc[k].(string); return v }

	s.Flag("issuer is the tenant's v2.0 issuer", str("issuer") == p.cfg.Entra.IssuerURL())
	for _, member := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		s.Flag(member+" is on the authority's own origin", p.sameOrigin(str(member)))
	}
	s.Flag("authorization_endpoint path ends /oauth2/v2.0/authorize", pathHasSuffix(str("authorization_endpoint"), "/oauth2/v2.0/authorize"))
	s.Flag("token_endpoint path ends /oauth2/v2.0/token", pathHasSuffix(str("token_endpoint"), "/oauth2/v2.0/token"))
	s.Flag("jwks_uri path ends /discovery/v2.0/keys", pathHasSuffix(str("jwks_uri"), "/discovery/v2.0/keys"))

	for _, member := range []string{
		"id_token_signing_alg_values_supported", "token_endpoint_auth_methods_supported", "response_types_supported",
		"response_modes_supported", "subject_types_supported", "grant_types_supported", "code_challenge_methods_supported",
	} {
		if list, ok := doc[member].([]any); ok {
			var values []string
			for _, v := range list {
				if sv, ok := v.(string); ok {
					values = append(values, sv)
				}
			}
			s.Set(member, values)
		}
	}

	if p.sameOrigin(str("jwks_uri")) {
		p.jwksURI = str("jwks_uri")
	}

	_, err = entra.New(ctx, p.cfg.Entra)
	s.Flag("accepted by the sign-in client", err == nil)
	if err != nil {
		s.Fail(entra.ErrorKind(err))
		return
	}
	s.Pass()
}

// stepKeys fetches the signing keys and records the shape of the set, never a
// key or a key ID.
func (p *probe) stepKeys(ctx context.Context) {
	s := p.report.Begin("Signing keys")
	defer p.progress(s)

	if p.jwksURI == "" {
		s.Skip("no key set address on the authority's origin")
		return
	}
	resp, err := p.get(ctx, p.jwksURI)
	if err != nil {
		s.Fail(entra.ErrorKind(err))
		return
	}
	s.Count("http status", resp.status)
	s.Fact("content type", mediaType(resp.contentType))
	var set map[string]any
	if resp.status != http.StatusOK || json.Unmarshal(resp.body, &set) != nil {
		s.Fail(fmt.Sprintf("unexpected answer (HTTP %d)", resp.status))
		return
	}
	s.Shape("members", typesOf(set), vocabulary("", "keys"))
	keys, _ := set["keys"].([]any)
	s.Count("keys", len(keys))
	if len(keys) == 0 {
		s.Fail("the key set is empty")
		return
	}

	present := map[string]int{}
	kids := map[string]bool{}
	var ktys, uses, algs, sizes []string
	allKid, allRSA, hasIssuer, templated, standardExp := true, true, false, false, true
	for _, k := range keys {
		m, _ := k.(map[string]any)
		for name := range m {
			present[name]++
		}
		kid, _ := m["kid"].(string)
		if kid == "" {
			allKid = false
		}
		kids[kid] = true
		kty, _ := m["kty"].(string)
		ktys = append(ktys, kty)
		allRSA = allRSA && kty == "RSA"
		if use, ok := m["use"].(string); ok {
			uses = append(uses, use)
		}
		if alg, ok := m["alg"].(string); ok {
			algs = append(algs, alg)
		}
		if n, ok := m["n"].(string); ok {
			if b, err := base64.RawURLEncoding.DecodeString(n); err == nil {
				sizes = append(sizes, strconv.Itoa(new(big.Int).SetBytes(b).BitLen()))
			}
		}
		if e, _ := m["e"].(string); e != "AQAB" {
			standardExp = false
		}
		if iss, ok := m["issuer"].(string); ok {
			hasIssuer = true
			templated = templated || strings.Contains(iss, "{tenantid}")
		}
	}
	var every, some []string
	for name, n := range present {
		if n == len(keys) {
			every = append(every, name)
		} else {
			some = append(some, name)
		}
	}
	s.Names("members in every key", every, jwkVocab)
	if len(some) > 0 {
		s.Names("members in some keys", some, jwkVocab)
	}
	s.Set("key types", ktys)
	s.Set("key uses", uses)
	s.Set("key algorithms", algs)
	s.Set("modulus sizes (bits)", sizes)
	s.Flag("every key has a key ID", allKid)
	s.Flag("key IDs are unique", len(kids) == len(keys))
	s.Flag("public exponent is 65537 in every key", standardExp)
	s.Flag("a key lists an issuer member", hasIssuer)
	s.Flag("an issuer member is templated with {tenantid}", templated)

	if !allRSA || !allKid {
		s.Fail("a key is not an RSA key with a key ID")
		return
	}
	s.Pass()
}

var roleRE = regexp.MustCompile(`^[A-Za-z]+(\.[A-Za-z]+){1,3}$`)

// stepAppToken gets an app-only token for Microsoft Graph (the client credentials
// flow) and records what its claims look like. The token is read without being
// validated: Microsoft says not to validate tokens for an API one does not own,
// and only the shape is wanted.
func (p *probe) stepAppToken(ctx context.Context) {
	s := p.report.Begin("App token for Microsoft Graph")
	defer p.progress(s)

	c, err := entra.New(ctx, p.cfg.Entra)
	if err != nil {
		s.Skip("the sign-in client could not be set up (see step 1)")
		return
	}
	tok, err := c.AppToken(ctx, entra.GraphScope)
	if err != nil {
		var te *entra.TokenError
		if errors.As(err, &te) {
			s.Count("http status", te.Status)
			codes := make([]string, 0, len(te.Codes))
			for _, c := range te.Codes {
				codes = append(codes, strconv.Itoa(c))
			}
			s.Set("AADSTS error numbers", codes)
		}
		s.Fail(entra.ErrorKind(err))
		return
	}
	raw := tok.Bearer()
	p.report.Watch(raw)
	s.Count("lifetime in minutes (as announced)", int(tok.ExpiresIn.Round(time.Minute)/time.Minute))

	header, payload, parts, ok := decodeJWT(raw)
	s.Flag("token is a JWT", ok)
	if !ok {
		s.Pass() // an opaque token is allowed; there is just nothing more to learn
		return
	}
	p.report.Watch(parts...)
	s.Shape("header", typesOf(header), headerVocab)
	s.Shape("claims", typesOf(payload), claimVocab)

	str := func(k string) string { v, _ := payload[k].(string); return v }
	ec := p.cfg.Entra
	switch ver := str("ver"); ver {
	case "1.0", "2.0":
		s.Fact("token version", ver)
	case "":
		s.Fact("token version", "(absent)")
	default:
		s.Fact("token version", "other")
	}
	s.Fact("issuer form", issuerClass(str("iss"), ec))
	switch aud := strings.TrimSuffix(strings.ToLower(str("aud")), "/"); aud {
	case graphWellKnownAppID:
		s.Fact("audience", "graph application id")
	case "https://graph.microsoft.com":
		s.Fact("audience", "graph url")
	default:
		s.Fact("audience", "other")
	}
	s.Flag("tid claim is the configured tenant", strings.EqualFold(str("tid"), ec.TenantID))
	for _, name := range []string{"appid", "azp"} {
		switch v, present := payload[name].(string); {
		case !present:
		case strings.EqualFold(v, ec.ClientID):
			s.Fact(name+" claim", "is the client ID")
		default:
			s.Fact(name+" claim", "differs from the client ID")
		}
	}
	if idtyp := str("idtyp"); idtyp == "app" || idtyp == "user" {
		s.Fact("idtyp claim", idtyp)
	}
	if exp, iat := number(payload["exp"]), number(payload["iat"]); exp > 0 && iat > 0 {
		s.Count("lifetime in minutes (exp minus iat)", int((exp-iat)/60))
	}

	var roles []string
	other := 0
	if list, ok := payload["roles"].([]any); ok {
		for _, r := range list {
			if name, _ := r.(string); roleRE.MatchString(name) && len(name) <= 60 {
				roles = append(roles, name)
			} else {
				other++
			}
		}
	}
	s.Set("application permissions (roles claim)", roles)
	if other > 0 {
		s.Count("roles that are not shown", other)
	}
	s.Pass()
}

func number(v any) float64 { f, _ := v.(float64); return f }

func issuerClass(iss string, c entra.Config) string {
	switch {
	case strings.EqualFold(iss, c.IssuerURL()):
		return "authority, tenant, v2.0"
	case strings.EqualFold(iss, "https://sts.windows.net/"+c.TenantID+"/"):
		return "sts.windows.net, tenant"
	}
	return "other"
}

// decodeJWT splits a compact JWT and decodes its header and payload without
// checking the signature.
func decodeJWT(raw string) (header, payload map[string]any, parts []string, ok bool) {
	parts = strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, nil, nil, false
	}
	for i, dst := range []*map[string]any{&header, &payload} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, dst) != nil {
			return nil, nil, nil, false
		}
	}
	return header, payload, parts, true
}

// stepGraph is a placeholder: the Graph calls come with the Graph client.
func (p *probe) stepGraph() {
	s := p.report.Begin("Microsoft Graph calls")
	s.Skip("not part of this version of the probe")
	p.progress(s)
}

// stepSignIn signs a person in through their own browser, using the same code
// the server will use, and records the shape of the ID token.
func (p *probe) stepSignIn(ctx context.Context) {
	s := p.report.Begin("Interactive sign-in")
	defer p.progress(s)

	if !p.cfg.SignIn {
		s.Skip("not requested")
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.cfg.ListenPort))
	if err != nil {
		s.Fail("could not listen on the loopback address")
		return
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := p.cfg.Entra
	cfg.RedirectURL = fmt.Sprintf("http://localhost:%d%s", port, callbackPath)
	client, err := entra.New(ctx, cfg)
	if err != nil {
		s.Fail(entra.ErrorKind(err))
		return
	}
	authURL, flow, err := client.Begin("")
	if err != nil {
		s.Fail("could not start the sign-in")
		return
	}
	p.report.Watch(authURL, flow.State, flow.Nonce, flow.Verifier)

	type outcome struct {
		id  *entra.Identity
		err error
	}
	done := make(chan outcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.report.Watch(q.Get("code"), q.Get("state"))
		cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		id, err := client.Complete(cctx, flow, q)
		if errors.Is(err, entra.ErrStateMismatch) {
			// Not a response to this sign-in: some other request to a local
			// port. It must not end the run.
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		select {
		case done <- outcome{id, err}:
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Sign-in finished. You can close this window and return to the terminal.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	p.say("")
	p.say("Open this address in a browser and sign in with a test account.")
	p.say("It contains your tenant ID and application ID, so it is NOT part of the report: do not share it.")
	p.say("")
	p.say("%s", authURL)
	p.say("")
	p.say("Waiting up to %s for the sign-in ...", p.cfg.SignInTimeout.Round(time.Second))
	if p.open != nil {
		if err := p.open(authURL); err != nil {
			s.Fail("could not open the browser")
			return
		}
	}

	var res outcome
	select {
	case res = <-done:
	case <-time.After(p.cfg.SignInTimeout):
		s.Fail("timed out waiting for the sign-in")
		return
	case <-ctx.Done():
		s.Fail("cancelled")
		return
	}

	if res.err != nil {
		var te *entra.TokenError
		if errors.As(res.err, &te) {
			s.Count("token endpoint http status", te.Status)
			codes := make([]string, 0, len(te.Codes))
			for _, c := range te.Codes {
				codes = append(codes, strconv.Itoa(c))
			}
			s.Set("AADSTS error numbers", codes)
		}
		s.Fail(entra.ErrorKind(res.err))
		return
	}

	id := res.id
	p.report.Watch(id.ObjectID, id.DisplayName, id.TenantID)
	d := id.Diagnostics
	s.Fact("signed-in person", p.report.Pseudonyms().Label("user", id.ObjectID))
	s.Fact("tenant in the token", p.report.Pseudonyms().Label("tenant", id.TenantID))
	s.Flag("tenant in the token is the configured tenant", strings.EqualFold(id.TenantID, p.cfg.Entra.TenantID))
	s.Shape("ID token header", d.HeaderClaims, headerVocab)
	s.Shape("ID token claims", d.ClaimTypes, claimVocab)
	s.Fact("signing algorithm", d.SigningAlg)
	if d.TokenVersion == "" {
		s.Fact("token version", "(absent)")
	} else {
		s.Fact("token version", d.TokenVersion)
	}
	s.Count("audiences", d.AudienceCount)
	s.Count("lifetime in minutes", int(d.Lifetime/time.Minute))
	s.Flag("a display name is available", id.DisplayName != "")
	_, hasEmail := d.ClaimTypes["email"]
	_, hasUPN := d.ClaimTypes["preferred_username"]
	s.Flag("an email claim is present (Sparky does not use it)", hasEmail)
	s.Flag("a preferred_username claim is present", hasUPN)
	var groupClaims []string
	for _, name := range []string{"groups", "hasgroups", "_claim_names", "_claim_sources"} {
		if _, ok := d.ClaimTypes[name]; ok {
			groupClaims = append(groupClaims, name)
		}
	}
	s.Set("group-related claims present (Sparky ignores them)", groupClaims)
	s.Pass()
}

// progress prints a step's outcome to the console as it finishes.
func (p *probe) progress(s *Step) {
	outcome := s.Outcome
	if outcome == "" {
		outcome = "fail"
	}
	p.say("%s: %s", s.Name, outcome)
}
