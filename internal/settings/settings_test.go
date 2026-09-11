// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"context"
	"encoding/json"
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

// fakeThemeSettingsStore implements themeSettingsStore for tests without a
// real Postgres - same pattern as fakeMetricsExportStore/
// fakeAuditSettingsStore.
type fakeThemeSettingsStore struct {
	settings  *db.ThemeSettings
	getErr    error
	updateErr error

	// updateFailAfter delays updateErr until this many Update calls have
	// already succeeded - lets a test simulate the forward write succeeding
	// and a later revert call failing, same pattern as internal/rbac's
	// fakeUserStore.updateTierFailAfter.
	updateFailAfter int
	updateCalls     []themeSettingsUpdateCall
}

type themeSettingsUpdateCall struct {
	defaultTheme        db.ThemePreset
	defaultCustomColors json.RawMessage
	statusPalette       *db.ThemeStatusPalette
	updatedBy           *string
}

func (f *fakeThemeSettingsStore) Get(context.Context) (*db.ThemeSettings, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.settings, nil
}

func (f *fakeThemeSettingsStore) Update(_ context.Context, defaultTheme db.ThemePreset, defaultCustomColors json.RawMessage, statusPalette *db.ThemeStatusPalette, updatedBy *string) error {
	f.updateCalls = append(f.updateCalls, themeSettingsUpdateCall{defaultTheme, defaultCustomColors, statusPalette, updatedBy})
	if f.updateErr != nil && len(f.updateCalls) > f.updateFailAfter {
		return f.updateErr
	}
	f.settings = &db.ThemeSettings{DefaultTheme: defaultTheme, DefaultCustomColors: defaultCustomColors, DefaultStatusPalette: statusPalette, UpdatedBy: updatedBy}
	return nil
}

// fakeAuditRecorder implements auditRecorder for tests without a real
// Postgres - same pattern as internal/rbac's own copy.
type fakeAuditRecorder struct {
	recordErr error
	calls     []auditCall
}

type auditCall struct {
	actorID            *string
	isSuperAdminAction bool
	action             string
	objectType         string
	objectID           string
	detail             map[string]any
}

func (f *fakeAuditRecorder) Record(_ context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.calls = append(f.calls, auditCall{actorID, isSuperAdminAction, action, objectType, objectID, detail})
	return nil
}

func newTestThemeSettingsStore() *fakeThemeSettingsStore {
	return &fakeThemeSettingsStore{settings: &db.ThemeSettings{DefaultTheme: db.ThemePresetCarbonDark, DefaultCustomColors: []byte(`{}`)}}
}

func TestService_Get_PermittedForAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog}}
	svc := NewService(metricsExport, auditSettings, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	mec, as, ts, err := svc.Get(context.Background(), actor)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if mec.BackendType != db.MetricsExportBackendNone {
		t.Errorf("BackendType = %q, want %q", mec.BackendType, db.MetricsExportBackendNone)
	}
	if as.RetentionMonths != 12 {
		t.Errorf("RetentionMonths = %d, want 12", as.RetentionMonths)
	}
	if ts.DefaultTheme != db.ThemePresetCarbonDark {
		t.Errorf("DefaultTheme = %q, want %q", ts.DefaultTheme, db.ThemePresetCarbonDark)
	}
}

func TestService_Get_PermittedForSuperAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, _, err := svc.Get(context.Background(), actor)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
}

func TestService_Get_NotPermittedBelowAdmin(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "user-1"}

	_, _, _, err := svc.Get(context.Background(), actor)
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("Get() error = %v, want ErrNotPermitted", err)
	}
}

