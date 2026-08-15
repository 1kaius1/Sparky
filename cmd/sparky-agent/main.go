// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-agent runs on every compute node. See docs/AGENT.md for the
// full lifecycle this will grow into.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/1kaius1/Sparky/agent/config"
	"github.com/1kaius1/Sparky/agent/connection"
	"github.com/1kaius1/Sparky/agent/enginetransfer"
	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/agent/runtime/baremetal"
	"github.com/1kaius1/Sparky/agent/runtime/containers"
	"github.com/1kaius1/Sparky/agent/telemetry"
	"github.com/1kaius1/Sparky/agent/transfer"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// Dispatched before config.Load() - unlike sparky-server's subcommands,
	// setup needs none of the normal agent env vars (SPARKY_CENTRAL_URL,
	// SPARKY_BEARER_TOKEN, ...), and requiring them to already be valid
	// would be circular: setup is meant to run before an operator
	// necessarily has a bearer token to put in secrets.env yet.
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		runSetup(logger)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	logger.Printf("configuration loaded (node_name=%s runtime_backend=%s)", cfg.NodeName, cfg.RuntimeBackend)

	telemetryPollInterval, err := time.ParseDuration(cfg.TelemetryPollInterval)
	if err != nil {
		logger.Fatalf("config: invalid SPARKY_TELEMETRY_POLL_INTERVAL %q: %v", cfg.TelemetryPollInterval, err)
	}

	// Picked once at startup, per the node's configured runtime_backend
	// (SCHEMA.md Nodes) - agent/connection never branches on which
	// concrete implementation it has (agent/runtime.Backend).
	var runtimeBackend runtime.Backend
	switch cfg.RuntimeBackend {
	case "docker", "podman":
		c, err := containers.New()
		if err != nil {
			logger.Fatalf("runtime backend: %v", err)
		}
		defer c.Close()
		runtimeBackend = c
	case "bare-metal":
		runtimeBackend = baremetal.New()
	default:
		logger.Fatalf("config: unknown SPARKY_RUNTIME_BACKEND %q (want docker, podman, or bare-metal)", cfg.RuntimeBackend)
	}

	conn := connection.New(connection.Config{
		CentralURL:  cfg.CentralURL,
		BearerToken: cfg.BearerToken,
		NodeName:    cfg.NodeName,
		EngineBinaryPaths: map[string]string{
			"llamacpp": cfg.LlamaCPPBinaryPath,
			"vllm":     cfg.VLLMBinaryPath,
		},
		ModelStoragePath:      cfg.ModelStoragePath,
		EngineInstallPath:     cfg.EngineInstallPath,
		TelemetryPollInterval: telemetryPollInterval,
	}, runtimeBackend, transfer.New(), enginetransfer.New(), telemetry.NewCollector(), logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Println("sparky-agent: starting")
	conn.Run(ctx)
	logger.Println("sparky-agent: stopped")
}
