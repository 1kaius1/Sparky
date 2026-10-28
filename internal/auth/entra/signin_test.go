// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra/fakeentra"
)

func TestBegin_BuildsAuthorizeAddress(t *testing.T) {
	e := newEnv(t)
	authURL, f, err := e.client.Begin("/profiles")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := e.fake.URL() + "/" + testTenant + "/oauth2/v2.0/authorize"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Errorf("authorize address = %s, want %s", u.Scheme+"://"+u.Host+u.Path, want)
	}
	q := u.Query()
	for name, want := range map[string]string{
		"client_id":             testClient,
		"response_type":         "code",
		"redirect_uri":          testRedirect,
		"response_mode":         "query",
		"scope":                 "openid profile",
		"state":                 f.State,
		"nonce":                 f.Nonce,
		"code_challenge":        challengeS256(f.Verifier),
		"code_challenge_method": "S256",
	} {
		if got := q.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(authURL, f.Verifier) {
		t.Error("the PKCE verifier is in the address; only its hash may be")
	}
	if f.ReturnPath != "/profiles" {
		t.Errorf("ReturnPath = %q", f.ReturnPath)
	}
	if want := e.cfg.Now().Add(FlowTTL); f.Expires.Sub(want) > time.Second || want.Sub(f.Expires) > time.Second {
		t.Errorf("Expires = %v, want about %v", f.Expires, want)
	}
	// RFC 7636: a verifier is 43 to 128 unreserved characters.
	if n := len(f.Verifier); n < 43 || n > 128 {
		t.Errorf("verifier length = %d", n)
	}
}

func TestBegin_ValuesAreFreshAndDistinct(t *testing.T) {
	e := newEnv(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		_, f, err := e.client.Begin("")
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{f.State, f.Nonce, f.Verifier} {
			if seen[v] {
				t.Fatalf("a state, nonce or verifier repeated: %q", v)
			}
			seen[v] = true
		}
		if f.State == f.Nonce || f.Nonce == f.Verifier || f.State == f.Verifier {
			t.Fatal("state, nonce and verifier must be independent values")
		}
	}
}

func TestSignIn_Succeeds(t *testing.T) {
	e := newEnv(t)
	id, err := e.signIn()
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if id.ObjectID != testOID || id.TenantID != testTenant || id.DisplayName != testName {
		t.Errorf("identity = %+v", id)
	}

	d := id.Diagnostics
	if d.SigningAlg != "RS256" || d.TokenVersion != "2.0" || d.AudienceCount != 1 || d.Lifetime != time.Hour {
		t.Errorf("diagnostics = %+v", d)
	}
	for _, name := range []string{"aud", "iss", "exp", "nbf", "iat", "oid", "tid", "nonce", "name", "preferred_username", "sub", "ver"} {
		if _, ok := d.ClaimTypes[name]; !ok {
			t.Errorf("claim %q missing from the diagnostics", name)
		}
	}
	if d.ClaimTypes["exp"] != "number" || d.ClaimTypes["oid"] != "string" {
		t.Errorf("claim types = %v", d.ClaimTypes)
	}
	if _, ok := d.ClaimTypes["email"]; ok {
		t.Error("an email claim was issued although the email scope was not requested (decision D5)")
	}
	if d.HeaderClaims["alg"] != "string" || d.HeaderClaims["kid"] != "string" {
		t.Errorf("header claims = %v", d.HeaderClaims)
	}

	// Diagnostics hold names and types, never values.
	enc, _ := json.Marshal(d)
	for _, v := range []string{testOID, testName, testUPN, testTenant, testClient} {
		if strings.Contains(string(enc), v) {
			t.Errorf("diagnostics contain the value %q", v)
		}
	}
}

