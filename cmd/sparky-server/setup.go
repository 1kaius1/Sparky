// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
)

// runSetup implements `sparky-server setup` - see ARCHITECTURE.md
// Application Lifecycle and Security Considerations. Whether setup has
// been completed is inferred from whether the break-glass credential has
// been set, rather than a dedicated setup-state table: it is already
// exactly the one piece of database-resident, first-run-relevant state
// this codebase has today, and every other setting is already an
// environment variable validated on every startup - see internal/httpapi's
// setupGate for where this same check gates the running server.
func runSetup(ctx context.Context, cfg *config.Config, logger *log.Logger) {
	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()

	breakGlass := db.NewBreakGlassRepository(pool)

	_, err = breakGlass.Get(ctx)
	switch {
	case errors.Is(err, db.ErrBreakGlassNotSet):
		fmt.Println("Sparky setup")
		fmt.Println("============")
		fmt.Println()
		fmt.Println("This sets the SuperAdmin break-glass password: the recovery credential")
		fmt.Printf("used to log in (POST %s) and promote the first real\n", cfg.BreakGlassLoginPath)
		fmt.Println("Admin when AD/LDAP access isn't available or hasn't been set up yet.")
		fmt.Println("See SCHEMA.md Break-glass credential.")
		fmt.Println()
	case err == nil:
		fmt.Println("Setup has already been run on this database - resetting the SuperAdmin password.")
		fmt.Println()
	default:
		logger.Fatalf("database not ready - has `migrate -path migrations/ -database \"$DATABASE_URL\" up` been run? %v", err)
	}

	if err := promptAndSetSuperAdminPassword(ctx, breakGlass, logger); err != nil {
		logger.Fatalf("setup: %v", err)
	}

	fmt.Println()
	fmt.Println("Setup complete. Start the server, then log in as SuperAdmin at")
	fmt.Printf("POST %s to bootstrap the first Admin - see SCHEMA.md\n", cfg.BreakGlassLoginPath)
	fmt.Println("Users, Elevation rules.")
}
