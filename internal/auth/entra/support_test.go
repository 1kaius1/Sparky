// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfigNormalized(t *testing.T) {
	good := Config{TenantID: testTenant, ClientID: testClient, ClientSecret: testSecret, RedirectURL: testRedirect}
	with := func(mod func(*Config)) Config { c := good; mod(&c); return c }

	t.Run("defaults and normalization", func(t *testing.T) {
		c, err := with(func(c *Config) {
			c.TenantID = "  " + strings.ToUpper(testTenant) + " "
			c.ClientID = strings.ToUpper(testClient)
		}).Normalized()
		if err != nil {
			t.Fatal(err)
		}
		if c.TenantID != testTenant || c.ClientID != testClient {
			t.Errorf("IDs not lower-cased: %q %q", c.TenantID, c.ClientID)
		}
		if c.AuthorityURL != PublicCloudAuthority {
			t.Errorf("authority = %q", c.AuthorityURL)
		}
		if want := PublicCloudAuthority + "/" + testTenant + "/v2.0"; c.IssuerURL() != want {
			t.Errorf("issuer = %q, want %q", c.IssuerURL(), want)
		}
	})

	accepted := []string{"https://login.microsoftonline.com", "https://login.microsoftonline.com/", "http://localhost:8123", "http://127.0.0.1:9", "http://[::1]:9"}
	for _, a := range accepted {
		t.Run("accepts authority "+a, func(t *testing.T) {
			c, err := with(func(c *Config) { c.AuthorityURL = a }).Normalized()
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(c.AuthorityURL, "/") {
				t.Errorf("trailing slash kept: %q", c.AuthorityURL)
			}
		})
	}

	rejected := map[string]Config{
		"tenant as a domain name":       with(func(c *Config) { c.TenantID = "contoso.onmicrosoft.com" }),
		"empty tenant":                  with(func(c *Config) { c.TenantID = "" }),
		"common as the tenant":          with(func(c *Config) { c.TenantID = "common" }),
		"client ID that is not a GUID":  with(func(c *Config) { c.ClientID = "my-app" }),
		"empty secret":                  with(func(c *Config) { c.ClientSecret = "" }),
		"blank secret":                  with(func(c *Config) { c.ClientSecret = "   " }),
		"plain http to a remote host":   with(func(c *Config) { c.AuthorityURL = "http://login.example.test" }),
		"authority with a path":         with(func(c *Config) { c.AuthorityURL = "https://login.example.test/tenant" }),
		"authority with a query":        with(func(c *Config) { c.AuthorityURL = "https://login.example.test?x=1" }),
		"authority with credentials":    with(func(c *Config) { c.AuthorityURL = "https://user:pw@login.example.test" }),
		"authority with another scheme": with(func(c *Config) { c.AuthorityURL = "ftp://login.example.test" }),
		"relative redirect URL":         with(func(c *Config) { c.RedirectURL = "/login/entra/callback" }),
		"redirect URL with a fragment":  with(func(c *Config) { c.RedirectURL = testRedirect + "#x" }),
		"redirect URL of another kind":  with(func(c *Config) { c.RedirectURL = "ftp://sparky.example.test/cb" }),
		"empty redirect URL":            with(func(c *Config) { c.RedirectURL = "" }),
	}
	for name, cfg := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := cfg.Normalized()
			if err == nil {
				t.Fatal("accepted")
			}
			// Messages name fields and never echo what was configured.
			for _, v := range []string{testSecret, "pw@", "contoso"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the message contains %q: %v", v, err)
				}
			}
		})
	}
}