func TestSignIn_SendsWhatMicrosoftDocuments(t *testing.T) {
	e := newEnv(t)
	f, q := e.begin()
	if _, err := e.client.Complete(context.Background(), f, q); err != nil {
		t.Fatal(err)
	}
	forms := e.fake.TokenForms()
	if len(forms) != 1 {
		t.Fatalf("token requests = %d, want 1", len(forms))
	}
	form := forms[0]
	for name, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     testClient,
		"client_secret": testSecret,
		"code":          q.Get("code"),
		"redirect_uri":  testRedirect,
		"code_verifier": f.Verifier,
	} {
		if got := form.Get(name); got != want {
			t.Errorf("token request %s = %q, want %q", name, got, want)
		}
	}
}

func TestSignIn_IgnoresEmailAndGroupsClaims(t *testing.T) {
	e := newEnv(t)
	e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) {
		s.Claims["email"] = "someone.else@example.test"
		s.Claims["groups"] = []string{"g1", "g2"}
		s.Claims["hasgroups"] = true
	})
	id, err := e.signIn()
	if err != nil {
		t.Fatal(err)
	}
	if id.ObjectID != testOID {
		t.Errorf("object ID = %q", id.ObjectID)
	}
	// The package has no field for either claim: the address comes from Graph
	// and groups are never read from the token (D5, design section 4).
	enc, _ := json.Marshal(id)
	if strings.Contains(string(enc), "someone.else") || strings.Contains(string(enc), "g1") {
		t.Errorf("a claim leaked into the identity: %s", enc)
	}
}

func TestSignIn_UpperCaseObjectIDIsNormalized(t *testing.T) {
	e := newEnv(t)
	e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { s.Claims["oid"] = strings.ToUpper(testOID) })
	id, err := e.signIn()
	if err != nil {
		t.Fatal(err)
	}
	if id.ObjectID != testOID {
		t.Errorf("object ID = %q, want the lower-case form %q", id.ObjectID, testOID)
	}
}

func TestSignIn_DisplayName(t *testing.T) {
	t.Run("falls back to the user name", func(t *testing.T) {
		e := newEnv(t)
		e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { delete(s.Claims, "name") })
		id, err := e.signIn()
		if err != nil {
			t.Fatal(err)
		}
		if id.DisplayName != testUPN {
			t.Errorf("display name = %q", id.DisplayName)
		}
	})
	t.Run("is cleaned", func(t *testing.T) {
		e := newEnv(t)
		e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) {
			s.Claims["name"] = "  Avery" + string(rune(7)) + "\n  Test" + string(rune(0x200b)) + "person  "
		})
		id, err := e.signIn()
		if err != nil {
			t.Fatal(err)
		}
		if id.DisplayName != "Avery Testperson" {
			t.Errorf("display name = %q", id.DisplayName)
		}
	})
}

func TestComplete_RefusesBeforeTalkingToEntra(t *testing.T) {
	cases := []struct {
		name string
		mod  func(f *Flow, q url.Values, e *env)
		want error
	}{
		{"wrong state", func(f *Flow, q url.Values, e *env) { q.Set("state", "not-the-state") }, ErrStateMismatch},
		{"no state", func(f *Flow, q url.Values, e *env) { q.Del("state") }, ErrStateMismatch},
		{"state of another sign-in", func(f *Flow, q url.Values, e *env) {
			_, other, _ := e.client.Begin("")
			q.Set("state", other.State)
		}, ErrStateMismatch},
		{"empty flow", func(f *Flow, q url.Values, e *env) { *f = Flow{} }, ErrFlowInvalid},
		{"flow without a verifier", func(f *Flow, q url.Values, e *env) { f.Verifier = "" }, ErrFlowInvalid},
		{"flow past its time", func(f *Flow, q url.Values, e *env) { e.offset.Store(int64(FlowTTL + time.Second)) }, ErrFlowInvalid},
		{"no code", func(f *Flow, q url.Values, e *env) { q.Del("code") }, ErrNoCode},
		{"enormous code", func(f *Flow, q url.Values, e *env) { q.Set("code", strings.Repeat("a", 5000)) }, ErrNoCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			f, q := e.begin()
			before := len(e.fake.TokenForms())
			tc.mod(&f, q, e)
			_, err := e.client.Complete(context.Background(), f, q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if after := len(e.fake.TokenForms()); after != before {
				t.Error("the token endpoint was called although the callback was refused first")
			}
		})
	}
}

