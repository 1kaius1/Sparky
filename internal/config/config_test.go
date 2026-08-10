// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

var requiredVars = []string{
	"DATABASE_URL",
	"LDAP_SERVER_ADDR",
	"LDAP_BIND_DN",
	"LDAP_BIND_PASSWORD",
	"LDAP_BASE_DN",
	"LDAP_ACCESS_GROUP_DN",
	"SESSION_SECRET",
}

func setAllRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LDAP_SERVER_ADDR", "ldap://x")
	t.Setenv("LDAP_BIND_DN", "cn=svc-sparky,dc=example")
	t.Setenv("LDAP_BIND_PASSWORD", "secret")
	t.Setenv("LDAP_BASE_DN", "dc=example")
	t.Setenv("LDAP_ACCESS_GROUP_DN", "cn=sparky-access,dc=example")
	t.Setenv("SESSION_SECRET", "sekrit")
}

func TestLoad_AllRequiredPresent_Succeeds(t *testing.T) {
	setAllRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.DatabaseURL != "postgres://x" {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, "postgres://x")
	}
	if cfg.SessionSecret != "sekrit" {
		t.Errorf("SessionSecret = %q, want %q", cfg.SessionSecret, "sekrit")
	}
}

func TestLoad_DefaultsApply(t *testing.T) {
	setAllRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ListenPort != "8080" {
		t.Errorf("ListenPort = %q, want default %q", cfg.ListenPort, "8080")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want default %q", cfg.LogFormat, "text")
	}
	if cfg.AuditForwardEnabled {
		t.Errorf("AuditForwardEnabled = true, want false by default")
	}
}

func TestLoad_OverridesDefaults(t *testing.T) {
	setAllRequired(t)
	t.Setenv("LISTEN_PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("AUDIT_FORWARD_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ListenPort != "9090" {
		t.Errorf("ListenPort = %q, want %q", cfg.ListenPort, "9090")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
	if !cfg.AuditForwardEnabled {
		t.Errorf("AuditForwardEnabled = false, want true")
	}
}

func TestLoad_MissingRequired_ReturnsError(t *testing.T) {
	for _, missing := range requiredVars {
		t.Run(missing, func(t *testing.T) {
			setAllRequired(t)
			t.Setenv(missing, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded despite missing %s, want an error", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("Load() error = %q, want it to mention %s", err.Error(), missing)
			}
		})
	}
}

func TestLoad_MissingAllRequired_ListsEveryVar(t *testing.T) {
	for _, v := range requiredVars {
		t.Setenv(v, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with no environment set, want an error")
	}
	for _, v := range requiredVars {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("Load() error = %q, want it to mention %s", err.Error(), v)
		}
	}
}
