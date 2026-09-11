// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// These are integration tests against a real, migrated Postgres instance -
// see ARCHITECTURE.md Testing Strategy. They require DATABASE_URL to point
// at a database with migrations/000026_create_theme_settings applied; see
// CLAUDE.md Database Setup for the disposable-podman recipe. They skip
// cleanly if DATABASE_URL is unset, rather than failing.

// jsonEqual compares two JSON values structurally rather than as raw
// bytes - Postgres's jsonb round-trip can re-serialize whitespace (e.g.
// inserting a space after ":"), so byte-for-byte comparison is brittle
// against jsonb columns specifically.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	aEncoded, _ := json.Marshal(av)
	bEncoded, _ := json.Marshal(bv)
	return string(aEncoded) == string(bEncoded)
}

func newTestThemeSettingsRepo(t *testing.T) *ThemeSettingsRepository {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}

	pool, err := New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewThemeSettingsRepository(pool)
}

func TestThemeSettingsRepository_Get_SeededDefault(t *testing.T) {
	repo := newTestThemeSettingsRepo(t)

	got, err := repo.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	// Migration 000026 seeds exactly one row - this test runs against a
	// freshly migrated database and never itself resets the row, so a
	// prior test in the same run may have already changed it; only assert
	// the row exists and scans cleanly, not its specific value.
	if got.DefaultTheme == "" {
		t.Error("DefaultTheme is empty, want a seeded value")
	}
}

func TestThemeSettingsRepository_Update(t *testing.T) {
	repo := newTestThemeSettingsRepo(t)
	ctx := context.Background()

	// updated_by has a real FK to users.id, so a genuine user row is
	// needed rather than an arbitrary UUID.
	userRepo := NewUserRepository(repo.pool)
	adSID := uniqueADSID(t)
	cleanupUser(t, userRepo, adSID)
	admin, err := userRepo.Create(ctx, adSID, "Theme Settings Test Admin", "CN=themeadmin,DC=example,DC=internal", TierAdmin)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	statusPalette := ThemeStatusPaletteDark
	colors := json.RawMessage(`{"--color-primary":"#8b5cf6"}`)

	if err := repo.Update(ctx, ThemePresetAmethystDark, colors, &statusPalette, &admin.ID); err != nil {
		t.Fatalf("Update() error: %v", err)
	}

	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.DefaultTheme != ThemePresetAmethystDark {
		t.Errorf("DefaultTheme = %q, want %q", got.DefaultTheme, ThemePresetAmethystDark)
	}
	if got.DefaultStatusPalette == nil || *got.DefaultStatusPalette != statusPalette {
		t.Errorf("DefaultStatusPalette = %v, want %q", got.DefaultStatusPalette, statusPalette)
	}
	if !jsonEqual(t, got.DefaultCustomColors, colors) {
		t.Errorf("DefaultCustomColors = %s, want %s", got.DefaultCustomColors, colors)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != admin.ID {
		t.Errorf("UpdatedBy = %v, want %q", got.UpdatedBy, admin.ID)
	}

	// Restore a clean, nil-updatedBy state (the break-glass SuperAdmin
	// shape) so this test doesn't leave a dangling FK-referencing value
	// behind for later tests/runs sharing the same database.
	if err := repo.Update(ctx, ThemePresetCarbonDark, json.RawMessage(`{}`), nil, nil); err != nil {
		t.Fatalf("Update() (restore) error: %v", err)
	}
	restored, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if restored.DefaultStatusPalette != nil {
		t.Errorf("DefaultStatusPalette = %v, want nil after restoring", *restored.DefaultStatusPalette)
	}
	if restored.UpdatedBy != nil {
		t.Errorf("UpdatedBy = %v, want nil after restoring", *restored.UpdatedBy)
	}
}
