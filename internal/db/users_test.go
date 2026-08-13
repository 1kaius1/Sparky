// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// These are integration tests against a real, migrated Postgres instance -
// see ARCHITECTURE.md Testing Strategy. They require DATABASE_URL to point
// at a database with migrations/000001_create_users applied; see CLAUDE.md
// Database Setup for the disposable-podman recipe. They skip cleanly if
// DATABASE_URL is unset, rather than failing.

func newTestUserRepo(t *testing.T) *UserRepository {
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

	return NewUserRepository(pool)
}

// uniqueADSID avoids collisions between test runs sharing one database.
func uniqueADSID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("S-1-TEST-%s-%d", t.Name(), time.Now().UnixNano())
}

func cleanupUser(t *testing.T, repo *UserRepository, adSID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), `DELETE FROM users WHERE ad_sid = $1`, adSID)
	})
}

func TestUserRepository_CreateAndFindByADSID(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	adSID := uniqueADSID(t)
	cleanupUser(t, repo, adSID)

	created, err := repo.Create(ctx, adSID, "Test User", TierDeveloper)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.ID == "" {
		t.Error("Create() returned an empty ID")
	}
	if created.Tier != TierDeveloper {
		t.Errorf("Create() Tier = %q, want %q", created.Tier, TierDeveloper)
	}
	if created.EntraObjectID != nil {
		t.Errorf("Create() EntraObjectID = %v, want nil", created.EntraObjectID)
	}
	if created.LastLoginAt != nil {
		t.Errorf("Create() LastLoginAt = %v, want nil", created.LastLoginAt)
	}

	found, err := repo.FindByADSID(ctx, adSID)
	if err != nil {
		t.Fatalf("FindByADSID() error: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("FindByADSID() ID = %q, want %q", found.ID, created.ID)
	}
	if found.DisplayName != "Test User" {
		t.Errorf("FindByADSID() DisplayName = %q, want %q", found.DisplayName, "Test User")
	}

	foundByID, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if foundByID.ADSID != adSID {
		t.Errorf("FindByID() ADSID = %q, want %q", foundByID.ADSID, adSID)
	}
}

func TestUserRepository_FindByID_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	_, err := repo.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrUserNotFound {
		t.Errorf("FindByID() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_FindByADSID_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	_, err := repo.FindByADSID(context.Background(), "S-1-DOES-NOT-EXIST")
	if err != ErrUserNotFound {
		t.Errorf("FindByADSID() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_UpdateLastLogin(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	adSID := uniqueADSID(t)
	cleanupUser(t, repo, adSID)

	created, err := repo.Create(ctx, adSID, "Test User", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	loginTime := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdateLastLogin(ctx, created.ID, loginTime); err != nil {
		t.Fatalf("UpdateLastLogin() error: %v", err)
	}

	found, err := repo.FindByADSID(ctx, adSID)
	if err != nil {
		t.Fatalf("FindByADSID() error: %v", err)
	}
	if found.LastLoginAt == nil {
		t.Fatal("LastLoginAt is nil after UpdateLastLogin()")
	}
	if !found.LastLoginAt.Equal(loginTime) {
		t.Errorf("LastLoginAt = %v, want %v", found.LastLoginAt, loginTime)
	}
}

func TestUserRepository_UpdateLastLogin_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	err := repo.UpdateLastLogin(context.Background(), "00000000-0000-0000-0000-000000000000", time.Now())
	if err != ErrUserNotFound {
		t.Errorf("UpdateLastLogin() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_UpdateTier(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()

	adminADSID := uniqueADSID(t) + "-admin"
	cleanupUser(t, repo, adminADSID)
	admin, err := repo.Create(ctx, adminADSID, "Admin User", TierAdmin)
	if err != nil {
		t.Fatalf("Create(admin) error: %v", err)
	}

	targetADSID := uniqueADSID(t) + "-target"
	cleanupUser(t, repo, targetADSID)
	target, err := repo.Create(ctx, targetADSID, "Target User", TierReadOnly)
	if err != nil {
		t.Fatalf("Create(target) error: %v", err)
	}

	elevatedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdateTier(ctx, target.ID, TierPowerDev, &admin.ID, elevatedAt); err != nil {
		t.Fatalf("UpdateTier() error: %v", err)
	}

	found, err := repo.FindByADSID(ctx, targetADSID)
	if err != nil {
		t.Fatalf("FindByADSID() error: %v", err)
	}
	if found.Tier != TierPowerDev {
		t.Errorf("Tier = %q, want %q", found.Tier, TierPowerDev)
	}
	if found.ElevatedBy == nil || *found.ElevatedBy != admin.ID {
		t.Errorf("ElevatedBy = %v, want %q", found.ElevatedBy, admin.ID)
	}
	if found.ElevatedAt == nil || !found.ElevatedAt.Equal(elevatedAt) {
		t.Errorf("ElevatedAt = %v, want %v", found.ElevatedAt, elevatedAt)
	}
}

func TestUserRepository_UpdateTier_NilElevatedBy(t *testing.T) {
	// elevatedBy is nil when the SuperAdmin makes the change - the
	// SuperAdmin is not a Users row, so elevated_by (a nullable FK) must
	// support NULL rather than requiring a value that can't exist.
	repo := newTestUserRepo(t)
	ctx := context.Background()

	targetADSID := uniqueADSID(t)
	cleanupUser(t, repo, targetADSID)
	target, err := repo.Create(ctx, targetADSID, "Target User", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := repo.UpdateTier(ctx, target.ID, TierAdmin, nil, time.Now().UTC()); err != nil {
		t.Fatalf("UpdateTier() error: %v", err)
	}

	found, err := repo.FindByADSID(ctx, targetADSID)
	if err != nil {
		t.Fatalf("FindByADSID() error: %v", err)
	}
	if found.ElevatedBy != nil {
		t.Errorf("ElevatedBy = %v, want nil", *found.ElevatedBy)
	}
	if found.Tier != TierAdmin {
		t.Errorf("Tier = %q, want %q", found.Tier, TierAdmin)
	}
}

func TestUserRepository_UpdateTier_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	elevatedBy := "00000000-0000-0000-0000-000000000000"
	err := repo.UpdateTier(context.Background(), "00000000-0000-0000-0000-000000000000", TierAdmin, &elevatedBy, time.Now())
	if err != ErrUserNotFound {
		t.Errorf("UpdateTier() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_List(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()

	adSIDA := uniqueADSID(t)
	adSIDB := uniqueADSID(t)
	cleanupUser(t, repo, adSIDA)
	cleanupUser(t, repo, adSIDB)

	a, err := repo.Create(ctx, adSIDA, "List Test User A", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	b, err := repo.Create(ctx, adSIDB, "List Test User B", TierDeveloper)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	got, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, u := range got {
		if u.ID == a.ID {
			foundA = true
		}
		if u.ID == b.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d users, missing one or both of the two just created", len(got))
	}
}
