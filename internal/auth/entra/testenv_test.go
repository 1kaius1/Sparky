// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra/fakeentra"
)

// The values below are recognizable on sight in a failure message, and the
// secret in particular is checked never to appear in an error, a log line or a
// formatted value.
const (
	testTenant   = "5f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	testClient   = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	testSecret   = "s3cr3t-value-that-must-never-appear~Q7"
	testRedirect = "https://sparky.example.test/login/entra/callback"
	testOID      = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	testName     = "Avery Testperson"
	testUPN      = "avery.testperson@example.test"
)

// env is a fake tenant, a Client discovered against it, and a clock the test
// can move.
type env struct {
	t      *testing.T
	fake   *fakeentra.Server
	cfg    Config
	client *Client
	offset atomic.Int64 // nanoseconds the client's clock is ahead of real time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t}
	e.fake = fakeentra.New(fakeentra.Options{
		TenantID:     testTenant,
		ClientID:     testClient,
		ClientSecret: testSecret,
		RedirectURIs: []string{testRedirect},
		GraphRoles:   []string{"User.Read.All", "GroupMember.Read.All"},
	})
	t.Cleanup(e.fake.Close)
	e.fake.AddPerson(fakeentra.Person{OID: testOID, DisplayName: testName, PreferredUsername: testUPN, Email: testUPN})

	e.cfg = Config{
		TenantID:     testTenant,
		ClientID:     testClient,
		ClientSecret: Secret(testSecret),
		AuthorityURL: e.fake.URL(),
		RedirectURL:  testRedirect,
		Now:          func() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) },
	}
	e.client = e.mustNew(e.cfg)
	return e
}

func (e *env) mustNew(cfg Config) *Client {
	e.t.Helper()
	c, err := New(context.Background(), cfg)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	return c
}

// browse plays the browser: it requests the authorize address without
// following the redirect and returns the query Entra put on it.
func (e *env) browse(authURL string) url.Values {
	e.t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(authURL)
	if err != nil {
		e.t.Fatalf("authorize request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("authorize answered HTTP %d, want a redirect", resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		e.t.Fatalf("authorize redirect: %v", err)
	}
	return loc.Query()
}

// begin starts a sign-in and returns the flow and what the redirect carries.
func (e *env) begin() (Flow, url.Values) {
	e.t.Helper()
	authURL, f, err := e.client.Begin("/dashboard")
	if err != nil {
		e.t.Fatalf("Begin: %v", err)
	}
	return f, e.browse(authURL)
}

// signIn runs a whole sign-in.
func (e *env) signIn() (*Identity, error) {
	e.t.Helper()
	f, q := e.begin()
	return e.client.Complete(context.Background(), f, q)
}

// stubTenant serves discovery pointing at itself, with the token endpoint
// handler supplied by the test, for answers the fake tenant would never give.
func stubTenant(t *testing.T, token http.HandlerFunc) Config {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                 base + "/" + testTenant + "/v2.0",
			"authorization_endpoint": base + "/" + testTenant + "/oauth2/v2.0/authorize",
			"token_endpoint":         base + "/" + testTenant + "/oauth2/v2.0/token",
			"jwks_uri":               base + "/" + testTenant + "/discovery/v2.0/keys",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("GET /{tenant}/discovery/v2.0/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", token)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	return Config{
		TenantID: testTenant, ClientID: testClient, ClientSecret: Secret(testSecret),
		AuthorityURL: srv.URL, RedirectURL: testRedirect,
	}
}

func shortTimeoutClient(d time.Duration) *http.Client { return &http.Client{Timeout: d} }

// callbackFor builds a valid-looking callback for a fresh flow, so a test can
// reach the token exchange against a stub.
func callbackFor(t *testing.T, c *Client) (Flow, url.Values) {
	t.Helper()
	_, f, err := c.Begin("")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return f, url.Values{"state": {f.State}, "code": {"a-code"}}
}
