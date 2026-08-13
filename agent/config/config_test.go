// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

var requiredVars = []string{
	"SPARKY_CENTRAL_URL",
	"SPARKY_BEARER_TOKEN",
	"SPARKY_NODE_NAME",
	"SPARKY_RUNTIME_BACKEND",
}

func setAllRequired(t *testing.T) {
	t.Helper()
	t.Setenv("SPARKY_CENTRAL_URL", "wss://central.example.internal/agent")
	t.Setenv("SPARKY_BEARER_TOKEN", "token")
	t.Setenv("SPARKY_NODE_NAME", "spark-1")
	t.Setenv("SPARKY_RUNTIME_BACKEND", "podman")
}

func TestLoad_AllRequiredPresent_Succeeds(t *testing.T) {
	setAllRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.NodeName != "spark-1" {
		t.Errorf("NodeName = %q, want %q", cfg.NodeName, "spark-1")
	}
	if cfg.RuntimeBackend != "podman" {
		t.Errorf("RuntimeBackend = %q, want %q", cfg.RuntimeBackend, "podman")
	}
}

func TestLoad_DefaultsApply(t *testing.T) {
	setAllRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.TelemetryPollInterval != "5s" {
		t.Errorf("TelemetryPollInterval = %q, want default %q", cfg.TelemetryPollInterval, "5s")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want default %q", cfg.LogFormat, "json")
	}
}

func TestLoad_OverridesDefaults(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_MODEL_STORAGE_PATH", "/mnt/models")
	t.Setenv("SPARKY_TELEMETRY_POLL_INTERVAL", "10s")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ModelStoragePath != "/mnt/models" {
		t.Errorf("ModelStoragePath = %q, want %q", cfg.ModelStoragePath, "/mnt/models")
	}
	if cfg.TelemetryPollInterval != "10s" {
		t.Errorf("TelemetryPollInterval = %q, want %q", cfg.TelemetryPollInterval, "10s")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "text")
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
