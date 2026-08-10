// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"golang.org/x/term"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
)

// minSuperAdminPasswordLength is a basic sanity floor, not a full password
// policy - this credential is set interactively by an operator with shell
// access to the host, not through any user-facing form.
const minSuperAdminPasswordLength = 12

// runSetSuperAdminPassword implements `sparky-server set-superadmin-password`
// - see SCHEMA.md Break-glass credential and ARCHITECTURE.md Security
// Considerations. Always overwrites unconditionally: shell access to run
// this command already implies enough trust to reset the credential, which
// is the point of a break-glass mechanism.
func runSetSuperAdminPassword(ctx context.Context, cfg *config.Config, logger *log.Logger) {
	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()

	breakGlass := db.NewBreakGlassRepository(pool)
	if err := promptAndSetSuperAdminPassword(ctx, breakGlass, logger); err != nil {
		logger.Fatalf("%v", err)
	}
}

// promptAndSetSuperAdminPassword prompts for a new password (with
// confirmation, no terminal echo), hashes it, and stores it - the shared
// core of both `set-superadmin-password` and `setup`.
func promptAndSetSuperAdminPassword(ctx context.Context, breakGlass *db.BreakGlassRepository, logger *log.Logger) error {
	fmt.Print("New SuperAdmin password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}

	fmt.Print("Confirm password: ")
	confirm, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}

	if string(password) != string(confirm) {
		return errors.New("passwords do not match")
	}
	if len(password) < minSuperAdminPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minSuperAdminPasswordLength)
	}

	hash, err := auth.HashPassword(string(password))
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if err := breakGlass.Set(ctx, hash); err != nil {
		return fmt.Errorf("set break-glass credential: %w", err)
	}

	logger.Println("SuperAdmin password set")
	return nil
}
