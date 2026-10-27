// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ScheduleSettings mirrors the schedule_settings table - see SCHEMA.md
// Schedule settings. A singleton row, always present as of migration 000044.
type ScheduleSettings struct {
	Timezone  string
	UpdatedBy *string
	UpdatedAt time.Time
}

// ScheduleSettingsRepository is the only component that queries the
// schedule_settings table directly.
type ScheduleSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewScheduleSettingsRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewScheduleSettingsRepository(pool *pgxpool.Pool) *ScheduleSettingsRepository {
	return &ScheduleSettingsRepository{pool: pool}
}

// Get returns the current settings.
func (r *ScheduleSettingsRepository) Get(ctx context.Context) (*ScheduleSettings, error) {
	var s ScheduleSettings
	err := r.pool.QueryRow(ctx,
		`SELECT timezone, updated_by, updated_at FROM schedule_settings WHERE id = true`).
		Scan(&s.Timezone, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get schedule settings: %w", err)
	}
	return &s, nil
}

// Update sets the time zone. updatedBy is nil only for the break-glass
// SuperAdmin, which is not a Users row. The name is validated against the zone
// database by the service before it gets here; the database only refuses a
// blank one.
func (r *ScheduleSettingsRepository) Update(ctx context.Context, timezone string, updatedBy *string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE schedule_settings SET timezone = $1, updated_by = $2, updated_at = now() WHERE id = true`,
		timezone, updatedBy); err != nil {
		return fmt.Errorf("update schedule settings: %w", err)
	}
	return nil
}
