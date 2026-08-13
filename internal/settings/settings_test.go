// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"context"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

type fakeMetricsExportStore struct {
	config *db.MetricsExportConfig
	err    error
}

func (f *fakeMetricsExportStore) Get(context.Context) (*db.MetricsExportConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.config, nil
}

type fakeAuditSettingsStore struct {
	settings *db.AuditSettings
	err      error
}

func (f *fakeAuditSettingsStore) Get(context.Context) (*db.AuditSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.settings, nil
}

func TestService_Get_PermittedForAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog}}
	svc := NewService(metricsExport, auditSettings)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	mec, as, err := svc.Get(context.Background(), actor)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if mec.BackendType != db.MetricsExportBackendNone {
		t.Errorf("BackendType = %q, want %q", mec.BackendType, db.MetricsExportBackendNone)
	}
	if as.RetentionMonths != 12 {
		t.Errorf("RetentionMonths = %d, want 12", as.RetentionMonths)
	}
}

func TestService_Get_PermittedForSuperAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.Get(context.Background(), actor)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
}

func TestService_Get_NotPermittedBelowAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings)
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "user-1"}

	_, _, err := svc.Get(context.Background(), actor)
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("Get() error = %v, want ErrNotPermitted", err)
	}
}

func TestService_Get_MetricsExportFailurePropagates(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{err: errors.New("database unreachable")}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.Get(context.Background(), actor)
	if err == nil {
		t.Fatal("Get() succeeded despite a metricsExport.Get failure")
	}
	if errors.Is(err, rbac.ErrNotPermitted) {
		t.Error("Get() returned ErrNotPermitted for an infrastructure failure")
	}
}

func TestService_Get_AuditSettingsFailurePropagates(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{err: errors.New("database unreachable")}
	svc := NewService(metricsExport, auditSettings)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.Get(context.Background(), actor)
	if err == nil {
		t.Fatal("Get() succeeded despite an auditSettings.Get failure")
	}
	if errors.Is(err, rbac.ErrNotPermitted) {
		t.Error("Get() returned ErrNotPermitted for an infrastructure failure")
	}
}