func TestSecretsRedactThemselvesEverywhere(t *testing.T) {
	e := newEnv(t)
	tok, err := e.client.AppToken(context.Background(), GraphScope)
	if err != nil {
		t.Fatal(err)
	}
	bearer := tok.Bearer()
	if bearer == "" {
		t.Fatal("no bearer text")
	}
	_, flow, _ := e.client.Begin("")
	secret := Secret(testSecret)
	cfg := e.cfg

	type wrapper struct {
		Secret Secret
		Token  AccessToken
		Flow   Flow
		Ptr    *AccessToken
	}
	w := wrapper{secret, tok, flow, &tok}

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("sign-in", "secret", secret, "token", tok, "flow", flow, "cfg", cfg.ClientSecret)
	var asJSON []byte
	asJSON, _ = json.Marshal(w)
	asText, _ := secret.MarshalText()

	var all []string
	for _, v := range []any{secret, tok, &tok, flow, w, &w, cfg} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			all = append(all, fmt.Sprintf(verb, v))
		}
	}
	all = append(all, logged.String(), string(asJSON), string(asText), fmt.Sprint(secret, tok, flow))

	for _, out := range all {
		for _, sensitive := range []string{testSecret, bearer, flow.State, flow.Nonce, flow.Verifier} {
			if strings.Contains(out, sensitive) {
				t.Fatalf("a secret appeared in formatted output: %s", out)
			}
		}
		// %x would hide the text in hex too.
		for _, sensitive := range []string{testSecret, bearer, flow.Verifier} {
			if strings.Contains(out, fmt.Sprintf("%x", sensitive)) {
				t.Fatalf("a secret appeared in hex: %s", out)
			}
		}
	}
	if secret.Reveal() != testSecret || tok.Bearer() != bearer {
		t.Error("Reveal and Bearer must still return the text")
	}
}