func TestComplete_EntraRefusal(t *testing.T) {
	e := newEnv(t)
	f, _ := e.begin()
	q := url.Values{"state": {f.State}, "error": {"access_denied"}, "error_description": {"AADSTS65004: The user declined to consent."}}
	_, err := e.client.Complete(context.Background(), f, q)
	var ae *AuthorizationError
	if !errors.As(err, &ae) || ae.Code != "access_denied" {
		t.Fatalf("error = %v, want an access_denied AuthorizationError", err)
	}
	if strings.Contains(err.Error(), "AADSTS") || strings.Contains(err.Error(), "declined") {
		t.Errorf("Entra's text is in the message: %v", err)
	}
	if ae.Description == "" {
		t.Error("the description should be kept for a log")
	}
	if ErrorKind(err) != "authorization:access_denied" {
		t.Errorf("kind = %s", ErrorKind(err))
	}

	// A refusal that does not carry this sign-in's state is not believed
	// either: the state is checked first.
	_, err = e.client.Complete(context.Background(), f, url.Values{"error": {"access_denied"}})
	if !errors.Is(err, ErrStateMismatch) {
		t.Errorf("error = %v, want ErrStateMismatch", err)
	}
}

func TestComplete_TokenEndpointRefusals(t *testing.T) {
	t.Run("code used twice", func(t *testing.T) {
		e := newEnv(t)
		f, q := e.begin()
		if _, err := e.client.Complete(context.Background(), f, q); err != nil {
			t.Fatal(err)
		}
		_, err := e.client.Complete(context.Background(), f, q)
		assertTokenError(t, err, "invalid_grant", 54005)
	})
	t.Run("wrong verifier", func(t *testing.T) {
		e := newEnv(t)
		f, q := e.begin()
		f.Verifier = "a-different-verifier-that-does-not-match-the-challenge"
		_, err := e.client.Complete(context.Background(), f, q)
		assertTokenError(t, err, "invalid_grant", 501481)
	})
	t.Run("code too old", func(t *testing.T) {
		e := newEnv(t)
		f, q := e.begin()
		e.fake.SetClockOffset(2 * time.Minute)
		_, err := e.client.Complete(context.Background(), f, q)
		assertTokenError(t, err, "invalid_grant", 50089)
	})
	t.Run("wrong client secret", func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg
		cfg.ClientSecret = "not-the-secret"
		c := e.mustNew(cfg)
		authURL, f, _ := c.Begin("")
		_, err := c.Complete(context.Background(), f, e.browse(authURL))
		assertTokenError(t, err, "invalid_client", 7000215)
		if strings.Contains(err.Error(), "not-the-secret") || strings.Contains(err.Error(), testSecret) {
			t.Errorf("a secret is in the error: %v", err)
		}
	})
	t.Run("unknown application", func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg
		cfg.ClientID = "00000000-0000-4000-8000-000000000000"
		c := e.mustNew(cfg)
		authURL, _, _ := c.Begin("")
		// The authorize page refuses an unknown application before any redirect.
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := hc.Get(authURL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("authorize status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("endpoint down", func(t *testing.T) {
		e := newEnv(t)
		f, q := e.begin()
		e.fake.SetDown(true)
		_, err := e.client.Complete(context.Background(), f, q)
		assertTokenError(t, err, "temporarily_unavailable", 0)
	})
}

func assertTokenError(t *testing.T, err error, code string, aadsts int) {
	t.Helper()
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v, want a TokenError", err)
	}
	if te.Code != code {
		t.Errorf("code = %q, want %q", te.Code, code)
	}
	if aadsts != 0 && (len(te.Codes) != 1 || te.Codes[0] != aadsts) {
		t.Errorf("AADSTS codes = %v, want [%d]", te.Codes, aadsts)
	}
	// Entra's description names the tenant and application; it is kept for a
	// log and kept out of the message.
	for _, v := range []string{testTenant, testClient, "Trace ID"} {
		if strings.Contains(err.Error(), v) {
			t.Errorf("the message contains %q: %v", v, err)
		}
	}
	if aadsts != 0 && te.Description == "" {
		t.Error("the description should be kept for a log")
	}
}

