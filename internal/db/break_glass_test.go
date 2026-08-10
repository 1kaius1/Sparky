// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"testing"
)

func newTestBreakGlassRepo(t *testing.T) *BreakGlassRepository {
	t.Helper()
	pool := newTestPool(t)
	repo := NewBreakGlassRepository(pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM break_glass_credential`)
	})
	return repo
}

func TestBreakGlassRepository_Get_NotSet(t *testing.T) {
	repo := newTestBreakGlassRepo(t)

	_, err := repo.Get(context.Background())
	if err != ErrBreakGlassNotSet {
		t.Errorf("Get() error = %v, want ErrBreakGlassNotSet", err)
	}
}

func TestBreakGlassRepository_SetAndGet(t *testing.T) {
	repo := newTestBreakGlassRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "argon2id$hash-placeholder"); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.PasswordHash != "argon2id$hash-placeholder" {
		t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, "argon2id$hash-placeholder")
	}
}

func TestBreakGlassRepository_Set_Replaces(t *testing.T) {
	repo := newTestBreakGlassRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "first-hash"); err != nil {
		t.Fatalf("first Set() error: %v", err)
	}
	first, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	if err := repo.Set(ctx, "second-hash"); err != nil {
		t.Fatalf("second Set() error: %v", err)
	}
	second, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	if second.PasswordHash != "second-hash" {
		t.Errorf("PasswordHash after replace = %q, want %q", second.PasswordHash, "second-hash")
	}
	if second.UpdatedAt.Before(first.UpdatedAt) {
		t.Errorf("UpdatedAt went backwards: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
}
