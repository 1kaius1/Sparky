// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EmailTLSMode is how the connection to the SMTP relay is protected.
type EmailTLSMode string

const (
	EmailTLSNone     EmailTLSMode = "none"
	EmailTLSStartTLS EmailTLSMode = "starttls"
	EmailTLSImplicit EmailTLSMode = "tls"
)

// Valid reports whether m is one of the three modes.
func (m EmailTLSMode) Valid() bool {
	return m == EmailTLSNone || m == EmailTLSStartTLS || m == EmailTLSImplicit
}

// EmailSettings mirrors the email_settings table - see SCHEMA.md Email
// settings. A singleton row, always present as of migration 000046. The
// relay's password is not part of it: it is an environment variable.
type EmailSettings struct {
	Enabled     bool
	Host        string
	Port        int
	TLSMode     EmailTLSMode
	Username    string
	FromAddress string
	FromName    string
	UpdatedBy   *string
	UpdatedAt   time.Time
}

// EmailSettingsRepository is the only component that queries the
// email_settings table directly.
type EmailSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewEmailSettingsRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewEmailSettingsRepository(pool *pgxpool.Pool) *EmailSettingsRepository {
	return &EmailSettingsRepository{pool: pool}
}

const emailSettingsColumns = `enabled, host, port, tls_mode, username, from_address, from_name, updated_by, updated_at`

// Get returns the current settings.
func (r *EmailSettingsRepository) Get(ctx context.Context) (*EmailSettings, error) {
	var s EmailSettings
	err := r.pool.QueryRow(ctx, `SELECT `+emailSettingsColumns+` FROM email_settings WHERE id = true`).
		Scan(&s.Enabled, &s.Host, &s.Port, &s.TLSMode, &s.Username, &s.FromAddress, &s.FromName, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get email settings: %w", err)
	}
	return &s, nil
}

// Update saves every field. UpdatedBy is nil only for the break-glass
// SuperAdmin, which is not a Users row. The database refuses enabling email
// without a relay host and a from address, a second line behind the service.
func (r *EmailSettingsRepository) Update(ctx context.Context, s EmailSettings) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_settings SET enabled = $1, host = $2, port = $3, tls_mode = $4, username = $5,
		   from_address = $6, from_name = $7, updated_by = $8, updated_at = now()
		 WHERE id = true`,
		s.Enabled, s.Host, s.Port, s.TLSMode, s.Username, s.FromAddress, s.FromName, s.UpdatedBy); err != nil {
		return fmt.Errorf("update email settings: %w", err)
	}
	return nil
}

// Restore puts the row back exactly as prev had it, including its update time
// - used to undo a change whose audit record could not be written.
func (r *EmailSettingsRepository) Restore(ctx context.Context, prev *EmailSettings) error {
	if prev == nil {
		return fmt.Errorf("restore email settings: nothing to restore")
	}
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_settings SET enabled = $1, host = $2, port = $3, tls_mode = $4, username = $5,
		   from_address = $6, from_name = $7, updated_by = $8, updated_at = $9
		 WHERE id = true`,
		prev.Enabled, prev.Host, prev.Port, prev.TLSMode, prev.Username, prev.FromAddress, prev.FromName, prev.UpdatedBy, prev.UpdatedAt); err != nil {
		return fmt.Errorf("restore email settings: %w", err)
	}
	return nil
}
