// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
)

func TestContainerLogSettingsRepository_GetUpdateAndRange(t *testing.T) {
	pool := newTestPool(t)
	repo := NewContainerLogSettingsRepository(pool)
	ctx := context.Background()

	before, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Update(ctx, before.RetentionMonths, before.UpdatedBy) })

	if err := repo.Update(ctx, 7, nil); err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	got, err := repo.Get(ctx)
	if err != nil || got.RetentionMonths != 7 || got.UpdatedBy != nil {
		t.Fatalf("after update: %+v, %v", got, err)
	}
	for _, bad := range []int{0, -1, 25, 1000} {
		if err := repo.Update(ctx, bad, nil); err == nil {
			t.Errorf("Update(%d) was accepted; the CHECK constraint should refuse it", bad)
		}
	}
	if got, _ := repo.Get(ctx); got.RetentionMonths != 7 {
		t.Errorf("a refused update changed the value to %d", got.RetentionMonths)
	}
}
