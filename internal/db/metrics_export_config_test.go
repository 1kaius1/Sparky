// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
)

func newTestMetricsExportConfigRepo(t *testing.T) *MetricsExportConfigRepository {
	t.Helper()
	pool := newTestPool(t)
	return NewMetricsExportConfigRepository(pool)
}

// TestMetricsExportConfigRepository_Get_SeededDefault confirms migration
// 000012's INSERT actually landed - the singleton row is always present,
// unlike break_glass_credential, so Get should never see ErrNoRows in a
// correctly migrated database.
func TestMetricsExportConfigRepository_Get_SeededDefault(t *testing.T) {
	repo := newTestMetricsExportConfigRepo(t)

	got, err := repo.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.BackendType != MetricsExportBackendNone {
		t.Errorf("BackendType = %q, want %q (the seeded default)", got.BackendType, MetricsExportBackendNone)
	}
	if got.UpdatedBy != nil {
		t.Errorf("UpdatedBy = %v, want nil for the seeded row", *got.UpdatedBy)
	}
}
