// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads sparky-agent's configuration from environment
// variables - see docs/AGENT.md Configuration and Data Storage.
package config

import (
	"fmt"
	"os"
	"strconv"
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

	// InstanceStartupTimeoutSecs is how long a load_instance's readiness
	// check (agent/connection.Conn.waitForReady) waits for a newly started
	// instance to prove it can genuinely serve before giving up and
	// reporting the load itself as failed -
	// SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS, optional, defaulting to 600
	// (10 minutes) - see agent/connection's defaultInstanceStartupTimeout
	// for why that default is generous. A non-positive value (here or via
	// this env var) falls back to that same package-level default rather
	// than disabling the check entirely - unlike TelemetryPollInterval,
	// there is no sensible "off" for a load's own success/failure
	// determination.
	InstanceStartupTimeoutSecs int

	// HealthCheckIntervalSecs is how often the periodic instance
	// health-check goroutine (agent/connection.Conn.sendInstanceHealth)
	// re-checks every instance this agent has confirmed running -
	// SPARKY_HEALTH_CHECK_INTERVAL_SECONDS, optional, defaulting to 60.
	HealthCheckIntervalSecs int

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

	// EngineInstallPath is the root directory agent/enginetransfer installs
	// provisioned compiled-engine binaries into - SPARKY_ENGINE_INSTALL_PATH,
	// optional, defaulting to bareMetalDefaultEngineInstallPath on a
	// bare-metal node the same way ModelStoragePath does. Distinct from
	// LlamaCPPBinaryPath/VLLMBinaryPath: those still point at one specific
	// executable an operator configures once
	// ($SPARKY_ENGINE_INSTALL_PATH/<engine_type>/latest/<binary>), while this
	// is the directory a provisioning run manages underneath - see
	// PLANNING.md's 2026-08-15 Decisions Log entry. Meaningless for
	// docker/podman nodes, which get engine software via container images,
	// not this mechanism.
	EngineInstallPath string
}

type required struct {
	envVar string
	dest   *string
}

// bareMetalDefaultModelStoragePath is SPARKY_MODEL_STORAGE_PATH's default
// when RuntimeBackend is "bare-metal" and the variable is unset - see
// docs/AGENT.md Configuration and Install (bare metal), which already
// documents this as the bare-metal default (the serviceloop account's own
// home directory, created by scripts/packaging/lib/agent-common.sh's
// ensure_model_storage_dir). Deliberately under /opt/sparky rather than
// /home - the systemd unit's ProtectHome=true makes /home/* inaccessible to
// the running process, so a path under /home would be unreachable
// regardless of whether it existed. No such default exists for
// docker/podman, where the value is always operator-configured to wherever
// the container mount should point.
const bareMetalDefaultModelStoragePath = "/opt/sparky/serviceloop/models"

// bareMetalDefaultEngineInstallPath is SPARKY_ENGINE_INSTALL_PATH's default
// when RuntimeBackend is "bare-metal" and the variable is unset - a sibling
// of bareMetalDefaultModelStoragePath under the same serviceloop-owned
// /opt/sparky/serviceloop tree, for the same ProtectHome=true reasoning.
const bareMetalDefaultEngineInstallPath = "/opt/sparky/serviceloop/engines"

// defaultInstanceStartupTimeoutSecs is SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS'
// own default (600s = 10 minutes) - deliberately the same value as
// agent/connection's defaultInstanceStartupTimeout, kept as a separate
// duplicated constant rather than an import: agent/config has no
// dependency on agent/connection (the reverse dependency already exists),
// matching this repo's existing precedent for small duplicated constants
// across package boundaries (e.g. agent/connection's own vllmDefaultImage).
const defaultInstanceStartupTimeoutSecs = 600

// defaultHealthCheckIntervalSecs is SPARKY_HEALTH_CHECK_INTERVAL_SECONDS'
// own default - the "once a minute" cadence confirmed directly with the
// user.
const defaultHealthCheckIntervalSecs = 60

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
		EngineInstallPath:     os.Getenv("SPARKY_ENGINE_INSTALL_PATH"),
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
	if cfg.EngineInstallPath == "" && cfg.RuntimeBackend == "bare-metal" {
		cfg.EngineInstallPath = bareMetalDefaultEngineInstallPath
	}

	instanceStartupTimeoutSecs, err := getEnvDefaultInt("SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS", defaultInstanceStartupTimeoutSecs)
	if err != nil {
		return nil, fmt.Errorf("parse SPARKY_INSTANCE_STARTUP_TIMEOUT_SECONDS: %w", err)
	}
	cfg.InstanceStartupTimeoutSecs = instanceStartupTimeoutSecs

	healthCheckIntervalSecs, err := getEnvDefaultInt("SPARKY_HEALTH_CHECK_INTERVAL_SECONDS", defaultHealthCheckIntervalSecs)
	if err != nil {
		return nil, fmt.Errorf("parse SPARKY_HEALTH_CHECK_INTERVAL_SECONDS: %w", err)
	}
	cfg.HealthCheckIntervalSecs = healthCheckIntervalSecs

	return cfg, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvDefaultInt mirrors internal/config's own identically-named helper
// (server-side) - parses key as an integer if set, or returns fallback if
// unset. Kept as a separate copy rather than a shared helper package: this
// is the only integer-valued env var either binary's agent-side config
// needs today, not worth a new shared dependency for one function.
func getEnvDefaultInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return strconv.Atoi(v)
}
