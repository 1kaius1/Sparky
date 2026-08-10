// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
	"time"
)

// These tests intentionally avoid requiring a live Postgres instance - see
// ARCHITECTURE.md Testing Strategy: full database integration tests are a
// separate concern from this package's own unit coverage.

func TestNew_MalformedDSN_ReturnsError(t *testing.T) {
	ctx := context.Background()

	_, err := New(ctx, "not-a-valid-connection-string")
	if err == nil {
		t.Fatal("New() succeeded with a malformed DSN, want an error")
	}
}

func TestNew_UnreachableDatabase_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Port 1 is privileged and never has Postgres listening on it in any
	// real environment, so this fails fast with connection refused rather
	// than waiting out the ping timeout.
	_, err := New(ctx, "postgres://user:pass@127.0.0.1:1/sparky_dev?sslmode=disable")
	if err == nil {
		t.Fatal("New() succeeded against an unreachable database, want an error")
	}
}
