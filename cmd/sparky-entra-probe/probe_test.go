// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra"
	"github.com/1kaius1/Sparky/internal/auth/entra/fakeentra"
)

// Canaries: values the probe must never let into a report. Each is distinctive
// enough that finding it anywhere in the output is certainly a leak.
const (
	cTenant = "7c2e9a41-3b5d-4f60-9a1e-0d8b6c4f2e13"
	cClient = "c4d8e1f2-6a3b-4c9d-8e7f-1a2b3c4d5e6f"
	cSecret = "canary-client-secret-value-9X"
	cOID    = "e5f6a7b8-c9d0-4e1f-8a2b-3c4d5e6f7a8b"
	cName   = "Casey Canarytest"
	cUPN    = "casey.canarytest@canary-corp.example"
	cEmail  = "casey.mail@canary-corp.example"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type probeEnv struct {
	t       *testing.T
	fake    *fakeentra.Server
	cfg     probeConfig
	console *syncBuffer
	p       *probe
}

func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	fake := fakeentra.New(fakeentra.Options{
		TenantID: cTenant, ClientID: cClient, ClientSecret: cSecret,
		RedirectURIs: []string{"http://localhost" + callbackPath},
		GraphRoles:   []string{"User.Read.All", "GroupMember.Read.All"},
	})
	t.Cleanup(fake.Close)
	fake.AddPerson(fakeentra.Person{OID: cOID, DisplayName: cName, PreferredUsername: cUPN, Email: cEmail})

	ec, err := entra.Config{
		TenantID: cTenant, ClientID: cClient, ClientSecret: cSecret, AuthorityURL: fake.URL(),
		RedirectURL: "http://localhost" + callbackPath,
	}.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	return &probeEnv{t: t, fake: fake, console: &syncBuffer{},
		cfg: probeConfig{Entra: ec, SignIn: true, SignInTimeout: 20 * time.Second}}
}

