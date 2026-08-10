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

// Tier mirrors the user_tier Postgres enum - see
// migrations/000001_create_users.up.sql and SCHEMA.md Users.
type Tier string

const (
	TierReadOnly  Tier = "read_only"
	TierDeveloper Tier = "developer"
	TierPowerDev  Tier = "power_dev"
	TierAdmin     Tier = "admin"
)

// User mirrors the users table - see SCHEMA.md Users.
type User struct {
	ID            string
	ADSID         string
	EntraObjectID *string
	DisplayName   string
	Tier          Tier
	CreatedAt     time.Time
	LastLoginAt   *time.Time
	ElevatedBy    *string
	ElevatedAt    *time.Time
}

// ErrUserNotFound is returned when a lookup or update finds no matching row.
var ErrUserNotFound = errors.New("user not found")

// UserRepository is the only component that queries the users table
// directly - see CLAUDE.md: the repository layer is the only place that
// accesses the database directly. Elevation rules (who may call UpdateTier,
// and with what constraints) belong to the RBAC component, not here - see
// SCHEMA.md Users, Elevation rules.
type UserRepository struct {
	pool *pgxpool.Pool
}

// NewUserRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

const userColumns = `id, ad_sid, entra_object_id, display_name, tier, created_at, last_login_at, elevated_by, elevated_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.ADSID, &u.EntraObjectID, &u.DisplayName, &u.Tier,
		&u.CreatedAt, &u.LastLoginAt, &u.ElevatedBy, &u.ElevatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}

// FindByADSID looks up a user by their AD SID - the external identity
// reference used at login time. Returns ErrUserNotFound if no row matches.
func (r *UserRepository) FindByADSID(ctx context.Context, adSID string) (*User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE ad_sid = $1`, adSID)
	return scanUser(row)
}

// Create inserts a new user on first login. tier is the baseline assigned
// at creation - see SCHEMA.md Users, Elevation rules for who can change it
// afterward.
func (r *UserRepository) Create(ctx context.Context, adSID, displayName string, tier Tier) (*User, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO users (ad_sid, display_name, tier) VALUES ($1, $2, $3) RETURNING `+userColumns,
		adSID, displayName, tier)

	u, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// UpdateLastLogin records the current login timestamp - called on every
// successful authentication, not just the first.
func (r *UserRepository) UpdateLastLogin(ctx context.Context, id string, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at = $1 WHERE id = $2`, at, id)
	if err != nil {
		return fmt.Errorf("update last login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateTier changes a user's tier and records who made the change and
// when - see SCHEMA.md Users, Elevation rules.
func (r *UserRepository) UpdateTier(ctx context.Context, id string, tier Tier, elevatedBy string, elevatedAt time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET tier = $1, elevated_by = $2, elevated_at = $3 WHERE id = $4`,
		tier, elevatedBy, elevatedAt, id)
	if err != nil {
		return fmt.Errorf("update user tier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}
