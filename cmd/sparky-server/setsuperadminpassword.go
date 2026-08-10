// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
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

	fmt.Print("New SuperAdmin password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		logger.Fatalf("read password: %v", err)
	}

	fmt.Print("Confirm password: ")
	confirm, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		logger.Fatalf("read password: %v", err)
	}

	if string(password) != string(confirm) {
		logger.Fatal("passwords do not match")
	}
	if len(password) < minSuperAdminPasswordLength {
		logger.Fatalf("password must be at least %d characters", minSuperAdminPasswordLength)
	}

	hash, err := auth.HashPassword(string(password))
	if err != nil {
		logger.Fatalf("hash password: %v", err)
	}

	breakGlass := db.NewBreakGlassRepository(pool)
	if err := breakGlass.Set(ctx, hash); err != nil {
		logger.Fatalf("set break-glass credential: %v", err)
	}

	logger.Println("SuperAdmin password set")
}
