// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditForwardingProtocol mirrors the audit_forwarding_protocol Postgres
// enum - see migrations/000013_create_audit_settings.up.sql and
// SCHEMA.md Audit settings.
type AuditForwardingProtocol string

const (
	AuditForwardingSyslog AuditForwardingProtocol = "syslog"
	AuditForwardingGELF   AuditForwardingProtocol = "gelf"
)

// AuditSettings mirrors the audit_settings table - see SCHEMA.md Audit
// settings. A singleton row, always present as of migration 000013
// (seeded with the defaults documented on that migration) - same
// always-present reasoning as MetricsExportConfig.
type AuditSettings struct {
	RetentionMonths      int
	ForwardingEnabled    bool
	ForwardingProtocol   AuditForwardingProtocol
	ForwardingHost       *string
	ForwardingPort       *int
	ForwardingTLSEnabled bool
	UpdatedBy            *string
	UpdatedAt            time.Time
}

// AuditSettingsRepository is the only component that queries the
// audit_settings table directly.
type AuditSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewAuditSettingsRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewAuditSettingsRepository(pool *pgxpool.Pool) *AuditSettingsRepository {
	return &AuditSettingsRepository{pool: pool}
}

// Get returns the current audit settings - the Settings page's read
// path. No write path exists yet (PLANNING.md's Dashboard UI
// write/action forms are a later phase), so this repository is
// deliberately read-only for now.
func (r *AuditSettingsRepository) Get(ctx context.Context) (*AuditSettings, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT retention_months, forwarding_enabled, forwarding_protocol, forwarding_host, forwarding_port, forwarding_tls_enabled, updated_by, updated_at
		 FROM audit_settings WHERE id = true`)

	var s AuditSettings
	if err := row.Scan(&s.RetentionMonths, &s.ForwardingEnabled, &s.ForwardingProtocol, &s.ForwardingHost, &s.ForwardingPort, &s.ForwardingTLSEnabled, &s.UpdatedBy, &s.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get audit settings: %w", err)
	}
	return &s, nil
}
