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

// uniqueLocalUsername avoids collisions between test runs sharing one
// database - the local-account equivalent of uniqueADSID.
func uniqueLocalUsername(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

func cleanupLocalUser(t *testing.T, repo *UserRepository, username string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), `DELETE FROM users WHERE local_username = $1`, username)
	})
}

func TestUserRepository_CreateAndFindByADSID(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	adSID := uniqueADSID(t)
	cleanupUser(t, repo, adSID)

	const dn = "CN=test,DC=example,DC=internal"
	created, err := repo.Create(ctx, adSID, "Test User", dn, TierDeveloper)
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
	if created.LDAPDN == nil || *created.LDAPDN != dn {
		t.Errorf("Create() LDAPDN = %v, want %q", created.LDAPDN, dn)
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
	if found.LDAPDN == nil || *found.LDAPDN != dn {
		t.Errorf("FindByADSID() LDAPDN = %v, want %q", found.LDAPDN, dn)
	}

	foundByID, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if foundByID.ADSID == nil || *foundByID.ADSID != adSID {
		t.Errorf("FindByID() ADSID = %v, want %q", foundByID.ADSID, adSID)
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

	created, err := repo.Create(ctx, adSID, "Test User", "CN=old,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	loginTime := time.Now().UTC().Truncate(time.Microsecond)
	const newDN = "CN=new,DC=example,DC=internal"
	if err := repo.UpdateLastLogin(ctx, created.ID, newDN, loginTime); err != nil {
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
	// UpdateLastLogin refreshes the cached DN too, not just the timestamp -
	// confirms it actually overwrites the value Create set, not just
	// populates it once.
	if found.LDAPDN == nil || *found.LDAPDN != newDN {
		t.Errorf("LDAPDN = %v, want %q (refreshed by UpdateLastLogin)", found.LDAPDN, newDN)
	}
}

func TestUserRepository_UpdateLastLogin_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	err := repo.UpdateLastLogin(context.Background(), "00000000-0000-0000-0000-000000000000", "CN=test,DC=example,DC=internal", time.Now())
	if err != ErrUserNotFound {
		t.Errorf("UpdateLastLogin() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_UpdateTier(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()

	adminADSID := uniqueADSID(t) + "-admin"
	cleanupUser(t, repo, adminADSID)
	admin, err := repo.Create(ctx, adminADSID, "Admin User", "CN=admin,DC=example,DC=internal", TierAdmin)
	if err != nil {
		t.Fatalf("Create(admin) error: %v", err)
	}

	targetADSID := uniqueADSID(t) + "-target"
	cleanupUser(t, repo, targetADSID)
	target, err := repo.Create(ctx, targetADSID, "Target User", "CN=target,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("Create(target) error: %v", err)
	}

	elevatedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdateTier(ctx, target.ID, TierPowerDev, &admin.ID, &elevatedAt); err != nil {
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
	target, err := repo.Create(ctx, targetADSID, "Target User", "CN=target,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	now := time.Now().UTC()
	if err := repo.UpdateTier(ctx, target.ID, TierAdmin, nil, &now); err != nil {
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

func TestUserRepository_UpdateTier_NilElevatedAt(t *testing.T) {
	// A revert to a user's pre-elevation state (rbac.Service.ElevateTier,
	// on an audit-write failure) needs to be able to restore NULL for a
	// user who was never previously elevated, not just a real timestamp.
	repo := newTestUserRepo(t)
	ctx := context.Background()

	adminADSID := uniqueADSID(t) + "-admin"
	cleanupUser(t, repo, adminADSID)
	admin, err := repo.Create(ctx, adminADSID, "Admin User", "CN=admin,DC=example,DC=internal", TierAdmin)
	if err != nil {
		t.Fatalf("Create(admin) error: %v", err)
	}

	targetADSID := uniqueADSID(t) + "-target"
	cleanupUser(t, repo, targetADSID)
	target, err := repo.Create(ctx, targetADSID, "Target User", "CN=target,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("Create(target) error: %v", err)
	}

	// First elevate with a real timestamp, then revert with a nil one -
	// confirms the nil case actually clears a previously-set value rather
	// than just never having been set.
	elevatedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdateTier(ctx, target.ID, TierPowerDev, &admin.ID, &elevatedAt); err != nil {
		t.Fatalf("UpdateTier() (elevate) error: %v", err)
	}
	if err := repo.UpdateTier(ctx, target.ID, TierReadOnly, nil, nil); err != nil {
		t.Fatalf("UpdateTier() (revert) error: %v", err)
	}

	found, err := repo.FindByADSID(ctx, targetADSID)
	if err != nil {
		t.Fatalf("FindByADSID() error: %v", err)
	}
	if found.Tier != TierReadOnly {
		t.Errorf("Tier = %q, want %q", found.Tier, TierReadOnly)
	}
	if found.ElevatedBy != nil {
		t.Errorf("ElevatedBy = %v, want nil", *found.ElevatedBy)
	}
	if found.ElevatedAt != nil {
		t.Errorf("ElevatedAt = %v, want nil", *found.ElevatedAt)
	}
}

func TestUserRepository_UpdateTier_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	elevatedBy := "00000000-0000-0000-0000-000000000000"
	now := time.Now()
	err := repo.UpdateTier(context.Background(), "00000000-0000-0000-0000-000000000000", TierAdmin, &elevatedBy, &now)
	if err != ErrUserNotFound {
		t.Errorf("UpdateTier() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_CreateLocalAndFindByLocalUsername(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	username := uniqueLocalUsername(t)
	cleanupLocalUser(t, repo, username)

	created, err := repo.CreateLocal(ctx, username, "argon2id$fake-hash", "Test Local User", TierDeveloper)
	if err != nil {
		t.Fatalf("CreateLocal() error: %v", err)
	}
	if created.ID == "" {
		t.Error("CreateLocal() returned an empty ID")
	}
	if created.ADSID != nil {
		t.Errorf("CreateLocal() ADSID = %v, want nil", *created.ADSID)
	}
	if created.LocalUsername == nil || *created.LocalUsername != username {
		t.Errorf("CreateLocal() LocalUsername = %v, want %q", created.LocalUsername, username)
	}

	found, hash, err := repo.FindByLocalUsername(ctx, username)
	if err != nil {
		t.Fatalf("FindByLocalUsername() error: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("FindByLocalUsername() ID = %q, want %q", found.ID, created.ID)
	}
	if hash != "argon2id$fake-hash" {
		t.Errorf("FindByLocalUsername() hash = %q, want %q", hash, "argon2id$fake-hash")
	}
}

func TestUserRepository_FindByLocalUsername_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	_, _, err := repo.FindByLocalUsername(context.Background(), "does-not-exist")
	if err != ErrUserNotFound {
		t.Errorf("FindByLocalUsername() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_CreateLocal_DuplicateUsername(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	username := uniqueLocalUsername(t)
	cleanupLocalUser(t, repo, username)

	if _, err := repo.CreateLocal(ctx, username, "hash-one", "First", TierReadOnly); err != nil {
		t.Fatalf("CreateLocal() first call error: %v", err)
	}

	_, err := repo.CreateLocal(ctx, username, "hash-two", "Second", TierReadOnly)
	if err != ErrLocalUsernameTaken {
		t.Errorf("CreateLocal() error = %v, want ErrLocalUsernameTaken", err)
	}
}

func TestUserRepository_UpdateDisplayName(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	username := uniqueLocalUsername(t)
	cleanupLocalUser(t, repo, username)

	created, err := repo.CreateLocal(ctx, username, "hash", "Old Name", TierReadOnly)
	if err != nil {
		t.Fatalf("CreateLocal() error: %v", err)
	}

	if err := repo.UpdateDisplayName(ctx, created.ID, "New Name"); err != nil {
		t.Fatalf("UpdateDisplayName() error: %v", err)
	}

	found, _, err := repo.FindByLocalUsername(ctx, username)
	if err != nil {
		t.Fatalf("FindByLocalUsername() error: %v", err)
	}
	if found.DisplayName != "New Name" {
		t.Errorf("DisplayName = %q, want %q", found.DisplayName, "New Name")
	}
}

func TestUserRepository_UpdateDisplayName_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	err := repo.UpdateDisplayName(context.Background(), "00000000-0000-0000-0000-000000000000", "New Name")
	if err != ErrUserNotFound {
		t.Errorf("UpdateDisplayName() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_UpdateLocalPassword(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()
	username := uniqueLocalUsername(t)
	cleanupLocalUser(t, repo, username)

	created, err := repo.CreateLocal(ctx, username, "old-hash", "Test Local User", TierReadOnly)
	if err != nil {
		t.Fatalf("CreateLocal() error: %v", err)
	}

	if err := repo.UpdateLocalPassword(ctx, created.ID, "new-hash"); err != nil {
		t.Fatalf("UpdateLocalPassword() error: %v", err)
	}

	_, hash, err := repo.FindByLocalUsername(ctx, username)
	if err != nil {
		t.Fatalf("FindByLocalUsername() error: %v", err)
	}
	if hash != "new-hash" {
		t.Errorf("hash = %q, want %q", hash, "new-hash")
	}
}

func TestUserRepository_UpdateLocalPassword_NotFound(t *testing.T) {
	repo := newTestUserRepo(t)

	err := repo.UpdateLocalPassword(context.Background(), "00000000-0000-0000-0000-000000000000", "new-hash")
	if err != ErrUserNotFound {
		t.Errorf("UpdateLocalPassword() error = %v, want ErrUserNotFound", err)
	}
}

func TestUserRepository_List(t *testing.T) {
	repo := newTestUserRepo(t)
	ctx := context.Background()

	adSIDA := uniqueADSID(t)
	adSIDB := uniqueADSID(t)
	cleanupUser(t, repo, adSIDA)
	cleanupUser(t, repo, adSIDB)

	a, err := repo.Create(ctx, adSIDA, "List Test User A", "CN=usera,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	b, err := repo.Create(ctx, adSIDB, "List Test User B", "CN=userb,DC=example,DC=internal", TierDeveloper)
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
