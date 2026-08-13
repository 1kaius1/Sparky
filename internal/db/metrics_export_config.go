// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MetricsExportBackend mirrors the metrics_export_backend Postgres enum -
// see migrations/000012_create_metrics_export_config.up.sql and
// SCHEMA.md Metrics export config.
type MetricsExportBackend string

const (
	MetricsExportBackendNone MetricsExportBackend = "none"
	MetricsExportBackendNFS  MetricsExportBackend = "nfs"
	MetricsExportBackendS3   MetricsExportBackend = "s3"
)

// MetricsExportConfig mirrors the metrics_export_config table - see
// SCHEMA.md Metrics export config. A singleton row, always present as of
// migration 000012 (seeded with backend_type = 'none') - unlike
// BreakGlassCredential, there is no "not configured yet" state to
// represent here.
type MetricsExportConfig struct {
	BackendType MetricsExportBackend
	Config      json.RawMessage
	UpdatedBy   *string
	UpdatedAt   time.Time
}

// MetricsExportConfigRepository is the only component that queries the
// metrics_export_config table directly.
type MetricsExportConfigRepository struct {
	pool *pgxpool.Pool
}

// NewMetricsExportConfigRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewMetricsExportConfigRepository(pool *pgxpool.Pool) *MetricsExportConfigRepository {
	return &MetricsExportConfigRepository{pool: pool}
}

// Get returns the current metrics export config - the Settings page's
// read path. No write path exists yet (PLANNING.md's Dashboard UI
// write/action forms are a later phase), so this repository is
// deliberately read-only for now.
func (r *MetricsExportConfigRepository) Get(ctx context.Context) (*MetricsExportConfig, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT backend_type, config, updated_by, updated_at FROM metrics_export_config WHERE id = true`)

	var c MetricsExportConfig
	if err := row.Scan(&c.BackendType, &c.Config, &c.UpdatedBy, &c.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get metrics export config: %w", err)
	}
	return &c, nil
}
