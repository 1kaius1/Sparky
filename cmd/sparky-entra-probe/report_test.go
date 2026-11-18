// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeValue(t *testing.T) {
	safe := []string{
		"", "pass", "RS256", "2.0", "authority, tenant, v2.0", "id_token_signing_alg_values_supported",
		"token_endpoint_auth_methods_supported", "application/json", "client_secret_post", "x5t#S256"[:3],
		"unexpected answer (HTTP 503)", "validation:audience", "network:connection",
	}
	for _, v := range safe {
		if !safeValue(v) {
			t.Errorf("%q should be safe", v)
		}
	}
	unsafe := map[string]string{
		"a GUID":               "7c2e9a41-3b5d-4f60-9a1e-0d8b6c4f2e13",
		"a GUID in text":       "tenant 7C2E9A41-3B5D-4F60-9A1E-0D8B6C4F2E13 is",
		"a JWT":                "eyJhbGciOiJSUzI1NiJ9.eyJhIjoxfQ.sig",
		"an email address":     "casey@example.test",
		"a URL":                "https://login.example.test/x",
		"a long random string": "Zx9Qm3Vb7Lk2Rt8Wn4Yh6Pd1Sf5Ga0Jc",
		"a base64 blob":        "dGhpcyBpcyBhIHNlY3JldCB2YWx1ZQ==",
		"too long":             strings.Repeat("a", 81),
		"a quote":              `a"b`,
		"a newline":            "a\nb",
		"braces":               "{tenantid}",
	}
	for name, v := range unsafe {
		if safeValue(v) {
			t.Errorf("%s should not be safe: %q", name, v)
		}
	}
}

func TestStepFactsAreFilteredAndCounted(t *testing.T) {
	r := NewReport()
	s := r.Begin("A step")
	s.Fact("shown", "keyword")
	s.Fact("hidden", "7c2e9a41-3b5d-4f60-9a1e-0d8b6c4f2e13")
	s.Set("list", []string{"b", "a", "a", "https://x.example/y", "casey@example.test"})
	s.Pass()

	var out bytes.Buffer
	if err := r.Render(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"shown: keyword", "hidden: [not shown]", "list: a, b (+2 not shown)",
		"Values left out of this report because they did not look safe to show: 3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestShapeShowsOnlyStandardNames(t *testing.T) {
	r := NewReport()
	s := r.Begin("Claims")
	s.Shape("claims", map[string]string{
		"oid": "string", "tid": "string", "xms_edov": "boolean",
		"extension_c4d8e1f26a3b4c9d8e7f1a2b3c4d5e6f_department": "string",
		"acme_internal_flag": "boolean",
	}, claimVocab)
	s.Names("members", []string{"kty", "kid", "d", "private_thing"}, jwkVocab)
	s.Pass()

	var out bytes.Buffer
	if err := r.Render(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "claims: other-1:string, other-2:boolean, oid:string, tid:string, xms_edov:boolean") &&
		!strings.Contains(text, "claims: other-1:boolean, other-2:string, oid:string, tid:string, xms_edov:boolean") {
		t.Errorf("unexpected claims line:\n%s", text)
	}
	if strings.Contains(text, "extension_") || strings.Contains(text, "acme") {
		t.Errorf("a custom name was shown:\n%s", text)
	}
	if !strings.Contains(text, "members: other-1, kid, kty, other-2") || strings.Contains(text, "private_thing") {
		t.Errorf("a name that is not standard was shown, or the order is wrong:\n%s", text)
	}
}

func TestRenderRefusesWhatLooksLikeAnIdentifier(t *testing.T) {
	cases := map[string]func(r *Report, s *Step){
		"a registered value that passes the text filter": func(r *Report, s *Step) {
			r.Watch("canarytenantword")
			s.Fact("note", "canarytenantword")
		},
		"a registered value in another case": func(r *Report, s *Step) {
			r.Watch("CanaryTenantWord")
			s.Fact("note", "canarytenantword")
		},
		"a registered value inside a longer one": func(r *Report, s *Step) {
			r.Watch("canary")
			s.Fact("note", "a canary here")
		},
		"a GUID that got in some other way": func(r *Report, s *Step) {
			s.lines = append(s.lines, "note: 7c2e9a41-3b5d-4f60-9a1e-0d8b6c4f2e13")
		},
		"a token that got in some other way": func(r *Report, s *Step) {
			s.lines = append(s.lines, "note: eyJhbGciOiJSUzI1NiJ9")
		},
		"an address": func(r *Report, s *Step) { s.lines = append(s.lines, "note: casey@example.test") },
		"a URL":      func(r *Report, s *Step) { s.lines = append(s.lines, "note: http://127.0.0.1:1234/x") },
		"a long opaque string": func(r *Report, s *Step) {
			s.lines = append(s.lines, "note: Zx9Qm3Vb7Lk2Rt8Wn4Yh6Pd1Sf5Ga0Jc")
		},
	}
	for name, add := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewReport()
			s := r.Begin("A step")
			add(r, s)
			s.Pass()
			var out bytes.Buffer
			err := r.Render(&out)
			if err == nil {
				t.Fatalf("rendered:\n%s", out.String())
			}
			if out.Len() != 0 {
				t.Error("something was written although the report was refused")
			}
			// The refusal does not say what it matched.
			for _, v := range []string{"canary", "7c2e", "casey", "eyJ", "127.0.0.1"} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the error mentions %q", v)
				}
			}
		})
	}
}

