// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertSettings mirrors the alert_settings table - see SCHEMA.md Alert
// settings. A singleton row, always present as of migration 000041.
type AlertSettings struct {
	RetentionMonths int
	UpdatedBy       *string
	UpdatedAt       time.Time
}

// AlertSettingsRepository is the only component that queries the
// alert_settings table directly.
type AlertSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewAlertSettingsRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewAlertSettingsRepository(pool *pgxpool.Pool) *AlertSettingsRepository {
	return &AlertSettingsRepository{pool: pool}
}

// Get returns the current settings.
func (r *AlertSettingsRepository) Get(ctx context.Context) (*AlertSettings, error) {
	var s AlertSettings
	err := r.pool.QueryRow(ctx,
		`SELECT retention_months, updated_by, updated_at FROM alert_settings WHERE id = true`).
		Scan(&s.RetentionMonths, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get alert settings: %w", err)
	}
	return &s, nil
}

// Update sets the retention period. updatedBy is nil only for the
// break-glass SuperAdmin, which is not a Users row. The database's own CHECK
// rejects a value outside 1 to 24, as a second line behind the service.
func (r *AlertSettingsRepository) Update(ctx context.Context, retentionMonths int, updatedBy *string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE alert_settings SET retention_months = $1, updated_by = $2, updated_at = now() WHERE id = true`,
		retentionMonths, updatedBy); err != nil {
		return fmt.Errorf("update alert settings: %w", err)
	}
	return nil
}