func TestFlowSealing(t *testing.T) {
	secret := []byte("a-session-secret-of-sufficient-length")
	now := time.Now()
	f := Flow{State: "st", Nonce: "no", Verifier: "ve-ri-fier", ReturnPath: "/models/inventory?node=1", Expires: now.Add(FlowTTL)}

	t.Run("round trip", func(t *testing.T) {
		sealed, err := SealFlow(secret, f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := OpenFlow(secret, sealed, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != f.State || got.Nonce != f.Nonce || got.Verifier != f.Verifier || got.ReturnPath != f.ReturnPath ||
			got.Expires.Unix() != f.Expires.Unix() {
			t.Errorf("got %+v, want %+v", got, f)
		}
	})

	t.Run("is encrypted, not only signed", func(t *testing.T) {
		sealed, _ := SealFlow(secret, f)
		raw, _ := base64.RawURLEncoding.DecodeString(sealed)
		for _, plain := range []string{f.State, f.Nonce, f.Verifier, f.ReturnPath} {
			if bytes.Contains(raw, []byte(plain)) || strings.Contains(sealed, plain) {
				t.Errorf("%q is readable in the cookie value", plain)
			}
		}
	})

	t.Run("two seals of the same flow differ", func(t *testing.T) {
		a, _ := SealFlow(secret, f)
		b, _ := SealFlow(secret, f)
		if a == b {
			t.Error("identical output: the nonce is not random")
		}
	})

	t.Run("any single changed byte is refused", func(t *testing.T) {
		sealed, _ := SealFlow(secret, f)
		raw, _ := base64.RawURLEncoding.DecodeString(sealed)
		for i := range raw {
			tampered := bytes.Clone(raw)
			tampered[i] ^= 0x01
			if _, err := OpenFlow(secret, base64.RawURLEncoding.EncodeToString(tampered), now); !errors.Is(err, ErrFlowInvalid) {
				t.Fatalf("byte %d changed and the value was accepted: %v", i, err)
			}
		}
	})

	t.Run("any truncation is refused", func(t *testing.T) {
		sealed, _ := SealFlow(secret, f)
		raw, _ := base64.RawURLEncoding.DecodeString(sealed)
		for n := 0; n < len(raw); n++ {
			if _, err := OpenFlow(secret, base64.RawURLEncoding.EncodeToString(raw[:n]), now); !errors.Is(err, ErrFlowInvalid) {
				t.Fatalf("truncation to %d bytes accepted: %v", n, err)
			}
		}
	})

	t.Run("another secret is refused", func(t *testing.T) {
		sealed, _ := SealFlow(secret, f)
		if _, err := OpenFlow([]byte("another-session-secret-of-length"), sealed, now); !errors.Is(err, ErrFlowInvalid) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("past its time is refused", func(t *testing.T) {
		sealed, _ := SealFlow(secret, f)
		if _, err := OpenFlow(secret, sealed, f.Expires); !errors.Is(err, ErrFlowInvalid) {
			t.Errorf("at the expiry instant: %v", err)
		}
		if _, err := OpenFlow(secret, sealed, f.Expires.Add(time.Hour)); !errors.Is(err, ErrFlowInvalid) {
			t.Errorf("an hour later: %v", err)
		}
	})

	t.Run("garbage is refused", func(t *testing.T) {
		for _, s := range []string{"", "!!!not base64!!!", "AAAA", strings.Repeat("A", 4000)} {
			if _, err := OpenFlow(secret, s, now); !errors.Is(err, ErrFlowInvalid) {
				t.Errorf("%q: %v", s, err)
			}
		}
	})

	t.Run("incomplete flows are refused after opening", func(t *testing.T) {
		for _, bad := range []Flow{{Nonce: "n", Verifier: "v", Expires: f.Expires}, {State: "s", Verifier: "v", Expires: f.Expires}, {State: "s", Nonce: "n", Expires: f.Expires}} {
			sealed, err := SealFlow(secret, bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenFlow(secret, sealed, now); !errors.Is(err, ErrFlowInvalid) {
				t.Errorf("%+v accepted", bad)
			}
		}
	})

	t.Run("limits", func(t *testing.T) {
		long := f
		long.ReturnPath = strings.Repeat("a", maxReturnPath+1)
		if _, err := SealFlow(secret, long); err == nil {
			t.Error("an over-long return path was sealed")
		}
		if _, err := SealFlow([]byte("short"), f); err == nil {
			t.Error("a short secret was accepted")
		}
		if _, err := OpenFlow([]byte("short"), "x", now); err == nil {
			t.Error("a short secret was accepted on open")
		}
		sealed, _ := SealFlow(secret, Flow{State: strings.Repeat("s", 43), Nonce: strings.Repeat("n", 43), Verifier: strings.Repeat("v", 43), ReturnPath: strings.Repeat("r", maxReturnPath), Expires: f.Expires})
		if len(sealed) > 1500 {
			t.Errorf("a worst-case cookie value is %d bytes", len(sealed))
		}
	})
}

func TestAppToken(t *testing.T) {
	t.Run("gets a token for the graph scope", func(t *testing.T) {
		e := newEnv(t)
		tok, err := e.client.AppToken(context.Background(), GraphScope)
		if err != nil {
			t.Fatal(err)
		}
		if tok.Bearer() == "" || tok.ExpiresIn != 3599*time.Second {
			t.Errorf("token = %v, lifetime %v", tok, tok.ExpiresIn)
		}
		forms := e.fake.TokenForms()
		form := forms[len(forms)-1]
		for name, want := range map[string]string{
			"grant_type": "client_credentials", "client_id": testClient, "client_secret": testSecret, "scope": GraphScope,
		} {
			if got := form.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("another resource is an invalid scope", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.client.AppToken(context.Background(), "https://vault.azure.net/.default")
		assertTokenError(t, err, "invalid_scope", 70011)
	})

	t.Run("several scopes are refused before any request", func(t *testing.T) {
		e := newEnv(t)
		before := len(e.fake.TokenForms())
		for _, s := range []string{"", "a b", "a\tb", "a\nb"} {
			if _, err := e.client.AppToken(context.Background(), s); err == nil {
				t.Errorf("scope %q accepted", s)
			}
		}
		if len(e.fake.TokenForms()) != before {
			t.Error("a request was sent")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg
		cfg.ClientSecret = "nope"
		_, err := e.mustNew(cfg).AppToken(context.Background(), GraphScope)
		assertTokenError(t, err, "invalid_client", 7000215)
	})

	odd := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"expires_in as a string", `{"token_type":"Bearer","expires_in":"3599","access_token":"t"}`, false},
		{"lower-case bearer", `{"token_type":"bearer","expires_in":60,"access_token":"t"}`, false},
		{"no token", `{"token_type":"Bearer","expires_in":60}`, true},
		{"no lifetime", `{"token_type":"Bearer","access_token":"t"}`, true},
		{"zero lifetime", `{"token_type":"Bearer","expires_in":0,"access_token":"t"}`, true},
		{"another token type", `{"token_type":"MAC","expires_in":60,"access_token":"t"}`, true},
		{"not JSON", `nope`, true},
	}
	for _, tc := range odd {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(context.Background(), stubTenant(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.AppToken(context.Background(), GraphScope)
			if (err != nil) != tc.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestErrorKind(t *testing.T) {
	e := newEnv(t)

	// A real timeout and a real refused connection, to prove the classification
	// works on what the transport produces and not only on constructed errors.
	cfg := e.cfg
	cfg.HTTPClient = shortTimeoutClient(80 * time.Millisecond)
	c2 := e.mustNew(e.cfg)
	c2.hc = cfg.httpClient()
	e.fake.SetDelay(300 * time.Millisecond)
	_, timeoutErr := c2.AppToken(context.Background(), GraphScope)
	e.fake.SetDelay(0)
	e.fake.Close()
	_, refusedErr := c2.AppToken(context.Background(), GraphScope)

	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsSrv.Close()
	tlsCfg := e.cfg
	tlsCfg.AuthorityURL = tlsSrv.URL
	_, tlsErr := New(context.Background(), tlsCfg)

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"flow", fmt.Errorf("wrapped: %w", ErrFlowInvalid), "flow_state"},
		{"state", ErrStateMismatch, "state_mismatch"},
		{"no code", ErrNoCode, "no_code"},
		{"authorization", &AuthorizationError{Code: "access_denied"}, "authorization:access_denied"},
		{"token endpoint", &TokenError{Code: "invalid_grant"}, "token_endpoint:invalid_grant"},
		{"validation", &ValidationError{Reason: "nonce"}, "validation:nonce"},
		{"protocol", &ProtocolError{Reason: "too_large"}, "protocol:too_large"},
		{"discovery", &DiscoveryError{Kind: "bad_document"}, "discovery:bad_document"},
		{"timeout", timeoutErr, "network:timeout"},
		{"refused", refusedErr, "network:connection"},
		{"untrusted certificate", tlsErr, "discovery:unreachable/tls"},
		{"anything else", errors.New("secret detail " + testSecret), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ErrorKind(tc.err)
			if got != tc.want {
				t.Errorf("ErrorKind = %q, want %q (error: %v)", got, tc.want, tc.err)
			}
			for _, v := range []string{testSecret, testTenant, testClient, "127.0.0.1"} {
				if strings.Contains(got, v) {
					t.Errorf("the kind %q contains %q", got, v)
				}
			}
		})
	}
}

func TestCleanDisplayName(t *testing.T) {
	cases := map[string]string{
		"":                                      "",
		"Avery Testperson":                      "Avery Testperson",
		"  Avery   Testperson  ":                "Avery Testperson",
		"Avery\tTest\nperson":                   "Avery Test person",
		"Av\u0000ery\u0007":                     "Avery",
		"Zero" + string(rune(0x200b)) + "width": "Zerowidth",
		"Bidi" + string(rune(0x202e)) + "override":              "Bidioverride",
		"Jos" + string(rune(0xe9)) + " " + string(rune(0x5f20)): "Jos" + string(rune(0xe9)) + " " + string(rune(0x5f20)),
		"bad\xffbytes":                   "badbytes",
		"<script>alert(1)</script> Name": "<script>alert(1)</script> Name", // escaping is the template layer's job
	}
	for in, want := range cases {
		if got := CleanDisplayName(in); got != want {
			t.Errorf("CleanDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanDisplayName(strings.Repeat(string(rune(0xe9)), 500)); len([]rune(got)) != maxDisplayNameRunes {
		t.Errorf("long name has %d runes, want %d", len([]rune(got)), maxDisplayNameRunes)
	}
}
