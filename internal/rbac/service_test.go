// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// fakeUserStore implements userStore for tests without a real Postgres -
// same pattern as internal/httpapi's fakeUserStore.
type fakeUserStore struct {
	byID map[string]*db.User

	findErr       error
	updateTierErr error
}

func newFakeUserStore(users ...*db.User) *fakeUserStore {
	byID := make(map[string]*db.User)
	for _, u := range users {
		byID[u.ID] = u
	}
	return &fakeUserStore{byID: byID}
}

func (f *fakeUserStore) FindByID(_ context.Context, id string) (*db.User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, db.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeUserStore) UpdateTier(_ context.Context, id string, tier db.Tier, elevatedBy *string, at time.Time) error {
	if f.updateTierErr != nil {
		return f.updateTierErr
	}
	u, ok := f.byID[id]
	if !ok {
		return db.ErrUserNotFound
	}
	u.Tier = tier
	u.ElevatedBy = elevatedBy
	u.ElevatedAt = &at
	return nil
}

func TestService_ElevateTier_PermittedByAdmin(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	svc := NewService(store)
	actor := Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierDeveloper)
	if err != nil {
		t.Fatalf("ElevateTier() error: %v", err)
	}
	if target.Tier != db.TierDeveloper {
		t.Errorf("Tier = %q, want %q", target.Tier, db.TierDeveloper)
	}
	if target.ElevatedBy == nil || *target.ElevatedBy != "admin-1" {
		t.Errorf("ElevatedBy = %v, want %q", target.ElevatedBy, "admin-1")
	}
}

func TestService_ElevateTier_PermittedBySuperAdmin_NilElevatedBy(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	svc := NewService(store)
	actor := Actor{IsSuperAdmin: true}

	// SuperAdmin can jump straight to Admin - something no regular Admin
	// actor could ever do, per CanElevate's rules.
	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierAdmin)
	if err != nil {
		t.Fatalf("ElevateTier() error: %v", err)
	}
	if target.Tier != db.TierAdmin {
		t.Errorf("Tier = %q, want %q", target.Tier, db.TierAdmin)
	}
	if target.ElevatedBy != nil {
		t.Errorf("ElevatedBy = %v, want nil (SuperAdmin is not a Users row)", *target.ElevatedBy)
	}
}

func TestService_ElevateTier_NotPermitted(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	svc := NewService(store)
	actor := Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	// Read-only -> PowerDev skips a step; not even an Admin can do this.
	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierPowerDev)
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("ElevateTier() error = %v, want ErrNotPermitted", err)
	}
	if target.Tier != db.TierReadOnly {
		t.Errorf("Tier = %q, want unchanged %q - a refused elevation must not persist", target.Tier, db.TierReadOnly)
	}
}

func TestService_ElevateTier_TargetNotFound(t *testing.T) {
	store := newFakeUserStore()
	svc := NewService(store)
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "does-not-exist", db.TierAdmin)
	if !errors.Is(err, db.ErrUserNotFound) {
		t.Errorf("ElevateTier() error = %v, want db.ErrUserNotFound", err)
	}
}

func TestService_ElevateTier_UpdateFails(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	store.updateTierErr = errors.New("database unreachable")
	svc := NewService(store)
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierDeveloper)
	if err == nil {
		t.Fatal("ElevateTier() succeeded despite an UpdateTier failure")
	}
	if errors.Is(err, ErrNotPermitted) {
		t.Error("ElevateTier() returned ErrNotPermitted for an infrastructure failure")
	}
}