// start builds the probe, with a browser that follows the sign-in address.
func (e *probeEnv) start() {
	e.p = newProbe(e.cfg, e.console)
	e.p.open = func(authURL string) error {
		go func() {
			resp, err := http.Get(authURL) // follows the redirect to the probe's own listener
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func (e *probeEnv) run() string {
	e.t.Helper()
	if e.p == nil {
		e.start()
	}
	e.p.Run(context.Background())
	var out bytes.Buffer
	if err := e.p.report.Render(&out, "Sparky Entra ID probe report"); err != nil {
		e.t.Fatalf("the report was refused: %v", err)
	}
	return out.String()
}

// canaries are the values to look for, with names for the failure message.
func (e *probeEnv) canaries() map[string]string {
	c := map[string]string{
		"tenant ID": cTenant, "client ID": cClient, "client secret": cSecret, "object ID": cOID,
		"display name": cName, "user name": cUPN, "email": cEmail, "key ID": e.fake.CurrentKey().KID,
		"host and port": strings.TrimPrefix(e.fake.URL(), "http://"),
	}
	// The key material, as it would appear in a JWKS document.
	n := base64.RawURLEncoding.EncodeToString(e.fake.CurrentKey().Private.N.Bytes())
	c["key modulus"] = n[:40]
	for i, tok := range e.fake.IssuedTokens() {
		c["issued token "+string(rune('0'+i))] = tok
		for j, part := range strings.Split(tok, ".") {
			if len(part) > 12 {
				c["issued token "+string(rune('0'+i))+" part "+string(rune('0'+j))] = part
			}
		}
	}
	for i, form := range e.fake.TokenForms() {
		for _, name := range []string{"code", "code_verifier"} {
			if v := form.Get(name); v != "" {
				c["token request "+name+string(rune('0'+i))] = v
			}
		}
	}
	return c
}

var (
	reGUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reJWT  = regexp.MustCompile(`eyJ`)
)

// assertNoLeak fails if text contains any canary, or anything shaped like an
// identifier, a token, an address or a URL. It does not use the report's own
// checks: a bug there must not hide a leak here.
func assertNoLeak(t *testing.T, what, text string, canaries map[string]string) {
	t.Helper()
	lower := strings.ToLower(text)
	for name, v := range canaries {
		if v != "" && strings.Contains(lower, strings.ToLower(v)) {
			t.Errorf("%s contains the %s", what, name)
		}
	}
	if reGUID.MatchString(text) {
		t.Errorf("%s contains something shaped like a GUID", what)
	}
	if reJWT.MatchString(text) {
		t.Errorf("%s contains something shaped like a JWT", what)
	}
	for _, bad := range []string{"@", "://"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s contains %q", what, bad)
		}
	}
}

// consoleWithoutAddress is the console output minus the one line that is allowed
// to hold identifiers: the sign-in address, which the person opens themselves and
// which is labelled as not part of the report.
func consoleWithoutAddress(t *testing.T, console string) string {
	t.Helper()
	var kept []string
	addresses := 0
	for _, line := range strings.Split(console, "\n") {
		if strings.HasPrefix(line, "http") {
			addresses++
			continue
		}
		kept = append(kept, line)
	}
	if addresses > 1 {
		t.Errorf("the sign-in address was printed %d times", addresses)
	}
	return strings.Join(kept, "\n")
}

func TestProbe_FullRunAgainstTheFakeTenant(t *testing.T) {
	e := newProbeEnv(t)
	report := e.run()

	for _, want := range []string{
		"Step 1: Discovery document", "Step 2: Signing keys", "Step 3: App token for Microsoft Graph",
		"Step 4: Microsoft Graph calls", "Step 5: Interactive sign-in",
		"Summary: 4 passed, 0 failed, 1 skipped",
		"issuer is the tenant's v2.0 issuer: yes",
		"token_endpoint is on the authority's own origin: yes",
		"accepted by the sign-in client: yes",
		"keys: 1",
		"every key has a key ID: yes",
		"application permissions (roles claim): GroupMember.Read.All, User.Read.All",
		"issuer form: sts.windows.net, tenant",
		"audience: graph application id",
		"signed-in person: user-1",
		"tenant in the token: tenant-A",
		"tenant in the token is the configured tenant: yes",
		"signing algorithm: RS256",
		"token version: 2.0",
		"an email claim is present (Sparky does not use it): no",
		"a preferred_username claim is present: yes",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not contain %q", want)
		}
	}
	if !e.p.report.Overall() {
		t.Error("Overall is false although every step passed or was skipped")
	}

	assertNoLeak(t, "the report", report, e.canaries())
	console := e.console.String()
	assertNoLeak(t, "the console (outside the sign-in address)", consoleWithoutAddress(t, console), e.canaries())

	// The exception is real and confined: the address is on the console, carries
	// the identifiers, and is labelled.
	if !strings.Contains(console, cTenant) || !strings.Contains(console, "NOT part of the report") {
		t.Error("the sign-in address and its warning should be on the console")
	}
	t.Logf("report:\n%s", report)
}

func TestProbe_NothingLeaksWhenThingsGoWrong(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *probeEnv)
		expect []string // text that must be in the report
		fails  bool
	}{
		{"wrong client secret", func(e *probeEnv) { e.cfg.Entra.ClientSecret = "wrong-" + cSecret },
			[]string{"token_endpoint:invalid_client", "AADSTS error numbers: 7000215"}, true},
		{"tenant that does not exist", func(e *probeEnv) { e.cfg.Entra.TenantID = "11111111-2222-4333-8444-555555555555" },
			[]string{"unexpected answer (HTTP 400)"}, true},
		{"server down", func(e *probeEnv) { e.fake.SetDown(true) },
			[]string{"unexpected answer (HTTP 503)"}, true},
		{"server gone", func(e *probeEnv) { e.fake.Close() },
			[]string{"network:connection"}, true},
		{"ID token for another application", func(e *probeEnv) {
			e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { s.Claims["aud"] = "99999999-8888-4777-8666-555555555555" })
		}, []string{"validation:audience"}, true},
		{"ID token with a bad signature", func(e *probeEnv) {
			e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { s.SignWith = e.fake.NewUnpublishedKey() })
		}, []string{"validation:signature"}, true},
		{"ID token from another tenant", func(e *probeEnv) {
			e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) {
				s.Claims["tid"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
				s.Claims["iss"] = e.fake.URL() + "/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/v2.0"
			})
		}, []string{"validation:issuer"}, true},
		{"redirect address not registered", func(e *probeEnv) {
			e.cfg.SignInTimeout = 600 * time.Millisecond
			e.fake.SetRedirectURIs([]string{"https://elsewhere.example.test/callback"})
		}, []string{"timed out waiting for the sign-in"}, true},
		{"the person never signs in", func(e *probeEnv) {
			e.cfg.SignInTimeout = 400 * time.Millisecond
			e.start()
			e.p.open = nil
		}, []string{"timed out waiting for the sign-in"}, true},
		{"sign-in skipped", func(e *probeEnv) { e.cfg.SignIn = false },
			[]string{"Step 5: Interactive sign-in", "result: skipped (not requested)", "Summary: 3 passed, 0 failed, 2 skipped"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newProbeEnv(t)
			tc.setup(e)
			report := e.run()

			for _, want := range tc.expect {
				if !strings.Contains(report, want) {
					t.Errorf("the report does not contain %q", want)
				}
			}
			if got := !e.p.report.Overall(); got != tc.fails {
				t.Errorf("a failure was expected: %v, got %v", tc.fails, got)
			}
			assertNoLeak(t, "the report", report, e.canaries())
			assertNoLeak(t, "the console", consoleWithoutAddress(t, e.console.String()), e.canaries())
		})
	}
}

func TestProbe_AStrayRequestToTheCallbackDoesNotEndTheRun(t *testing.T) {
	e := newProbeEnv(t)
	e.start()
	realOpen := e.p.open
	e.p.open = func(authURL string) error {
		// Something else on the machine hits the callback first: a request that
		// carries the wrong state, and one carrying none.
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		callback := u.Query().Get("redirect_uri")
		for _, query := range []string{"?code=x&state=not-the-state", "?code=x", ""} {
			resp, err := http.Get(callback + query)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("a stray request %q was answered HTTP %d, want 400", query, resp.StatusCode)
			}
		}
		return realOpen(authURL)
	}
	report := e.run()
	if !strings.Contains(report, "Summary: 4 passed, 0 failed, 1 skipped") {
		t.Errorf("the stray requests ended or spoiled the run:\n%s", report)
	}
}
