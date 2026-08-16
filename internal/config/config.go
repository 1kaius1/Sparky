// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads sparky-server's configuration from environment
// variables. It is the only package that reads the environment directly -
// see ARCHITECTURE.md Configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds sparky-server's validated environment configuration. See
// CLAUDE.md Configuration and Environment Variables for the full reference.
type Config struct {
	DatabaseURL string

	LDAPServerAddr    string
	LDAPBindDN        string
	LDAPBindPassword  string
	LDAPBaseDN        string
	LDAPAccessGroupDN string

	SessionSecret string

	ListenPort               string
	LogLevel                 string
	LogFormat                string
	AuditForwardEnabled      bool
	BreakGlassAllowedIPs     string
	BreakGlassLoginPath      string
	AuthRateLimitMaxAttempts int
	AuthRateLimitWindowSecs  int
}

// required maps each mandatory environment variable to the Config field it
// populates, so a missing value can be reported by its actual variable name.
type required struct {
	envVar string
	dest   *string
}

// Load reads and validates configuration from the environment, failing fast
// if anything required is missing - see ARCHITECTURE.md Application
// Lifecycle, Config / Env Validation.
func Load() (*Config, error) {
	authRateLimitMaxAttempts, err := getEnvDefaultInt("AUTH_RATE_LIMIT_MAX_ATTEMPTS", 10)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_RATE_LIMIT_MAX_ATTEMPTS: %w", err)
	}
	authRateLimitWindowSecs, err := getEnvDefaultInt("AUTH_RATE_LIMIT_WINDOW_SECONDS", 300)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_RATE_LIMIT_WINDOW_SECONDS: %w", err)
	}
	breakGlassLoginPath := getEnvDefault("BREAKGLASS_LOGIN_PATH", "/login/break-glass")
	if !strings.HasPrefix(breakGlassLoginPath, "/") {
		return nil, fmt.Errorf("parse BREAKGLASS_LOGIN_PATH: must start with \"/\", got %q", breakGlassLoginPath)
	}

	cfg := &Config{
		ListenPort:               getEnvDefault("LISTEN_PORT", "8080"),
		LogLevel:                 getEnvDefault("LOG_LEVEL", "info"),
		LogFormat:                getEnvDefault("LOG_FORMAT", "text"),
		AuditForwardEnabled:      os.Getenv("AUDIT_FORWARD_ENABLED") == "true",
		BreakGlassAllowedIPs:     getEnvDefault("BREAKGLASS_ALLOWED_IPS", ""),
		BreakGlassLoginPath:      breakGlassLoginPath,
		AuthRateLimitMaxAttempts: authRateLimitMaxAttempts,
		AuthRateLimitWindowSecs:  authRateLimitWindowSecs,
	}

	fields := []required{
		{"DATABASE_URL", &cfg.DatabaseURL},
		{"LDAP_SERVER_ADDR", &cfg.LDAPServerAddr},
		{"LDAP_BIND_DN", &cfg.LDAPBindDN},
		{"LDAP_BIND_PASSWORD", &cfg.LDAPBindPassword},
		{"LDAP_BASE_DN", &cfg.LDAPBaseDN},
		{"LDAP_ACCESS_GROUP_DN", &cfg.LDAPAccessGroupDN},
		{"SESSION_SECRET", &cfg.SessionSecret},
	}

	var missing []string
	for _, f := range fields {
		v := os.Getenv(f.envVar)
		if v == "" {
			missing = append(missing, f.envVar)
			continue
		}
		*f.dest = v
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variable(s): %v", missing)
	}

	return cfg, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvDefaultInt reads key as an integer, falling back to fallback if
// unset. Returns an error on a set-but-malformed value - a startup-time
// config mistake should fail fast, same philosophy as Load's own missing-
// required-variable check and newBreakGlassIPWhitelist's malformed-entry
// handling.
func getEnvDefaultInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, v)
	}
	return n, nil
}
