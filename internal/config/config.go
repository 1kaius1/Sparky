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

	// LDAPConfigured is true only when every LDAP_* variable below is set -
	// see Load's own doc comment. When false, AD login is unavailable and
	// local-only accounts (SCHEMA.md Users' Local-only accounts subsection)
	// are the only way to sign in.
	LDAPConfigured bool

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
	AuthRecheckIntervalSecs  int
}

// required maps each mandatory environment variable to the Config field it
// populates, so a missing value can be reported by its actual variable name.
type required struct {
	envVar string
	dest   *string
}

// ldapEnvVars are the five LDAP_* variables treated as a single all-or-
// nothing group - see Load's own doc comment.
var ldapEnvVars = []string{
	"LDAP_SERVER_ADDR",
	"LDAP_BIND_DN",
	"LDAP_BIND_PASSWORD",
	"LDAP_BASE_DN",
	"LDAP_ACCESS_GROUP_DN",
}

// Load reads and validates configuration from the environment, failing fast
// if anything required is missing - see ARCHITECTURE.md Application
// Lifecycle, Config / Env Validation.
//
// The LDAP_* variables are validated as a single all-or-nothing group, not
// individually required: either every one of them is set (AD login is
// available, alongside local-only accounts) or none of them are (AD login
// is unavailable - local-only accounts are the only way to sign in, see
// SCHEMA.md Users' Local-only accounts subsection). A partial set stays a
// hard error - that's almost certainly a real misconfiguration, not a
// deliberate choice, same posture Load already takes toward any other
// missing required variable.
func Load() (*Config, error) {
	authRateLimitMaxAttempts, err := getEnvDefaultInt("AUTH_RATE_LIMIT_MAX_ATTEMPTS", 10)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_RATE_LIMIT_MAX_ATTEMPTS: %w", err)
	}
	authRateLimitWindowSecs, err := getEnvDefaultInt("AUTH_RATE_LIMIT_WINDOW_SECONDS", 300)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_RATE_LIMIT_WINDOW_SECONDS: %w", err)
	}
	authRecheckIntervalSecs, err := getEnvDefaultInt("AUTH_RECHECK_INTERVAL_SECONDS", 3600)
	if err != nil {
		return nil, fmt.Errorf("parse AUTH_RECHECK_INTERVAL_SECONDS: %w", err)
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
		AuthRecheckIntervalSecs:  authRecheckIntervalSecs,
	}

	fields := []required{
		{"DATABASE_URL", &cfg.DatabaseURL},
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

	ldapFields := []required{
		{"LDAP_SERVER_ADDR", &cfg.LDAPServerAddr},
		{"LDAP_BIND_DN", &cfg.LDAPBindDN},
		{"LDAP_BIND_PASSWORD", &cfg.LDAPBindPassword},
		{"LDAP_BASE_DN", &cfg.LDAPBaseDN},
		{"LDAP_ACCESS_GROUP_DN", &cfg.LDAPAccessGroupDN},
	}
	var ldapSet, ldapUnset []string
	for _, f := range ldapFields {
		v := os.Getenv(f.envVar)
		if v == "" {
			ldapUnset = append(ldapUnset, f.envVar)
			continue
		}
		ldapSet = append(ldapSet, f.envVar)
		*f.dest = v
	}
	switch {
	case len(ldapUnset) == 0:
		cfg.LDAPConfigured = true
	case len(ldapSet) == 0:
		cfg.LDAPConfigured = false
	default:
		return nil, fmt.Errorf("partial LDAP configuration: %v are set but %v are missing - set all five LDAP_* variables to enable AD login, or none to use local-only accounts only", ldapSet, ldapUnset)
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
