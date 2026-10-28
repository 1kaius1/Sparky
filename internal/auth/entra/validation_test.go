// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra/fakeentra"
)

const otherTenant = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// TestIDTokenValidation sends the tokens Entra must never be believed to send,
// each altered in exactly one way, and expects each to be refused for the right
// reason. The first row is the control: an unaltered token is accepted.
func TestIDTokenValidation(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(s *fakeentra.IDTokenSpec, e *env)
		reason string // "" means accepted
	}{
		{"unaltered", func(s *fakeentra.IDTokenSpec, e *env) {}, ""},

		// Audience.
		{"another application's audience", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["aud"] = otherTenant }, "audience"},
		{"two audiences, ours among them", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["aud"] = []string{testClient, otherTenant} }, "audience"},
		{"audience in another case", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["aud"] = strings.ToUpper(testClient) }, "audience"},

		// Issuer and tenant.
		{"issuer of another tenant", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["iss"] = e.fake.URL() + "/" + otherTenant + "/v2.0" }, "issuer"},
		{"issuer with a trailing slash", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["iss"] = e.fake.Issuer() + "/" }, "issuer"},
		{"issuer in the v1.0 form", func(s *fakeentra.IDTokenSpec, e *env) {
			s.Claims["iss"] = "https://sts.windows.net/" + testTenant + "/"
		}, "issuer"},
		{"tenant claim of another tenant", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["tid"] = otherTenant }, "tenant"},
		{"a second tenant's whole token", func(s *fakeentra.IDTokenSpec, e *env) {
			s.Claims["tid"] = otherTenant
			s.Claims["iss"] = e.fake.URL() + "/" + otherTenant + "/v2.0"
		}, "issuer"},
		{"no tenant claim", func(s *fakeentra.IDTokenSpec, e *env) { delete(s.Claims, "tid") }, "tenant"},

		// Time. The client allows a minute of clock skew on expiry and go-oidc
		// allows five minutes on not-before.
		{"expired five minutes ago", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["exp"] = time.Now().Add(-5 * time.Minute).Unix() }, "expired"},
		{"expired thirty seconds ago (inside the skew)", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["exp"] = time.Now().Add(-30 * time.Second).Unix() }, ""},
		{"not valid for ten minutes", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["nbf"] = time.Now().Add(10 * time.Minute).Unix() }, "not_yet_valid"},
		{"not valid for two minutes (inside the leeway)", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["nbf"] = time.Now().Add(2 * time.Minute).Unix() }, ""},

		// Nonce.
		{"another sign-in's nonce", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["nonce"] = "a-nonce-from-another-sign-in" }, "nonce"},
		{"no nonce", func(s *fakeentra.IDTokenSpec, e *env) { delete(s.Claims, "nonce") }, "nonce"},

		// Signature and algorithm.
		{"signed by a key that is not published", func(s *fakeentra.IDTokenSpec, e *env) { s.SignWith = e.fake.NewUnpublishedKey() }, "signature"},
		{"a published key id on a forged signature", func(s *fakeentra.IDTokenSpec, e *env) {
			s.Header["kid"] = e.fake.CurrentKey().KID
			s.SignWith = e.fake.NewUnpublishedKey()
		}, "signature"},
		{"HMAC keyed with the public key (algorithm confusion)", func(s *fakeentra.IDTokenSpec, e *env) { s.Alg = "HS256" }, "algorithm"},
		{"RS384 instead of RS256", func(s *fakeentra.IDTokenSpec, e *env) { s.Alg = "RS384" }, "algorithm"},
		{"alg none", func(s *fakeentra.IDTokenSpec, e *env) { s.Alg = "none" }, "algorithm"},

		// Who.
		{"no object ID", func(s *fakeentra.IDTokenSpec, e *env) { delete(s.Claims, "oid") }, "subject"},
		{"object ID that is not a GUID", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["oid"] = "alice@example.test" }, "subject"},
		{"object ID of the wrong type", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["oid"] = 12345 }, "subject"},

		// Not an error.
		{"unfamiliar token version", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["ver"] = "3.0" }, ""},
		{"extra claims", func(s *fakeentra.IDTokenSpec, e *env) { s.Claims["xms_unknown"] = map[string]any{"a": 1} }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { tc.mod(s, e) })
			id, err := e.signIn()

			if tc.reason == "" {
				if err != nil {
					t.Fatalf("a good token was refused: %v", err)
				}
				if id.ObjectID != testOID {
					t.Errorf("object ID = %q", id.ObjectID)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
			if ve.Reason != tc.reason {
				t.Errorf("reason = %q, want %q (underlying: %v)", ve.Reason, tc.reason, errors.Unwrap(ve))
			}
			if id != nil {
				t.Error("an identity was returned along with the error")
			}
			if got := ErrorKind(err); got != "validation:"+tc.reason {
				t.Errorf("ErrorKind = %q", got)
			}
			// The message carries the reason and nothing from the token.
			for _, v := range []string{testOID, testName, testClient, testTenant, otherTenant} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the message contains %q: %v", v, err)
				}
			}
		})
	}
}

