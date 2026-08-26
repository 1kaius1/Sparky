// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// User mirrors the users table - see SCHEMA.md Users. Exactly one of ADSID
// (an AD-backed row) or LocalUsername (a local-only row) is ever non-nil -
// enforced by the users_identity_mechanism_check CHECK constraint at the
// database level, not just assumed here.
type User struct {
	ID string

	// ADSID is nullable - a local-only account has no AD identity at all.
	// See SCHEMA.md Users.
	ADSID         *string
	EntraObjectID *string
	DisplayName   string
	Tier          Tier
	CreatedAt     time.Time
	LastLoginAt   *time.Time
	ElevatedBy    *string
	ElevatedAt    *time.Time

	// LDAPDN is the user's LDAP distinguishedName, cached at every login -
	// see SCHEMA.md Users and PLANNING.md's mid-session AD group
	// re-validation Decisions Log entry. Nil for a user who hasn't logged
	// in since this column was added, and always nil for a local-only
	// account (never AD-backed at all).
	LDAPDN *string

	// LocalUsername is the login identifier for a local-only account - nil
	// for an AD-backed row. local_password_hash itself is intentionally
	// not exposed on this type at all - nothing outside this file needs it
	// (FindByLocalUsername below returns it separately, only to the one
	// caller that actually verifies it).
	LocalUsername *string
}

// ErrUserNotFound is returned when a lookup or update finds no matching row.
var ErrUserNotFound = errors.New("user not found")

// ErrLocalUsernameTaken is returned by CreateLocal when local_username's
// unique constraint rejects the insert - a real, expected, user-facing
// scenario for an Admin filling out the create-local-account form, unlike
// most other unique-constraint conflicts in this codebase, which are rare
// enough today to leave as a generic error.
var ErrLocalUsernameTaken = errors.New("local username already taken")

// uniqueViolationCode is Postgres's own SQLSTATE code for a unique
// constraint violation - see
// https://www.postgresql.org/docs/current/errcodes-appendix.html.
const uniqueViolationCode = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode
}

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

const userColumns = `id, ad_sid, entra_object_id, display_name, tier, created_at, last_login_at, elevated_by, elevated_at, ldap_dn, local_username`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.ADSID, &u.EntraObjectID, &u.DisplayName, &u.Tier,
		&u.CreatedAt, &u.LastLoginAt, &u.ElevatedBy, &u.ElevatedAt, &u.LDAPDN, &u.LocalUsername)
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

// FindByLocalUsername looks up a local-only user by their login identifier -
// the local-account equivalent of FindByADSID. Returns ErrUserNotFound if no
// row matches; passwordHash is returned separately (not on User itself) as
// the one exception to "never expose local_password_hash outside this
// file" - only LocalLoginService's own verification step needs it.
func (r *UserRepository) FindByLocalUsername(ctx context.Context, username string) (user *User, passwordHash string, err error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userColumns+`, local_password_hash FROM users WHERE local_username = $1`, username)

	var u User
	scanErr := row.Scan(&u.ID, &u.ADSID, &u.EntraObjectID, &u.DisplayName, &u.Tier,
		&u.CreatedAt, &u.LastLoginAt, &u.ElevatedBy, &u.ElevatedAt, &u.LDAPDN, &u.LocalUsername, &passwordHash)
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return nil, "", ErrUserNotFound
	}
	if scanErr != nil {
		return nil, "", fmt.Errorf("scan user: %w", scanErr)
	}
	return &u, passwordHash, nil
}

// FindByID looks up a user by their internal ID - used by internal/rbac to
// fetch a target user's current tier before an elevation decision.
// Returns ErrUserNotFound if no row matches.
func (r *UserRepository) FindByID(ctx context.Context, id string) (*User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return scanUser(row)
}

// Create inserts a new user on first login. tier is the baseline assigned
// at creation - see SCHEMA.md Users, Elevation rules for who can change it
// afterward. dn is the user's LDAP distinguishedName, resolved by the same
// login that's creating this row - see LDAPDN's own doc comment.
func (r *UserRepository) Create(ctx context.Context, adSID, displayName, dn string, tier Tier) (*User, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO users (ad_sid, display_name, tier, ldap_dn) VALUES ($1, $2, $3, $4) RETURNING `+userColumns,
		adSID, displayName, tier, dn)

	u, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// CreateLocal inserts a new local-only account - no AD identity behind it at
// all (ad_sid stays NULL, matching the CHECK constraint). Returns
// ErrLocalUsernameTaken instead of a generic wrapped error when username
// collides with an existing account.
func (r *UserRepository) CreateLocal(ctx context.Context, username, passwordHash, displayName string, tier Tier) (*User, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO users (local_username, local_password_hash, display_name, tier) VALUES ($1, $2, $3, $4) RETURNING `+userColumns,
		username, passwordHash, displayName, tier)

	u, err := scanUser(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrLocalUsernameTaken
		}
		return nil, fmt.Errorf("create local user: %w", err)
	}
	return u, nil
}

// UpdateDisplayName changes a local account's own display name - the
// local-account equivalent of the display_name refresh UpdateLastLogin
// performs for an AD-backed account on every login.
func (r *UserRepository) UpdateDisplayName(ctx context.Context, id, displayName string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET display_name = $1 WHERE id = $2`, displayName, id)
	if err != nil {
		return fmt.Errorf("update display name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateLocalPassword sets a local account's password hash - used by both a
// self-service password change and an Admin-driven reset, which differ only
// in who calls this and how the new hash was authorized, not in the write
// itself.
func (r *UserRepository) UpdateLocalPassword(ctx context.Context, id, passwordHash string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET local_password_hash = $1 WHERE id = $2`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("update local password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateLastLogin records the current login timestamp and refreshes the
// cached LDAP distinguishedName (dn) - called on every successful
// authentication, not just the first, so a later mid-session recheck
// always has an up-to-date DN to search against.
func (r *UserRepository) UpdateLastLogin(ctx context.Context, id, dn string, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at = $1, ldap_dn = $2 WHERE id = $3`, at, dn, id)
	if err != nil {
		return fmt.Errorf("update last login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateTier changes a user's tier and records who made the change and
// when - see SCHEMA.md Users, Elevation rules. elevatedBy is nil when the
// SuperAdmin made the change, since the SuperAdmin is not a Users row and
// elevated_by cannot reference one - see SCHEMA.md Break-glass credential.
// elevatedAt is a pointer, not a plain time.Time, so a caller reverting a
// tier change (rbac.Service.ElevateTier, on an audit-write failure) can
// restore the exact prior value, including nil/NULL for a user who was
// never previously elevated - pgx binds a nil *time.Time to SQL NULL
// natively, no special-casing needed here.
func (r *UserRepository) UpdateTier(ctx context.Context, id string, tier Tier, elevatedBy *string, elevatedAt *time.Time) error {
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

// List returns every user, ordered by display name - the future Users &
// permissions page's full roster, and in the meantime the Audit log
// page's source for resolving an audit record's actor_id to a display
// name (the same map-of-names pattern already used for node names on the
// Model profiles and Transfers pages).
func (r *UserRepository) List(ctx context.Context) ([]*User, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY display_name`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}
