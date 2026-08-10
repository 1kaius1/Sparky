// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db owns the Postgres connection pool. Per CLAUDE.md, it is the
// only package that accesses the database directly - repository/query code
// for individual entities (Users, Nodes, and so on) lands here as those
// features are built.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pingTimeout bounds how long New waits for the initial connectivity check,
// so a misconfigured or unreachable database fails startup quickly rather
// than hanging - see ARCHITECTURE.md Application Lifecycle, Database
// Connection Pool.
const pingTimeout = 5 * time.Second

// New creates a connection pool and verifies connectivity with a ping
// before returning, so the caller can fail fast at startup rather than
// discovering a bad DATABASE_URL on the first request.
func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return pool, nil
}