func TestIDTokenValidation_ClockSkewOfTheServer(t *testing.T) {
	// This server's clock is two minutes ahead of Entra's: a token issued just
	// now has an iat and nbf in this server's past, and an exp an hour ahead.
	e := newEnv(t)
	e.offset.Store(int64(2 * time.Minute))
	if _, err := e.signIn(); err != nil {
		t.Fatalf("sign-in with a clock two minutes ahead: %v", err)
	}

	// And two minutes behind Entra's: nbf is two minutes ahead, inside go-oidc's
	// five minute leeway.
	e2 := newEnv(t)
	e2.offset.Store(int64(-2 * time.Minute))
	if _, err := e2.signIn(); err != nil {
		t.Fatalf("sign-in with a clock two minutes behind: %v", err)
	}

	// But not ten minutes behind.
	e3 := newEnv(t)
	e3.offset.Store(int64(-10 * time.Minute))
	_, err := e3.signIn()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Reason != "not_yet_valid" {
		t.Fatalf("error = %v, want not_yet_valid", err)
	}
}

func TestKeyRotation(t *testing.T) {
	e := newEnv(t)
	keys := func() int { return e.fake.Requests(fakeentra.EndpointKeys) }

	if _, err := e.signIn(); err != nil {
		t.Fatal(err)
	}
	if keys() != 1 {
		t.Fatalf("first sign-in fetched the key set %d times, want 1", keys())
	}
	if _, err := e.signIn(); err != nil {
		t.Fatal(err)
	}
	if keys() != 1 {
		t.Errorf("the key set was fetched again (%d) although the key is cached", keys())
	}

	// Entra rotates: a new signing key, the old one gone from the set.
	e.fake.RotateKeys(false)
	if _, err := e.signIn(); err != nil {
		t.Fatalf("sign-in after rotation: %v", err)
	}
	if keys() != 2 {
		t.Errorf("rotation: key set fetched %d times, want 2 (one refresh for the unknown key id)", keys())
	}
	if _, err := e.signIn(); err != nil {
		t.Fatal(err)
	}
	if keys() != 2 {
		t.Errorf("the key set was fetched again (%d) after the refresh", keys())
	}

	// A key id nobody publishes costs one refresh per attempt and no more, and
	// the token is refused.
	e.fake.OnIDToken(func(s *fakeentra.IDTokenSpec) { s.SignWith = e.fake.NewUnpublishedKey() })
	_, err := e.signIn()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Reason != "signature" {
		t.Fatalf("error = %v, want signature", err)
	}
	if keys() != 3 {
		t.Errorf("an unknown key id caused %d fetches in total, want 3", keys())
	}
}

