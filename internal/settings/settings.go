// SPDX-License-Identifier: AGPL-3.0-or-later

// Package settings is the RBAC-gated read path for the Settings page's two
// singleton config rows - Metrics export config and Audit settings (see
// SCHEMA.md for both). Neither row belongs to an existing Service's
// domain: internal/metrics.Service is scoped to telemetry ingestion only
// (its own doc comment defers NFS/S3 export itself to the v0.4.0
// Historical metrics milestone), and internal/audit.Recorder is scoped to
// the audit_log table, not the separate audit_settings table that
// configures its optional forwarding. This package exists so the single
// RBAC decision behind the Settings page ("may this actor view it at
// all") has one home, rather than being split across two unrelated
// packages for one page's data.
package settings

import (
	"context"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// metricsExportStore is the subset of *db.MetricsExportConfigRepository
// this package needs, narrow enough to fake in tests without a real
// Postgres instance - same pattern used throughout internal/httpapi and
// internal/rbac.
type metricsExportStore interface {
	Get(ctx context.Context) (*db.MetricsExportConfig, error)
}

// auditSettingsStore is the subset of *db.AuditSettingsRepository this
// package needs.
type auditSettingsStore interface {
	Get(ctx context.Context) (*db.AuditSettings, error)
}

// Service is the Settings page's orchestration layer - callers never
// access *db.MetricsExportConfigRepository/*db.AuditSettingsRepository
// directly, matching the Handler -> Service Layer -> Repository pattern
// CLAUDE.md establishes elsewhere.
type Service struct {
	metricsExport metricsExportStore
	auditSettings auditSettingsStore
}

// NewService constructs a Service.
func NewService(metricsExport metricsExportStore, auditSettings auditSettingsStore) *Service {
	return &Service{metricsExport: metricsExport, auditSettings: auditSettings}
}

// Get returns both singleton config rows if actor is permitted to view
// the Settings page - see rbac.CanViewSettings. The RBAC check lives
// here, not only at the HTTP layer, matching audit.Recorder.List's and
// rbac.Service.ListUsers's own reasoning: the guarantee travels with the
// method regardless of caller. Returns rbac.ErrNotPermitted if actor is
// not permitted; the two rows are returned as plain *db types rather than
// a wrapping struct so internal/httpapi doesn't need to import this
// package's own types, matching how auditLister/transferLister/userRoster
// are defined there against db types alone.
func (s *Service) Get(ctx context.Context, actor rbac.Actor) (*db.MetricsExportConfig, *db.AuditSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, nil, rbac.ErrNotPermitted
	}

	metricsExport, err := s.metricsExport.Get(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get metrics export config: %w", err)
	}
	auditSettings, err := s.auditSettings.Get(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get audit settings: %w", err)
	}
	return metricsExport, auditSettings, nil
}
