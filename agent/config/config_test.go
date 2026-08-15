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

func TestLoad_EngineBinaryPaths(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_LLAMACPP_BINARY_PATH", "/opt/llama.cpp/llama-server")
	t.Setenv("SPARKY_VLLM_BINARY_PATH", "/opt/venvs/vllm/bin/vllm")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.LlamaCPPBinaryPath != "/opt/llama.cpp/llama-server" {
		t.Errorf("LlamaCPPBinaryPath = %q, want %q", cfg.LlamaCPPBinaryPath, "/opt/llama.cpp/llama-server")
	}
	if cfg.VLLMBinaryPath != "/opt/venvs/vllm/bin/vllm" {
		t.Errorf("VLLMBinaryPath = %q, want %q", cfg.VLLMBinaryPath, "/opt/venvs/vllm/bin/vllm")
	}
}

func TestLoad_EngineBinaryPaths_UnsetByDefault(t *testing.T) {
	setAllRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.LlamaCPPBinaryPath != "" {
		t.Errorf("LlamaCPPBinaryPath = %q, want empty (no default)", cfg.LlamaCPPBinaryPath)
	}
	if cfg.VLLMBinaryPath != "" {
		t.Errorf("VLLMBinaryPath = %q, want empty (no default)", cfg.VLLMBinaryPath)
	}
}

func TestLoad_BareMetalRuntimeBackend_DefaultsModelStoragePath(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_RUNTIME_BACKEND", "bare-metal")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ModelStoragePath != "/opt/sparky/serviceloop/models" {
		t.Errorf("ModelStoragePath = %q, want the bare-metal default %q", cfg.ModelStoragePath, "/opt/sparky/serviceloop/models")
	}
}

func TestLoad_BareMetalRuntimeBackend_ExplicitModelStoragePathNotOverridden(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_RUNTIME_BACKEND", "bare-metal")
	t.Setenv("SPARKY_MODEL_STORAGE_PATH", "/mnt/models")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ModelStoragePath != "/mnt/models" {
		t.Errorf("ModelStoragePath = %q, want the explicitly configured %q", cfg.ModelStoragePath, "/mnt/models")
	}
}

func TestLoad_NonBareMetalRuntimeBackend_NoModelStoragePathDefault(t *testing.T) {
	setAllRequired(t) // SPARKY_RUNTIME_BACKEND=podman

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.ModelStoragePath != "" {
		t.Errorf("ModelStoragePath = %q, want empty - the bare-metal default must not apply to podman", cfg.ModelStoragePath)
	}
}

func TestLoad_BareMetalRuntimeBackend_DefaultsEngineInstallPath(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_RUNTIME_BACKEND", "bare-metal")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.EngineInstallPath != "/opt/sparky/serviceloop/engines" {
		t.Errorf("EngineInstallPath = %q, want the bare-metal default %q", cfg.EngineInstallPath, "/opt/sparky/serviceloop/engines")
	}
}

func TestLoad_BareMetalRuntimeBackend_ExplicitEngineInstallPathNotOverridden(t *testing.T) {
	setAllRequired(t)
	t.Setenv("SPARKY_RUNTIME_BACKEND", "bare-metal")
	t.Setenv("SPARKY_ENGINE_INSTALL_PATH", "/mnt/engines")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.EngineInstallPath != "/mnt/engines" {
		t.Errorf("EngineInstallPath = %q, want the explicitly configured %q", cfg.EngineInstallPath, "/mnt/engines")
	}
}

func TestLoad_NonBareMetalRuntimeBackend_NoEngineInstallPathDefault(t *testing.T) {
	setAllRequired(t) // SPARKY_RUNTIME_BACKEND=podman

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.EngineInstallPath != "" {
		t.Errorf("EngineInstallPath = %q, want empty - the bare-metal default must not apply to podman", cfg.EngineInstallPath)
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
