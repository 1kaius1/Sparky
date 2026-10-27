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

	// DailyDigestTime is "HH:MM" in the global time zone: when the daily digest
	// goes out.
	DailyDigestTime string

	// DigestScheduleSetAt is when the digest schedule last changed; the shared
	// scheduler never runs an occurrence from before it. LastHourlyDigestAt and
	// LastDailyDigestAt are when each digest last began, nil if never.
	DigestScheduleSetAt time.Time
	LastHourlyDigestAt  *time.Time
	LastDailyDigestAt   *time.Time

	UpdatedBy *string
	UpdatedAt time.Time
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

const emailSettingsColumns = `enabled, host, port, tls_mode, username, from_address, from_name,
	daily_digest_time, digest_schedule_set_at, last_hourly_digest_at, last_daily_digest_at, updated_by, updated_at`

// Get returns the current settings.
func (r *EmailSettingsRepository) Get(ctx context.Context) (*EmailSettings, error) {
	var s EmailSettings
	err := r.pool.QueryRow(ctx, `SELECT `+emailSettingsColumns+` FROM email_settings WHERE id = true`).
		Scan(&s.Enabled, &s.Host, &s.Port, &s.TLSMode, &s.Username, &s.FromAddress, &s.FromName,
			&s.DailyDigestTime, &s.DigestScheduleSetAt, &s.LastHourlyDigestAt, &s.LastDailyDigestAt, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get email settings: %w", err)
	}
	return &s, nil
}

// Update saves what an Admin can change: the relay, the sender and the daily
// digest time. Changing the digest time restarts the digest schedule
// (DigestScheduleSetAt becomes now), so a new time never triggers a digest for
// a moment that has already passed; saving anything else leaves it alone.
// UpdatedBy is nil only for the break-glass SuperAdmin, which is not a Users
// row. The database refuses enabling email without a relay host and a from
// address, and a malformed digest time, a second line behind the service.
func (r *EmailSettingsRepository) Update(ctx context.Context, s EmailSettings) error {
	// Every SET expression reads the row as it was before this statement.
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_settings SET enabled = $1, host = $2, port = $3, tls_mode = $4, username = $5,
		   from_address = $6, from_name = $7,
		   digest_schedule_set_at = CASE WHEN daily_digest_time IS DISTINCT FROM $8 THEN now() ELSE digest_schedule_set_at END,
		   daily_digest_time = $8, updated_by = $9, updated_at = now()
		 WHERE id = true`,
		s.Enabled, s.Host, s.Port, s.TLSMode, s.Username, s.FromAddress, s.FromName, s.DailyDigestTime, s.UpdatedBy); err != nil {
		return fmt.Errorf("update email settings: %w", err)
	}
	return nil
}

// Restore puts the row back exactly as prev had it, including its update time
// and digest bookkeeping - used to undo a change whose audit record could not
// be written.
func (r *EmailSettingsRepository) Restore(ctx context.Context, prev *EmailSettings) error {
	if prev == nil {
		return fmt.Errorf("restore email settings: nothing to restore")
	}
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_settings SET enabled = $1, host = $2, port = $3, tls_mode = $4, username = $5,
		   from_address = $6, from_name = $7, daily_digest_time = $8, digest_schedule_set_at = $9,
		   last_hourly_digest_at = $10, last_daily_digest_at = $11, updated_by = $12, updated_at = $13
		 WHERE id = true`,
		prev.Enabled, prev.Host, prev.Port, prev.TLSMode, prev.Username, prev.FromAddress, prev.FromName, prev.DailyDigestTime,
		prev.DigestScheduleSetAt, prev.LastHourlyDigestAt, prev.LastDailyDigestAt, prev.UpdatedBy, prev.UpdatedAt); err != nil {
		return fmt.Errorf("restore email settings: %w", err)
	}
	return nil
}

// DigestKind names one of the two digests.
type DigestKind string

const (
	DigestHourly DigestKind = "hourly"
	DigestDaily  DigestKind = "daily"
)

// MarkDigestRun records that a digest began at startedAt, before it does
// anything, so a crash part-way cannot make it run twice.
func (r *EmailSettingsRepository) MarkDigestRun(ctx context.Context, kind DigestKind, startedAt time.Time) error {
	column := "last_hourly_digest_at"
	if kind == DigestDaily {
		column = "last_daily_digest_at"
	}
	if _, err := r.pool.Exec(ctx, `UPDATE email_settings SET `+column+` = $1 WHERE id = true`, startedAt); err != nil {
		return fmt.Errorf("mark %s digest run: %w", kind, err)
	}
	return nil
}