func TestComplete_NeverFollowsARedirectFromTheTokenEndpoint(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer elsewhere.Close()

	f, q := e.begin()
	e.fake.RedirectTokenEndpoint(elsewhere.URL)
	_, err := e.client.Complete(context.Background(), f, q)

	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Reason != "redirect" {
		t.Fatalf("error = %v, want a redirect ProtocolError", err)
	}
	if hits.Load() != 0 {
		t.Error("the client followed the redirect and sent its request, secret included, elsewhere")
	}
}

func TestCallerHTTPClientIsNotModified(t *testing.T) {
	e := newEnv(t)
	hc := &http.Client{Timeout: 3 * time.Second}
	cfg := e.cfg
	cfg.HTTPClient = hc
	e.mustNew(cfg)
	if hc.CheckRedirect != nil {
		t.Error("New changed the caller's http.Client")
	}
}

func TestComplete_MisbehavingTokenEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		reason  string
	}{
		{"oversized answer", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+10)))
		}, "too_large"},
		{"not JSON", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>oops</html>")) }, "malformed_body"},
		{"no ID token", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"token_type":"Bearer","access_token":"x"}`))
		}, "missing_field"},
		{"error that is not OAuth", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		}, "unexpected_status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(context.Background(), stubTenant(t, tc.handler))
			if err != nil {
				t.Fatal(err)
			}
			f, q := callbackFor(t, c)
			_, err = c.Complete(context.Background(), f, q)
			var pe *ProtocolError
			if !errors.As(err, &pe) || pe.Reason != tc.reason {
				t.Fatalf("error = %v, want ProtocolError %s", err, tc.reason)
			}
		})
	}

	t.Run("error value that is not a plain token", func(t *testing.T) {
		c, err := New(context.Background(), stubTenant(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Bad Value <script>","error_codes":[1,2,3,4,5,6,7,8,9,10,11,12]}`))
		}))
		if err != nil {
			t.Fatal(err)
		}
		f, q := callbackFor(t, c)
		_, err = c.Complete(context.Background(), f, q)
		var te *TokenError
		if !errors.As(err, &te) || te.Code != "unrecognized" || len(te.Codes) != 8 {
			t.Fatalf("error = %#v", err)
		}
	})
}

func TestNoSecretMaterialInErrors(t *testing.T) {
	e := newEnv(t)
	f, q := e.begin()
	code := q.Get("code")
	var all []error

	// Provoke the failures that handle the code, verifier and secret.
	bad := f
	bad.Verifier = "wrong-" + f.Verifier
	_, err := e.client.Complete(context.Background(), bad, q)
	all = append(all, err)
	_, err = e.client.Complete(context.Background(), f, q) // reuse of the now-consumed code
	all = append(all, err)
	cfg := e.cfg
	cfg.ClientSecret = "wrong-" + testSecret
	c := e.mustNew(cfg)
	authURL, f2, _ := c.Begin("")
	_, err = c.Complete(context.Background(), f2, e.browse(authURL))
	all = append(all, err)
	e.fake.SetDown(true)
	_, err = c.AppToken(context.Background(), GraphScope)
	all = append(all, err)

	for i, err := range all {
		if err == nil {
			t.Fatalf("case %d did not fail", i)
		}
		for _, secretish := range []string{testSecret, "wrong-" + testSecret, code, f.Verifier, f.Nonce} {
			if strings.Contains(err.Error(), secretish) || strings.Contains(ErrorKind(err), secretish) {
				t.Errorf("case %d: %q appears in %v", i, secretish, err)
			}
		}
	}
}
