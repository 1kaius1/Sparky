// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
)

// defaultAdminLocalUsername/defaultAdminDisplayName are the default local
// account `setup` bootstraps - see SCHEMA.md Users' Local-only accounts
// subsection. This is what makes the break-glass credential a true
// recovery-only mechanism after this feature lands: a real, RBAC-tiered
// Admin account exists from the very first `setup` run, so break-glass
// never needs to be the account anyone signs in with day to day.
const (
	defaultAdminLocalUsername = "admin"
	defaultAdminDisplayName   = "Admin"
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

	users := db.NewUserRepository(pool)
	if err := bootstrapDefaultAdminAccount(ctx, users, logger); err != nil {
		logger.Fatalf("setup: %v", err)
	}

	fmt.Println()
	fmt.Println("Setup complete. Start the server, then sign in with the local")
	fmt.Println("account shown above (or as SuperAdmin via break-glass, POST")
	fmt.Printf("%s) - see SCHEMA.md Users, Elevation rules.\n", cfg.BreakGlassLoginPath)
}

// bootstrapDefaultAdminAccount creates the default local "admin" account
// (tier Admin, display name "Admin") if it doesn't already exist -
// idempotent, matching sparky-agent setup's own "safe to re-run" precedent.
// An already-existing admin account is left completely untouched, password
// included: this must never reset a password an Admin has since changed,
// unlike promptAndSetSuperAdminPassword's own always-overwrites behavior
// for the break-glass credential (that command is explicitly a deliberate,
// interactive reset; a repeated `setup` run is not). The generated password
// is printed once in plaintext - the only time it is ever shown - mirroring
// RegisterNode's own "shown here only once" precedent for a node's bearer
// token.
func bootstrapDefaultAdminAccount(ctx context.Context, users *db.UserRepository, logger *log.Logger) error {
	_, _, err := users.FindByLocalUsername(ctx, defaultAdminLocalUsername)
	switch {
	case err == nil:
		fmt.Println()
		fmt.Println("Default local \"admin\" account already exists - leaving its password unchanged.")
		return nil
	case !errors.Is(err, db.ErrUserNotFound):
		return fmt.Errorf("look up default admin account: %w", err)
	}

	password, err := auth.GenerateInitialPassword()
	if err != nil {
		return fmt.Errorf("generate initial password: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash initial password: %w", err)
	}
	if _, err := users.CreateLocal(ctx, defaultAdminLocalUsername, hash, defaultAdminDisplayName, db.TierAdmin); err != nil {
		return fmt.Errorf("create default admin account: %w", err)
	}

	fmt.Println()
	fmt.Println("Default local admin account created:")
	fmt.Printf("  Username: %s\n", defaultAdminLocalUsername)
	fmt.Printf("  Password: %s\n", password)
	fmt.Println("This password is shown only now - sign in and change it (see /account).")
	return nil
}
