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

// Capability mirrors the permission_capability Postgres enum - see
// migrations/000002_create_permission_overrides.up.sql and SCHEMA.md
// Permission overrides.
type Capability string

// CapabilityManageModelStore is currently the only capability - download
// and delete are tied together as one grant, see PLANNING.md Decisions Log.
const CapabilityManageModelStore Capability = "manage_model_store"

// PermissionOverride mirrors the permission_overrides table. Admins and
// SuperAdmin have every capability implicitly and never have a row here -
// see SCHEMA.md Permission overrides.
type PermissionOverride struct {
	UserID     string
	Capability Capability
	GrantedBy  string
	GrantedAt  time.Time
}

// ErrPermissionOverrideNotFound is returned when a user has not been
// granted the given capability.
var ErrPermissionOverrideNotFound = errors.New("permission override not found")

// PermissionOverrideRepository is the only component that queries the
// permission_overrides table directly.
type PermissionOverrideRepository struct {
	pool *pgxpool.Pool
}

// NewPermissionOverrideRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewPermissionOverrideRepository(pool *pgxpool.Pool) *PermissionOverrideRepository {
	return &PermissionOverrideRepository{pool: pool}
}

const permissionOverrideColumns = `user_id, capability, granted_by, granted_at`

func scanPermissionOverride(row pgx.Row) (*PermissionOverride, error) {
	var o PermissionOverride
	err := row.Scan(&o.UserID, &o.Capability, &o.GrantedBy, &o.GrantedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPermissionOverrideNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan permission override: %w", err)
	}
	return &o, nil
}

// Get looks up a user's override row for a specific capability. Returns
// ErrPermissionOverrideNotFound if the user has not been granted it.
func (r *PermissionOverrideRepository) Get(ctx context.Context, userID string, capability Capability) (*PermissionOverride, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+permissionOverrideColumns+` FROM permission_overrides WHERE user_id = $1 AND capability = $2`,
		userID, capability)
	return scanPermissionOverride(row)
}

// Grant records a capability override for a user. Granting a capability
// the user already has replaces the granted_by/granted_at record rather
// than erroring.
func (r *PermissionOverrideRepository) Grant(ctx context.Context, userID string, capability Capability, grantedBy string) (*PermissionOverride, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO permission_overrides (user_id, capability, granted_by)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, capability) DO UPDATE SET granted_by = EXCLUDED.granted_by, granted_at = now()
		 RETURNING `+permissionOverrideColumns,
		userID, capability, grantedBy)

	o, err := scanPermissionOverride(row)
	if err != nil {
		return nil, fmt.Errorf("grant permission override: %w", err)
	}
	return o, nil
}

// Revoke removes a capability override. Revoking one that was never
// granted is not an error - the end state is the same either way.
func (r *PermissionOverrideRepository) Revoke(ctx context.Context, userID string, capability Capability) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM permission_overrides WHERE user_id = $1 AND capability = $2`, userID, capability)
	if err != nil {
		return fmt.Errorf("revoke permission override: %w", err)
	}
	return nil
}
