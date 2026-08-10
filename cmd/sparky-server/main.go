// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-server is the central Sparky application. See
// ARCHITECTURE.md Application Lifecycle for the full startup sequence this
// will grow into.
package main

import (
	"log"
	"os"

	"github.com/1kaius1/Sparky/internal/config"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	logger.Printf("configuration loaded (listen_port=%s log_level=%s)", cfg.ListenPort, cfg.LogLevel)

	// The rest of the Application Lifecycle - setup check, database pool,
	// middleware, routes, and the HTTP listener - is not implemented yet.
	logger.Println("sparky-server: startup not yet implemented beyond config validation")
}