func TestService_Get_MetricsExportFailurePropagates(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{err: errors.New("database unreachable")}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	svc := NewService(metricsExport, auditSettings, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, _, err := svc.Get(context.Background(), actor)
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
	svc := NewService(metricsExport, auditSettings, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, _, err := svc.Get(context.Background(), actor)
	if err == nil {
		t.Fatal("Get() succeeded despite an auditSettings.Get failure")
	}
	if errors.Is(err, rbac.ErrNotPermitted) {
		t.Error("Get() returned ErrNotPermitted for an infrastructure failure")
	}
}

func TestService_Get_ThemeSettingsFailurePropagates(t *testing.T) {
	metricsExport := &fakeMetricsExportStore{config: &db.MetricsExportConfig{}}
	auditSettings := &fakeAuditSettingsStore{settings: &db.AuditSettings{}}
	themeSettings := &fakeThemeSettingsStore{getErr: errors.New("database unreachable")}
	svc := NewService(metricsExport, auditSettings, themeSettings, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, _, err := svc.Get(context.Background(), actor)
	if err == nil {
		t.Fatal("Get() succeeded despite a themeSettings.Get failure")
	}
	if errors.Is(err, rbac.ErrNotPermitted) {
		t.Error("Get() returned ErrNotPermitted for an infrastructure failure")
	}
}

func TestService_UpdateDefaultTheme_PermittedForAdmin(t *testing.T) {
	themeSettings := newTestThemeSettingsStore()
	audit := &fakeAuditRecorder{}
	svc := NewService(&fakeMetricsExportStore{}, &fakeAuditSettingsStore{}, themeSettings, audit)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	err := svc.UpdateDefaultTheme(context.Background(), actor, db.ThemePresetTronDark, nil, nil)
	if err != nil {
		t.Fatalf("UpdateDefaultTheme() error: %v", err)
	}
	if themeSettings.settings.DefaultTheme != db.ThemePresetTronDark {
		t.Errorf("DefaultTheme = %q, want %q", themeSettings.settings.DefaultTheme, db.ThemePresetTronDark)
	}
	if len(audit.calls) != 1 || audit.calls[0].action != "updated_default_theme" {
		t.Errorf("audit calls = %+v, want one updated_default_theme call", audit.calls)
	}
}

func TestService_UpdateDefaultTheme_NotPermittedBelowAdmin(t *testing.T) {
	svc := NewService(&fakeMetricsExportStore{}, &fakeAuditSettingsStore{}, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "user-1"}

	err := svc.UpdateDefaultTheme(context.Background(), actor, db.ThemePresetTronDark, nil, nil)
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("UpdateDefaultTheme() error = %v, want ErrNotPermitted", err)
	}
}

func TestService_UpdateDefaultTheme_UnknownPreset(t *testing.T) {
	svc := NewService(&fakeMetricsExportStore{}, &fakeAuditSettingsStore{}, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	err := svc.UpdateDefaultTheme(context.Background(), actor, db.ThemePreset("not-a-real-preset"), nil, nil)
	if !errors.Is(err, rbac.ErrInvalidTheme) {
		t.Errorf("UpdateDefaultTheme() error = %v, want ErrInvalidTheme", err)
	}
}

func TestService_UpdateDefaultTheme_UnknownColorKey(t *testing.T) {
	svc := NewService(&fakeMetricsExportStore{}, &fakeAuditSettingsStore{}, newTestThemeSettingsStore(), &fakeAuditRecorder{})
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	err := svc.UpdateDefaultTheme(context.Background(), actor, db.ThemePresetCarbonDark, map[string]string{"--color-status-failed": "#ff0000"}, nil)
	if !errors.Is(err, rbac.ErrInvalidTheme) {
		t.Errorf("UpdateDefaultTheme() error = %v, want ErrInvalidTheme (status colors are locked)", err)
	}
}

func TestService_UpdateDefaultTheme_AuditFailure_Reverts(t *testing.T) {
	themeSettings := newTestThemeSettingsStore()
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := NewService(&fakeMetricsExportStore{}, &fakeAuditSettingsStore{}, themeSettings, audit)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	err := svc.UpdateDefaultTheme(context.Background(), actor, db.ThemePresetTronDark, nil, nil)
	if err == nil {
		t.Fatal("UpdateDefaultTheme() succeeded despite an audit Record failure")
	}
	if themeSettings.settings.DefaultTheme != db.ThemePresetCarbonDark {
		t.Errorf("DefaultTheme = %q, want %q (reverted after the audit write failed)", themeSettings.settings.DefaultTheme, db.ThemePresetCarbonDark)
	}
	if len(themeSettings.updateCalls) != 2 {
		t.Fatalf("Update called %d times, want 2 (forward, then revert)", len(themeSettings.updateCalls))
	}
}
