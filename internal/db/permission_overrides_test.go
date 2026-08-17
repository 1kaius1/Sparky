// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// See users_test.go for why these integration tests skip cleanly instead
// of failing when DATABASE_URL is unset.

func newTestPool(t *testing.T) *pgxpool.Pool {
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

	return pool
}

// createTestUser creates a throwaway user to satisfy permission_overrides'
// foreign keys, cleaned up (and its overrides, via ON DELETE CASCADE-free
// FK - deleted explicitly) after the test.
func createTestUser(t *testing.T, users *UserRepository, adSID string) *User {
	t.Helper()
	u, err := users.Create(context.Background(), adSID, "Test User", "CN=test,DC=example,DC=internal", TierReadOnly)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = users.pool.Exec(context.Background(), `DELETE FROM permission_overrides WHERE user_id = $1 OR granted_by = $1`, u.ID)
		_, _ = users.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

func TestPermissionOverrideRepository_GrantAndGet(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	overrides := NewPermissionOverrideRepository(pool)
	ctx := context.Background()

	grantee := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-grantee", t.Name()))
	admin := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-admin", t.Name()))

	granted, err := overrides.Grant(ctx, grantee.ID, CapabilityManageModelStore, admin.ID)
	if err != nil {
		t.Fatalf("Grant() error: %v", err)
	}
	if granted.Capability != CapabilityManageModelStore {
		t.Errorf("Capability = %q, want %q", granted.Capability, CapabilityManageModelStore)
	}
	if granted.GrantedBy != admin.ID {
		t.Errorf("GrantedBy = %q, want %q", granted.GrantedBy, admin.ID)
	}

	got, err := overrides.Get(ctx, grantee.ID, CapabilityManageModelStore)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.UserID != grantee.ID {
		t.Errorf("UserID = %q, want %q", got.UserID, grantee.ID)
	}
}

func TestPermissionOverrideRepository_Get_NotFound(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	overrides := NewPermissionOverrideRepository(pool)
	ctx := context.Background()

	user := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))

	_, err := overrides.Get(ctx, user.ID, CapabilityManageModelStore)
	if err != ErrPermissionOverrideNotFound {
		t.Errorf("Get() error = %v, want ErrPermissionOverrideNotFound", err)
	}
}

func TestPermissionOverrideRepository_Grant_Regrant(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	overrides := NewPermissionOverrideRepository(pool)
	ctx := context.Background()

	grantee := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-grantee", t.Name()))
	admin1 := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-admin1", t.Name()))
	admin2 := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-admin2", t.Name()))

	if _, err := overrides.Grant(ctx, grantee.ID, CapabilityManageModelStore, admin1.ID); err != nil {
		t.Fatalf("first Grant() error: %v", err)
	}

	// Granting the same capability again (e.g. a different admin
	// re-confirms it) should update, not create a duplicate row - the
	// (user_id, capability) primary key would otherwise conflict.
	regranted, err := overrides.Grant(ctx, grantee.ID, CapabilityManageModelStore, admin2.ID)
	if err != nil {
		t.Fatalf("second Grant() error: %v", err)
	}
	if regranted.GrantedBy != admin2.ID {
		t.Errorf("GrantedBy after regrant = %q, want %q", regranted.GrantedBy, admin2.ID)
	}
}

func TestPermissionOverrideRepository_Revoke(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	overrides := NewPermissionOverrideRepository(pool)
	ctx := context.Background()

	grantee := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-grantee", t.Name()))
	admin := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-admin", t.Name()))

	if _, err := overrides.Grant(ctx, grantee.ID, CapabilityManageModelStore, admin.ID); err != nil {
		t.Fatalf("Grant() error: %v", err)
	}

	if err := overrides.Revoke(ctx, grantee.ID, CapabilityManageModelStore); err != nil {
		t.Fatalf("Revoke() error: %v", err)
	}

	_, err := overrides.Get(ctx, grantee.ID, CapabilityManageModelStore)
	if err != ErrPermissionOverrideNotFound {
		t.Errorf("Get() after Revoke() error = %v, want ErrPermissionOverrideNotFound", err)
	}
}

func TestPermissionOverrideRepository_Revoke_NeverGranted(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	overrides := NewPermissionOverrideRepository(pool)
	ctx := context.Background()

	user := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))

	if err := overrides.Revoke(ctx, user.ID, CapabilityManageModelStore); err != nil {
		t.Errorf("Revoke() of a never-granted capability returned an error: %v", err)
	}
}
