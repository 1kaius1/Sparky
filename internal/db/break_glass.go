// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BreakGlassCredential mirrors the break_glass_credential table - see
// SCHEMA.md Break-glass credential. Not a row in Users: a single, isolated
// secret only the application process reads or validates.
type BreakGlassCredential struct {
	PasswordHash string
	UpdatedAt    time.Time
}

// ErrBreakGlassNotSet is returned when the break-glass credential has
// never been configured - the state before `sparky set-superadmin-password`
// has ever run.
var ErrBreakGlassNotSet = errors.New("break-glass credential not set")

// BreakGlassRepository is the only component that queries the
// break_glass_credential table directly.
type BreakGlassRepository struct {
	pool *pgxpool.Pool
}

// NewBreakGlassRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewBreakGlassRepository(pool *pgxpool.Pool) *BreakGlassRepository {
	return &BreakGlassRepository{pool: pool}
}

// Get returns the current break-glass credential, or ErrBreakGlassNotSet
// if it has never been configured.
func (r *BreakGlassRepository) Get(ctx context.Context) (*BreakGlassCredential, error) {
	row := r.pool.QueryRow(ctx, `SELECT password_hash, updated_at FROM break_glass_credential WHERE id = true`)

	var c BreakGlassCredential
	err := row.Scan(&c.PasswordHash, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBreakGlassNotSet
	}
	if err != nil {
		return nil, fmt.Errorf("get break-glass credential: %w", err)
	}
	return &c, nil
}

// Set creates or replaces the break-glass credential - the only write
// path, backing `sparky set-superadmin-password`.
func (r *BreakGlassRepository) Set(ctx context.Context, passwordHash string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO break_glass_credential (id, password_hash, updated_at)
		 VALUES (true, $1, now())
		 ON CONFLICT (id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = now()`,
		passwordHash)
	if err != nil {
		return fmt.Errorf("set break-glass credential: %w", err)
	}
	return nil
}