func TestRenderAllowsLongProtocolNames(t *testing.T) {
	r := NewReport()
	s := r.Begin("Discovery")
	s.Shape("members", map[string]string{"token_endpoint_auth_methods_supported": "array", "id_token_signing_alg_values_supported": "array"}, discoveryVocab)
	s.Pass()
	var out bytes.Buffer
	if err := r.Render(&out); err != nil {
		t.Fatalf("a long standard name was refused: %v", err)
	}
}

func TestUnfinishedStepCountsAsFailed(t *testing.T) {
	r := NewReport()
	r.Begin("never finished")
	var out bytes.Buffer
	if err := r.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "result: fail") || r.Overall() {
		t.Errorf("an unfinished step was not treated as a failure:\n%s", out.String())
	}
}

func TestPseudonyms(t *testing.T) {
	p := NewPseudonyms()
	if a, b := p.Label("user", "ABC"), p.Label("user", "abc"); a != b || a != "user-1" {
		t.Errorf("the same value (in another case) got %q and %q", a, b)
	}
	if got := p.Label("user", "def"); got != "user-2" {
		t.Errorf("second user = %q", got)
	}
	if got := p.Label("tenant", "abc"); got != "tenant-A" {
		t.Errorf("a tenant with the same text as a user = %q", got)
	}
	if got := p.Label("tenant", "xyz"); got != "tenant-B" {
		t.Errorf("second tenant = %q", got)
	}
	// Labels carry no trace of the value.
	for _, l := range []string{p.Label("user", "secret-looking-value"), p.Label("tenant", "secret-looking-value")} {
		if strings.Contains(l, "secret") {
			t.Errorf("label %q contains part of the value", l)
		}
	}
	// A new table starts again: labels cannot be matched between runs.
	if got := NewPseudonyms().Label("user", "def"); got != "user-1" {
		t.Errorf("a new run labelled the same value %q", got)
	}
}

