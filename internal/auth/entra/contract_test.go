// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Target is where the contract suite runs: an authority and the credentials of
// an app registration in a tenant. Everything the suite asserts is something a
// real tenant must also do, so the same checks run against the fake tenant
// always and against a real one only when someone opts in.
//
// The suite deliberately makes only requests that succeed. A check that
// provokes a failure (a wrong secret, a bad code) would put failed sign-ins in
// a real tenant's logs for no benefit; those cases are tested against the fake
// alone.
type Target struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	AuthorityURL string
}

func (tg Target) config() Config {
	return Config{
		TenantID: tg.TenantID, ClientID: tg.ClientID, ClientSecret: Secret(tg.ClientSecret),
		AuthorityURL: tg.AuthorityURL,
		// Never used by the suite, which signs no one in.
		RedirectURL: "https://sparky.invalid/login/entra/callback",
	}
}

func runContract(t *testing.T, tg Target) {
	cfg, err := tg.config().Normalized()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var jwksURI string
	t.Run("discovery", func(t *testing.T) {
		if _, err := New(ctx, cfg); err != nil {
			t.Fatalf("the client could not use the tenant's metadata: %s", ErrorKind(err))
		}
		doc := getJSON(t, ctx, cfg.IssuerURL()+"/.well-known/openid-configuration")
		for _, member := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
			if s, _ := doc[member].(string); s == "" {
				t.Errorf("the discovery document has no %s", member)
			}
		}
		if doc["issuer"] != cfg.IssuerURL() {
			t.Error("the discovery document's issuer is not the tenant's v2.0 issuer")
		}
		jwksURI, _ = doc["jwks_uri"].(string)
	})

	t.Run("signing keys", func(t *testing.T) {
		if jwksURI == "" {
			t.Skip("no key set address (the discovery check failed)")
		}
		set := getJSON(t, ctx, jwksURI)
		keys, _ := set["keys"].([]any)
		if len(keys) == 0 {
			t.Fatal("the key set is empty")
		}
		for i, k := range keys {
			key, _ := k.(map[string]any)
			for _, member := range []string{"kty", "kid", "n", "e"} {
				if s, _ := key[member].(string); s == "" {
					t.Errorf("key %d has no %s", i, member)
				}
			}
			if key["kty"] != "RSA" {
				t.Errorf("key %d is not an RSA key", i)
			}
		}
	})

	t.Run("app token", func(t *testing.T) {
		c, err := New(ctx, cfg)
		if err != nil {
			t.Skip("no client (the discovery check failed)")
		}
		tok, err := c.AppToken(ctx, GraphScope)
		if err != nil {
			t.Fatalf("no app token: %s", ErrorKind(err))
		}
		if tok.Bearer() == "" || tok.ExpiresIn <= 0 {
			t.Error("the token is empty or already expired")
		}
	})
}

func getJSON(t *testing.T, ctx context.Context, rawURL string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %s", ErrorKind(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected answer: HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal("the answer is not a JSON object")
	}
	return out
}

func TestContract_FakeTenant(t *testing.T) {
	e := newEnv(t)
	runContract(t, Target{TenantID: testTenant, ClientID: testClient, ClientSecret: testSecret, AuthorityURL: e.fake.URL()})
}

// TestContract_RealTenant runs the suite against a real tenant, only when all of
// SPARKY_ENTRA_REAL_TENANT_ID, _CLIENT_ID and _CLIENT_SECRET are set (and
// optionally _AUTHORITY_URL). Nobody sets them on the development machine; the
// sparky-entra-probe program is how a real run is made and reported safely.
func TestContract_RealTenant(t *testing.T) {
	tg := Target{
		TenantID:     os.Getenv("SPARKY_ENTRA_REAL_TENANT_ID"),
		ClientID:     os.Getenv("SPARKY_ENTRA_REAL_CLIENT_ID"),
		ClientSecret: os.Getenv("SPARKY_ENTRA_REAL_CLIENT_SECRET"),
		AuthorityURL: os.Getenv("SPARKY_ENTRA_REAL_AUTHORITY_URL"),
	}
	if tg.TenantID == "" || tg.ClientID == "" || tg.ClientSecret == "" {
		t.Skip("SPARKY_ENTRA_REAL_* not set: no real tenant to test against")
	}
	runContract(t, tg)
}
