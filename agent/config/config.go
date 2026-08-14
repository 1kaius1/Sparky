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

	// LlamaCPPBinaryPath / VLLMBinaryPath are the local executables to run
	// for a load_instance of that engine type on a bare-metal runtime
	// backend node - SPARKY_LLAMACPP_BINARY_PATH / SPARKY_VLLM_BINARY_PATH,
	// optional with no default (a binary's on-disk location is inherently
	// host-specific, so there is nothing sensible to default to). Absence
	// is a valid, meaningful state - "this node doesn't run that engine
	// type" - not an error; a load_instance for an unconfigured engine
	// type fails clearly via agent/runtime/baremetal.Backend.Start's own
	// error instead. Unused by docker/podman nodes.
	LlamaCPPBinaryPath string
	VLLMBinaryPath     string
}

type required struct {
	envVar string
	dest   *string
}

// bareMetalDefaultModelStoragePath is SPARKY_MODEL_STORAGE_PATH's default
// when RuntimeBackend is "bare-metal" and the variable is unset - see
// docs/AGENT.md Configuration and Install (bare metal), which already
// documents this as the bare-metal default (the serviceloop account's own
// home directory). No such default exists for docker/podman, where the
// value is always operator-configured to wherever the container mount
// should point.
const bareMetalDefaultModelStoragePath = "/home/serviceloop/models"

// Load reads and validates configuration from the environment, failing fast
// if anything required is missing.
func Load() (*Config, error) {
	cfg := &Config{
		ModelStoragePath:      os.Getenv("SPARKY_MODEL_STORAGE_PATH"),
		TelemetryPollInterval: getEnvDefault("SPARKY_TELEMETRY_POLL_INTERVAL", "5s"),
		LogLevel:              getEnvDefault("LOG_LEVEL", "info"),
		LogFormat:             getEnvDefault("LOG_FORMAT", "json"),
		LlamaCPPBinaryPath:    os.Getenv("SPARKY_LLAMACPP_BINARY_PATH"),
		VLLMBinaryPath:        os.Getenv("SPARKY_VLLM_BINARY_PATH"),
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

	if cfg.ModelStoragePath == "" && cfg.RuntimeBackend == "bare-metal" {
		cfg.ModelStoragePath = bareMetalDefaultModelStoragePath
	}

	return cfg, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
