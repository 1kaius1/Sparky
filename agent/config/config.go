// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads sparky-agent's configuration from environment
// variables - see docs/AGENT.md Configuration and Data Storage.
package config

import (
	"fmt"
	"os"
)

// Config holds sparky-agent's validated environment configuration.
type Config struct {
	CentralURL     string
	BearerToken    string
	NodeName       string
	RuntimeBackend string

	ModelStoragePath      string
	TelemetryPollInterval string
	LogLevel              string
	LogFormat             string
}

type required struct {
	envVar string
	dest   *string
}

// Load reads and validates configuration from the environment, failing fast
// if anything required is missing.
func Load() (*Config, error) {
	cfg := &Config{
		ModelStoragePath:      os.Getenv("SPARKY_MODEL_STORAGE_PATH"),
		TelemetryPollInterval: getEnvDefault("SPARKY_TELEMETRY_POLL_INTERVAL", "5s"),
		LogLevel:              getEnvDefault("LOG_LEVEL", "info"),
		LogFormat:             getEnvDefault("LOG_FORMAT", "json"),
	}

	fields := []required{
		{"SPARKY_CENTRAL_URL", &cfg.CentralURL},
		{"SPARKY_BEARER_TOKEN", &cfg.BearerToken},
		{"SPARKY_NODE_NAME", &cfg.NodeName},
		{"SPARKY_RUNTIME_BACKEND", &cfg.RuntimeBackend},
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
