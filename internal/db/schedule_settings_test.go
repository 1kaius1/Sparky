// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
)

func TestScheduleSettingsRepository_GetUpdateAndBlankRefused(t *testing.T) {
	pool := newTestPool(t)
	repo := NewScheduleSettingsRepository(pool)
	ctx := context.Background()

	before, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Update(ctx, before.Timezone, before.UpdatedBy) })

	if err := repo.Update(ctx, "America/New_York", nil); err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	got, err := repo.Get(ctx)
	if err != nil || got.Timezone != "America/New_York" || got.UpdatedBy != nil {
		t.Fatalf("after update: %+v, %v", got, err)
	}
	for _, bad := range []string{"", "   "} {
		if err := repo.Update(ctx, bad, nil); err == nil {
			t.Errorf("Update(%q) was accepted; the CHECK constraint should refuse it", bad)
		}
	}
	if got, _ := repo.Get(ctx); got.Timezone != "America/New_York" {
		t.Errorf("a refused update changed the value to %q", got.Timezone)
	}
}
