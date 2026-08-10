// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-agent runs on every compute node. See docs/AGENT.md for the
// full lifecycle this will grow into.
package main

import (
	"log"
	"os"

	"github.com/1kaius1/Sparky/agent/config"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	logger.Printf("configuration loaded (node_name=%s node_type=%s)", cfg.NodeName, cfg.NodeType)

	// Runtime backend init, telemetry collector, and the WebSocket dial to
	// the central app are not implemented yet.
	logger.Println("sparky-agent: startup not yet implemented beyond config validation")
}
