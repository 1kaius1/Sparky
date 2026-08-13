// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
)

func newTestAuditSettingsRepo(t *testing.T) *AuditSettingsRepository {
	t.Helper()
	pool := newTestPool(t)
	return NewAuditSettingsRepository(pool)
}

// TestAuditSettingsRepository_Get_SeededDefault confirms migration
// 000013's INSERT actually landed with the defaults documented on that
// migration - retention_months=12 (PLANNING.md Decisions Log,
// 2026-08-12), forwarding disabled, syslog protocol (SCHEMA.md's own
// stated default).
func TestAuditSettingsRepository_Get_SeededDefault(t *testing.T) {
	repo := newTestAuditSettingsRepo(t)

	got, err := repo.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.RetentionMonths != 12 {
		t.Errorf("RetentionMonths = %d, want 12 (the seeded default)", got.RetentionMonths)
	}
	if got.ForwardingEnabled {
		t.Error("ForwardingEnabled = true, want false for the seeded row")
	}
	if got.ForwardingProtocol != AuditForwardingSyslog {
		t.Errorf("ForwardingProtocol = %q, want %q (the seeded default)", got.ForwardingProtocol, AuditForwardingSyslog)
	}
	if got.ForwardingHost != nil {
		t.Errorf("ForwardingHost = %v, want nil for the seeded row", *got.ForwardingHost)
	}
}
