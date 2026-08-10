// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-server is the central Sparky application. See
// ARCHITECTURE.md Application Lifecycle for the full startup sequence this
// will grow into.
package main

import (
	"context"
	"log"
	"os"

	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	logger.Printf("configuration loaded (listen_port=%s log_level=%s)", cfg.ListenPort, cfg.LogLevel)

	ctx := context.Background()

	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()

	logger.Println("database connection pool established")

	// The rest of the Application Lifecycle - setup check, middleware,
	// routes, and the HTTP listener - is not implemented yet.
	logger.Println("sparky-server: startup not yet implemented beyond config validation and database connectivity")
}