func TestRun_CommandLine(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := map[string]string{"ENTRA_TENANT_ID": cTenant, "ENTRA_CLIENT_ID": cClient, "ENTRA_CLIENT_SECRET": cSecret}

	t.Run("help describes the setup and exits cleanly", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-h"}, env(nil), &stdout, &stderr); code != 0 {
			t.Errorf("exit code = %d", code)
		}
		for _, want := range []string{"http://localhost/sparky-probe/callback", "ENTRA_CLIENT_SECRET", "sign-in logs"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("the help does not mention %q", want)
			}
		}
	})

	t.Run("missing settings are a configuration problem that names no value", func(t *testing.T) {
		for _, m := range []map[string]string{
			nil,
			{"ENTRA_TENANT_ID": cTenant},
			{"ENTRA_TENANT_ID": cTenant, "ENTRA_CLIENT_ID": cClient},
			{"ENTRA_TENANT_ID": "contoso.onmicrosoft.com", "ENTRA_CLIENT_ID": cClient, "ENTRA_CLIENT_SECRET": cSecret},
		} {
			var stdout, stderr bytes.Buffer
			if code := run(nil, env(m), &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d for %v", code, m)
			}
			if stdout.Len() != 0 {
				t.Error("something was written to the report output")
			}
			for _, v := range []string{cSecret, "contoso"} {
				if strings.Contains(stderr.String(), v) {
					t.Errorf("the message contains %q", v)
				}
			}
		}
	})

	t.Run("the secret is not accepted as a flag", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-client-secret", cSecret}, env(good), &stdout, &stderr); code != 2 {
			t.Errorf("exit code = %d", code)
		}
	})

	t.Run("a bad port is refused", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-listen-port", "70000"}, env(good), &stdout, &stderr); code != 2 {
			t.Errorf("exit code = %d", code)
		}
	})

	t.Run("a run against the fake tenant writes the report to standard output", func(t *testing.T) {
		e := newProbeEnv(t)
		m := map[string]string{"ENTRA_TENANT_ID": cTenant, "ENTRA_CLIENT_ID": cClient, "ENTRA_CLIENT_SECRET": cSecret, "ENTRA_AUTHORITY_URL": e.fake.URL()}
		var stdout, stderr bytes.Buffer
		code := run([]string{"-skip-signin"}, env(m), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
		}
		for _, want := range []string{"Sparky Entra ID probe report", "Probe version: 1", "Summary: 3 passed, 0 failed, 2 skipped", "Read it before you share it."} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("the report does not contain %q", want)
			}
		}
		assertNoLeak(t, "the report", stdout.String(), e.canaries())
		assertNoLeak(t, "the console", stderr.String(), e.canaries())
	})

	t.Run("a failed check gives exit code 1", func(t *testing.T) {
		e := newProbeEnv(t)
		m := map[string]string{"ENTRA_TENANT_ID": cTenant, "ENTRA_CLIENT_ID": cClient, "ENTRA_CLIENT_SECRET": "wrong", "ENTRA_AUTHORITY_URL": e.fake.URL()}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-skip-signin"}, env(m), &stdout, &stderr); code != 1 {
			t.Errorf("exit code = %d", code)
		}
		if !strings.Contains(stdout.String(), "token_endpoint:invalid_client") {
			t.Errorf("the failure is not in the report:\n%s", stdout.String())
		}
	})

	t.Run("the report file is new, private, and never overwritten", func(t *testing.T) {
		e := newProbeEnv(t)
		m := map[string]string{"ENTRA_TENANT_ID": cTenant, "ENTRA_CLIENT_ID": cClient, "ENTRA_CLIENT_SECRET": cSecret, "ENTRA_AUTHORITY_URL": e.fake.URL()}
		path := filepath.Join(t.TempDir(), "report.txt")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-skip-signin", "-report", path}, env(m), &stdout, &stderr); code != 0 {
			t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Error("the report was also written to standard output")
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
		first, _ := os.ReadFile(path)

		stderr.Reset()
		if code := run([]string{"-skip-signin", "-report", path}, env(m), &stdout, &stderr); code != 2 {
			t.Errorf("overwriting: exit code = %d, want 2", code)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(first, after) {
			t.Error("the existing file was changed")
		}
	})
}
