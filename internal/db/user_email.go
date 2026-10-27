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

// EmailSource says where a user's email address came from - see SCHEMA.md
// Users.
type EmailSource string

const (
	// EmailSourceDirectory: read from AD's mail attribute at login. AD-backed
	// accounts only.
	EmailSourceDirectory EmailSource = "directory"

	// EmailSourceSelf: a local account's own address, confirmed by its owner.
	EmailSourceSelf EmailSource = "self"

	// EmailSourceAdmin: a local account's address set by an Admin.
	EmailSourceAdmin EmailSource = "admin"
)

// UserEmail is one user's email address and where it came from. It is kept apart
// from User on purpose: email is read by a few places only, and widening the
// shared user row would touch every query that scans one.
type UserEmail struct {
	UserID string

	// Directory is true for an AD-backed account, whose address comes from the
	// directory and cannot be edited here.
	Directory bool

	Email  *string
	Source *EmailSource
}

// EmailConfirmation is a change of address waiting to be confirmed.
type EmailConfirmation struct {
	UserID      string
	Email       string
	TokenHash   string
	Source      EmailSource
	RequestedBy *string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// ErrEmailConfirmationNotFound is returned for an unknown token or a user with
// nothing pending.
var ErrEmailConfirmationNotFound = errors.New("email confirmation not found")

// UserEmailRepository is the only component that queries users' email columns
// and the email_confirmations table directly.
type UserEmailRepository struct {
	pool *pgxpool.Pool
}

// NewUserEmailRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewUserEmailRepository(pool *pgxpool.Pool) *UserEmailRepository {
	return &UserEmailRepository{pool: pool}
}

// Get returns one user's address, or ErrUserNotFound.
func (r *UserEmailRepository) Get(ctx context.Context, userID string) (*UserEmail, error) {
	var u UserEmail
	err := r.pool.QueryRow(ctx,
		`SELECT id, ad_sid IS NOT NULL, email, email_source FROM users WHERE id = $1`, userID).
		Scan(&u.UserID, &u.Directory, &u.Email, &u.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user email: %w", err)
	}
	return &u, nil
}

// ListAll returns every user's address, keyed by user id - what the Users page
// shows beside each account.
func (r *UserEmailRepository) ListAll(ctx context.Context) (map[string]*UserEmail, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, ad_sid IS NOT NULL, email, email_source FROM users`)
	if err != nil {
		return nil, fmt.Errorf("list user emails: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*UserEmail)
	for rows.Next() {
		var u UserEmail
		if err := rows.Scan(&u.UserID, &u.Directory, &u.Email, &u.Source); err != nil {
			return nil, fmt.Errorf("scan user email: %w", err)
		}
		out[u.UserID] = &u
	}
	return out, rows.Err()
}

// SetDirectory records the address AD gave for an AD-backed account, or clears
// it when AD gave none (email nil). It does nothing for a local account, whose
// address the directory knows nothing about, and returns whether it changed
// anything.
func (r *UserEmailRepository) SetDirectory(ctx context.Context, userID string, email *string) (bool, error) {
	var source *EmailSource
	if email != nil {
		s := EmailSourceDirectory
		source = &s
	}
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET email = $2, email_source = $3
		 WHERE id = $1 AND ad_sid IS NOT NULL AND (email IS DISTINCT FROM $2 OR email_source IS DISTINCT FROM $3)`,
		userID, email, source)
	if err != nil {
		return false, fmt.Errorf("set directory email: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetLocal sets or clears (email nil, source ignored) a local account's
// address. It does nothing for an AD-backed account, whose address comes from
// the directory, and returns whether it changed anything.
func (r *UserEmailRepository) SetLocal(ctx context.Context, userID string, email *string, source EmailSource) (bool, error) {
	var src *EmailSource
	if email != nil {
		src = &source
	}
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET email = $2, email_source = $3
		 WHERE id = $1 AND ad_sid IS NULL AND (email IS DISTINCT FROM $2 OR email_source IS DISTINCT FROM $3)`,
		userID, email, src)
	if err != nil {
		return false, fmt.Errorf("set local email: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PutConfirmation records a pending change of address for a user, replacing any
// earlier one.
func (r *UserEmailRepository) PutConfirmation(ctx context.Context, c EmailConfirmation) error {
	if _, err := r.pool.Exec(ctx,
		`INSERT INTO email_confirmations (user_id, email, token_hash, source, requested_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id) DO UPDATE SET email = EXCLUDED.email, token_hash = EXCLUDED.token_hash,
		   source = EXCLUDED.source, requested_by = EXCLUDED.requested_by, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at`,
		c.UserID, c.Email, c.TokenHash, c.Source, c.RequestedBy, c.CreatedAt, c.ExpiresAt); err != nil {
		return fmt.Errorf("put email confirmation: %w", err)
	}
	return nil
}

const confirmationColumns = `user_id, email, token_hash, source, requested_by, created_at, expires_at`

func scanConfirmation(row pgx.Row) (*EmailConfirmation, error) {
	var c EmailConfirmation
	err := row.Scan(&c.UserID, &c.Email, &c.TokenHash, &c.Source, &c.RequestedBy, &c.CreatedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEmailConfirmationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan email confirmation: %w", err)
	}
	return &c, nil
}

// PendingConfirmation returns a user's pending change of address, or
// ErrEmailConfirmationNotFound.
func (r *UserEmailRepository) PendingConfirmation(ctx context.Context, userID string) (*EmailConfirmation, error) {
	return scanConfirmation(r.pool.QueryRow(ctx, `SELECT `+confirmationColumns+` FROM email_confirmations WHERE user_id = $1`, userID))
}

// ConfirmationByTokenHash finds the pending change a token belongs to, or
// ErrEmailConfirmationNotFound.
func (r *UserEmailRepository) ConfirmationByTokenHash(ctx context.Context, hash string) (*EmailConfirmation, error) {
	return scanConfirmation(r.pool.QueryRow(ctx, `SELECT `+confirmationColumns+` FROM email_confirmations WHERE token_hash = $1`, hash))
}

// DeleteConfirmation removes a user's pending change, and reports whether there
// was one.
func (r *UserEmailRepository) DeleteConfirmation(ctx context.Context, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM email_confirmations WHERE user_id = $1`, userID)
	if err != nil {
		return false, fmt.Errorf("delete email confirmation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteExpiredConfirmations removes pending changes past their expiry and
// returns how many.
func (r *UserEmailRepository) DeleteExpiredConfirmations(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM email_confirmations WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired email confirmations: %w", err)
	}
	return tag.RowsAffected(), nil
}