func TestKeyRotation_OldKeyKeptForAWhile(t *testing.T) {
	e := newEnv(t)
	if _, err := e.signIn(); err != nil {
		t.Fatal(err)
	}
	e.fake.RotateKeys(true)
	if _, err := e.signIn(); err != nil {
		t.Fatalf("sign-in with the old key still published: %v", err)
	}
}

func TestNew_Discovery(t *testing.T) {
	t.Run("works", func(t *testing.T) {
		e := newEnv(t)
		if e.fake.Requests(fakeentra.EndpointDiscovery) != 1 {
			t.Errorf("discovery requests = %d", e.fake.Requests(fakeentra.EndpointDiscovery))
		}
	})

	failures := []struct {
		name string
		mod  func(e *env)
		kind string
	}{
		{"server down", func(e *env) { e.fake.SetDown(true) }, "unexpected_status"},
		{"issuer names another tenant", func(e *env) {
			e.fake.MutateDiscovery(func(d map[string]any) { d["issuer"] = e.fake.URL() + "/" + otherTenant + "/v2.0" })
		}, "issuer_mismatch"},
		{"token endpoint on another host", func(e *env) {
			e.fake.MutateDiscovery(func(d map[string]any) { d["token_endpoint"] = "https://attacker.example.test/token" })
		}, "foreign_endpoint"},
		{"authorize endpoint on another host", func(e *env) {
			e.fake.MutateDiscovery(func(d map[string]any) { d["authorization_endpoint"] = "https://attacker.example.test/authorize" })
		}, "foreign_endpoint"},
		{"token endpoint over plain http on the same host", func(e *env) {
			e.fake.MutateDiscovery(func(d map[string]any) {
				d["token_endpoint"] = strings.Replace(d["token_endpoint"].(string), "http://", "https://", 1)
			})
		}, "foreign_endpoint"},
		{"document without an issuer", func(e *env) { e.fake.MutateDiscovery(func(d map[string]any) { delete(d, "issuer") }) }, "issuer_mismatch"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.mod(e)
			_, err := New(context.Background(), e.cfg)
			var de *DiscoveryError
			if !errors.As(err, &de) {
				t.Fatalf("error = %v, want a DiscoveryError", err)
			}
			if de.Kind != tc.kind {
				t.Errorf("kind = %q, want %q (underlying: %v)", de.Kind, tc.kind, errors.Unwrap(de))
			}
			if strings.Contains(err.Error(), testTenant) || strings.Contains(err.Error(), "attacker") {
				t.Errorf("the message quotes the network: %v", err)
			}
		})
	}

	t.Run("tenant that does not exist", func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg
		cfg.TenantID = otherTenant
		_, err := New(context.Background(), cfg)
		var de *DiscoveryError
		if !errors.As(err, &de) || de.Kind != "unexpected_status" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Close()
		_, err := New(context.Background(), e.cfg)
		if got := ErrorKind(err); got != "discovery:unreachable/connection" {
			t.Errorf("ErrorKind = %q (%v)", got, err)
		}
	})

	t.Run("slow", func(t *testing.T) {
		e := newEnv(t)
		e.fake.SetDelay(300 * time.Millisecond)
		cfg := e.cfg
		// The default timeout is generous; a client given a short one fails fast.
		cfg.HTTPClient = shortTimeoutClient(100 * time.Millisecond)
		_, err := New(context.Background(), cfg)
		if got := ErrorKind(err); got != "discovery:unreachable/timeout" {
			t.Errorf("ErrorKind = %q (%v)", got, err)
		}
	})
}

// TestKeyFetchesSurviveTheCallersContext guards the reason New hands go-oidc a
// context without the caller's cancellation: the provider keeps it for every
// later key fetch.
func TestKeyFetchesSurviveTheCallersContext(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the caller (a request, a startup step) is long gone

	authURL, f, _ := c.Begin("")
	if _, err := c.Complete(context.Background(), f, e.browse(authURL)); err != nil {
		t.Fatalf("sign-in after the creating context was cancelled: %v", err)
	}
}
