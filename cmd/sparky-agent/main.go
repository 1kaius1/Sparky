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
	"github.com/1kaius1/Sparky/agent/runtime/containers"
	"github.com/1kaius1/Sparky/agent/telemetry"
	"github.com/1kaius1/Sparky/agent/transfer"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	logger.Printf("configuration loaded (node_name=%s runtime_backend=%s)", cfg.NodeName, cfg.RuntimeBackend)

	telemetryPollInterval, err := time.ParseDuration(cfg.TelemetryPollInterval)
	if err != nil {
		logger.Fatalf("config: invalid SPARKY_TELEMETRY_POLL_INTERVAL %q: %v", cfg.TelemetryPollInterval, err)
	}

	// Bare-metal runtime backend selection (for hosts without GPU
	// passthrough) is v0.2.0 work (CLAUDE.md Current Focus) - only
	// Docker/Podman exists today.
	runtimeBackend, err := containers.New()
	if err != nil {
		logger.Fatalf("runtime backend: %v", err)
	}
	defer runtimeBackend.Close()

	conn := connection.New(connection.Config{
		CentralURL:            cfg.CentralURL,
		BearerToken:           cfg.BearerToken,
		NodeName:              cfg.NodeName,
		ModelStoragePath:      cfg.ModelStoragePath,
		TelemetryPollInterval: telemetryPollInterval,
	}, runtimeBackend, transfer.New(), telemetry.NewCollector(), logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Println("sparky-agent: starting")
	conn.Run(ctx)
	logger.Println("sparky-agent: stopped")
}
