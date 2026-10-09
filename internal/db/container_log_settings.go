// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ContainerLogSettings mirrors the container_log_settings table - see
// SCHEMA.md Container log settings. A singleton row, always present as of
// migration 000040.
type ContainerLogSettings struct {
	RetentionMonths int
	UpdatedBy       *string
	UpdatedAt       time.Time
}

// ContainerLogSettingsRepository is the only component that queries the
// container_log_settings table directly.
type ContainerLogSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewContainerLogSettingsRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewContainerLogSettingsRepository(pool *pgxpool.Pool) *ContainerLogSettingsRepository {
	return &ContainerLogSettingsRepository{pool: pool}
}

// Get returns the current settings.
func (r *ContainerLogSettingsRepository) Get(ctx context.Context) (*ContainerLogSettings, error) {
	var s ContainerLogSettings
	err := r.pool.QueryRow(ctx,
		`SELECT retention_months, updated_by, updated_at FROM container_log_settings WHERE id = true`).
		Scan(&s.RetentionMonths, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get container log settings: %w", err)
	}
	return &s, nil
}

// Update sets the retention period. updatedBy is nil only for the
// break-glass SuperAdmin, which is not a Users row. The database's own CHECK
// rejects a value outside 1 to 24, as a second line behind the service.
func (r *ContainerLogSettingsRepository) Update(ctx context.Context, retentionMonths int, updatedBy *string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE container_log_settings SET retention_months = $1, updated_by = $2, updated_at = now() WHERE id = true`,
		retentionMonths, updatedBy); err != nil {
		return fmt.Errorf("update container log settings: %w", err)
	}
	return nil
}
