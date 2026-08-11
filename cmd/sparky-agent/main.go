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

	"github.com/1kaius1/Sparky/agent/config"
	"github.com/1kaius1/Sparky/agent/connection"
	"github.com/1kaius1/Sparky/agent/runtime/containers"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	logger.Printf("configuration loaded (node_name=%s node_type=%s)", cfg.NodeName, cfg.NodeType)

	// Bare-metal (Spark) runtime backend selection is v0.2.0 work
	// (CLAUDE.md Current Focus) - only Docker/Podman exists today.
	runtimeBackend, err := containers.New()
	if err != nil {
		logger.Fatalf("runtime backend: %v", err)
	}
	defer runtimeBackend.Close()

	conn := connection.New(connection.Config{
		CentralURL:  cfg.CentralURL,
		BearerToken: cfg.BearerToken,
		NodeName:    cfg.NodeName,
	}, runtimeBackend, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Println("sparky-agent: starting")
	conn.Run(ctx)
	logger.Println("sparky-agent: stopped")
}
