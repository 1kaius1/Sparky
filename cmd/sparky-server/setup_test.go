// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/1kaius1/Sparky/internal/db"
)

// These are integration tests against a real, migrated Postgres instance -
// see internal/db's own test files for the same DATABASE_URL-driven skip
// pattern.

func newTestUserRepoForSetup(t *testing.T) *db.UserRepository {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}

	pool, err := db.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	return db.NewUserRepository(pool)
}

func testLoggerForSetup() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

// cleanupDefaultAdminAccount removes the bootstrapped "admin" row after a
// test, via a raw pgxpool connection - db.UserRepository exposes no delete
// method (this codebase has no reason for one outside tests), so this
// connects directly, same as internal/db's own test cleanup helpers do
// from within that package.
func cleanupDefaultAdminAccount(t *testing.T, databaseURL string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), databaseURL)
		if err != nil {
			t.Logf("cleanup: connect to test database: %v", err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE local_username = $1`, defaultAdminLocalUsername); err != nil {
			t.Logf("cleanup: delete default admin account: %v", err)
		}
	})
}

func TestBootstrapDefaultAdminAccount_CreatesAccount(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}
	users := newTestUserRepoForSetup(t)
	ctx := context.Background()
	cleanupDefaultAdminAccount(t, databaseURL)

	if err := bootstrapDefaultAdminAccount(ctx, users, testLoggerForSetup()); err != nil {
		t.Fatalf("bootstrapDefaultAdminAccount() error: %v", err)
	}

	created, _, err := users.FindByLocalUsername(ctx, defaultAdminLocalUsername)
	if err != nil {
		t.Fatalf("FindByLocalUsername() after bootstrap: %v", err)
	}
	if created.DisplayName != defaultAdminDisplayName {
		t.Errorf("DisplayName = %q, want %q", created.DisplayName, defaultAdminDisplayName)
	}
	if created.Tier != db.TierAdmin {
		t.Errorf("Tier = %q, want %q", created.Tier, db.TierAdmin)
	}
}

func TestBootstrapDefaultAdminAccount_Idempotent(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}
	users := newTestUserRepoForSetup(t)
	ctx := context.Background()
	cleanupDefaultAdminAccount(t, databaseURL)

	if err := bootstrapDefaultAdminAccount(ctx, users, testLoggerForSetup()); err != nil {
		t.Fatalf("bootstrapDefaultAdminAccount() first call error: %v", err)
	}
	first, hashBefore, err := users.FindByLocalUsername(ctx, defaultAdminLocalUsername)
	if err != nil {
		t.Fatalf("FindByLocalUsername() after first bootstrap: %v", err)
	}

	// A second run must be a no-op - it must never reset an
	// already-existing admin account's password.
	if err := bootstrapDefaultAdminAccount(ctx, users, testLoggerForSetup()); err != nil {
		t.Fatalf("bootstrapDefaultAdminAccount() second call error: %v", err)
	}
	second, hashAfter, err := users.FindByLocalUsername(ctx, defaultAdminLocalUsername)
	if err != nil {
		t.Fatalf("FindByLocalUsername() after second bootstrap: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("ID changed across bootstrap runs: %q -> %q", first.ID, second.ID)
	}
	if hashBefore != hashAfter {
		t.Error("password hash changed on a second bootstrap run - must be left unchanged")
	}
}
