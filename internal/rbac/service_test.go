// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

func testLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

// fakeAuditRecorder implements auditRecorder for tests without a real
// Postgres - same pattern as fakeUserStore.
type fakeAuditRecorder struct {
	recordErr error
	calls     []auditCall
}

type auditCall struct {
	actorID            *string
	isSuperAdminAction bool
	action             string
	objectType         string
	objectID           string
	detail             map[string]any
}

func (f *fakeAuditRecorder) Record(_ context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.calls = append(f.calls, auditCall{actorID, isSuperAdminAction, action, objectType, objectID, detail})
	return nil
}

// fakeUserStore implements userStore for tests without a real Postgres -
// same pattern as internal/httpapi's fakeUserStore.
type fakeUserStore struct {
	byID map[string]*db.User

	findErr       error
	updateTierErr error
	listErr       error

	// updateTierFailAfter delays updateTierErr until this many UpdateTier
	// calls have already succeeded - lets a test simulate the forward tier
	// update succeeding and a later revert call failing. Zero (the
	// default) fails immediately, same pattern as internal/lifecycle's
	// fakeInstanceStore.setStatusFailAfter.
	updateTierFailAfter int
	updateTierCalls     []updateTierCall
}

type updateTierCall struct {
	id         string
	tier       db.Tier
	elevatedBy *string
	elevatedAt *time.Time
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

func (f *fakeUserStore) UpdateTier(_ context.Context, id string, tier db.Tier, elevatedBy *string, elevatedAt *time.Time) error {
	if f.updateTierErr != nil && len(f.updateTierCalls) >= f.updateTierFailAfter {
		return f.updateTierErr
	}
	f.updateTierCalls = append(f.updateTierCalls, updateTierCall{id, tier, elevatedBy, elevatedAt})
	u, ok := f.byID[id]
	if !ok {
		return db.ErrUserNotFound
	}
	u.Tier = tier
	u.ElevatedBy = elevatedBy
	u.ElevatedAt = elevatedAt
	return nil
}

func (f *fakeUserStore) List(_ context.Context) ([]*db.User, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	users := make([]*db.User, 0, len(f.byID))
	for _, u := range f.byID {
		users = append(users, u)
	}
	return users, nil
}

func TestService_ElevateTier_PermittedByAdmin(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	audit := &fakeAuditRecorder{}
	svc := NewService(store, audit, testLogger())
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

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	got := audit.calls[0]
	if got.actorID == nil || *got.actorID != "admin-1" {
		t.Errorf("audit actorID = %v, want %q", got.actorID, "admin-1")
	}
	if got.isSuperAdminAction {
		t.Error("audit isSuperAdminAction = true, want false")
	}
	if got.action != "elevated_user" || got.objectType != "user" || got.objectID != "target-1" {
		t.Errorf("audit call = %+v, want action=elevated_user objectType=user objectID=target-1", got)
	}
	if got.detail["from_tier"] != string(db.TierReadOnly) || got.detail["to_tier"] != string(db.TierDeveloper) {
		t.Errorf("audit detail = %+v, want from_tier=%q to_tier=%q", got.detail, db.TierReadOnly, db.TierDeveloper)
	}
}

func TestService_ElevateTier_PermittedBySuperAdmin_NilElevatedBy(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	audit := &fakeAuditRecorder{}
	svc := NewService(store, audit, testLogger())
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

	// The SuperAdmin's actions are never exempt from the audit log - see
	// ARCHITECTURE.md's "no exceptions" guarantee.
	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	got := audit.calls[0]
	if got.actorID != nil {
		t.Errorf("audit actorID = %v, want nil (SuperAdmin is not a Users row)", *got.actorID)
	}
	if !got.isSuperAdminAction {
		t.Error("audit isSuperAdminAction = false, want true")
	}
}

func TestService_ElevateTier_NotPermitted(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	audit := &fakeAuditRecorder{}
	svc := NewService(store, audit, testLogger())
	actor := Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	// Read-only -> PowerDev skips a step; not even an Admin can do this.
	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierPowerDev)
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("ElevateTier() error = %v, want ErrNotPermitted", err)
	}
	if target.Tier != db.TierReadOnly {
		t.Errorf("Tier = %q, want unchanged %q - a refused elevation must not persist", target.Tier, db.TierReadOnly)
	}
	if len(audit.calls) != 0 {
		t.Errorf("audit.Record called %d times, want 0 - a refused elevation must not be audited as though it happened", len(audit.calls))
	}
}

func TestService_ElevateTier_TargetNotFound(t *testing.T) {
	store := newFakeUserStore()
	audit := &fakeAuditRecorder{}
	svc := NewService(store, audit, testLogger())
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "does-not-exist", db.TierAdmin)
	if !errors.Is(err, db.ErrUserNotFound) {
		t.Errorf("ElevateTier() error = %v, want db.ErrUserNotFound", err)
	}
	if len(audit.calls) != 0 {
		t.Errorf("audit.Record called %d times, want 0", len(audit.calls))
	}
}

func TestService_ElevateTier_UpdateFails(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	store.updateTierErr = errors.New("database unreachable")
	audit := &fakeAuditRecorder{}
	svc := NewService(store, audit, testLogger())
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierDeveloper)
	if err == nil {
		t.Fatal("ElevateTier() succeeded despite an UpdateTier failure")
	}
	if errors.Is(err, ErrNotPermitted) {
		t.Error("ElevateTier() returned ErrNotPermitted for an infrastructure failure")
	}
	if len(audit.calls) != 0 {
		t.Errorf("audit.Record called %d times, want 0 - a tier update that never persisted must not be audited", len(audit.calls))
	}
}

func TestService_ElevateTier_AuditFailurePropagates(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := NewService(store, audit, testLogger())
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierDeveloper)
	if err == nil {
		t.Fatal("ElevateTier() succeeded despite an audit Record failure")
	}
	// The tier change is reverted rather than left unaudited - see
	// ElevateTier's doc comment and PLANNING.md Decisions Log.
	if target.Tier != db.TierReadOnly {
		t.Errorf("Tier = %q, want %q (reverted after the audit write failed)", target.Tier, db.TierReadOnly)
	}
	if target.ElevatedBy != nil {
		t.Errorf("ElevatedBy = %v, want nil (reverted to never-elevated)", *target.ElevatedBy)
	}
	if target.ElevatedAt != nil {
		t.Errorf("ElevatedAt = %v, want nil (reverted to never-elevated)", *target.ElevatedAt)
	}
	if len(store.updateTierCalls) != 2 {
		t.Fatalf("UpdateTier called %d times, want 2 (forward, then revert)", len(store.updateTierCalls))
	}
	if store.updateTierCalls[1].tier != db.TierReadOnly {
		t.Errorf("revert call tier = %q, want %q", store.updateTierCalls[1].tier, db.TierReadOnly)
	}
}

// TestService_ElevateTier_AuditFailure_RevertAlsoFails_StillReturnsAuditError
// confirms the original audit error - not a secondary revert error - is
// what ElevateTier returns when both writes fail, same defensive-failure
// coverage as the running_instances dispatch-recovery fix's own
// *_SetStatusAlsoFails tests.
func TestService_ElevateTier_AuditFailure_RevertAlsoFails_StillReturnsAuditError(t *testing.T) {
	target := &db.User{ID: "target-1", Tier: db.TierReadOnly}
	store := newFakeUserStore(target)
	store.updateTierErr = errors.New("database unreachable")
	store.updateTierFailAfter = 1
	audit := &fakeAuditRecorder{recordErr: errors.New("audit database unreachable")}
	svc := NewService(store, audit, testLogger())
	actor := Actor{IsSuperAdmin: true}

	err := svc.ElevateTier(context.Background(), actor, "target-1", db.TierDeveloper)
	if err == nil {
		t.Fatal("ElevateTier() succeeded despite an audit Record failure")
	}
	if !strings.Contains(err.Error(), "audit database unreachable") {
		t.Errorf("ElevateTier() error = %v, want the original audit error, not the revert failure", err)
	}
}

func TestService_ListUsers_PermittedForAdmin(t *testing.T) {
	store := newFakeUserStore(
		&db.User{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin},
		&db.User{ID: "user-2", DisplayName: "Sam Developer", Tier: db.TierDeveloper},
	)
	svc := NewService(store, &fakeAuditRecorder{}, testLogger())
	actor := Actor{Tier: db.TierAdmin, UserID: "user-1"}

	users, err := svc.ListUsers(context.Background(), actor)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(users))
	}
}

func TestService_ListUsers_PermittedForSuperAdmin(t *testing.T) {
	store := newFakeUserStore(&db.User{ID: "user-1", Tier: db.TierReadOnly})
	svc := NewService(store, &fakeAuditRecorder{}, testLogger())
	actor := Actor{IsSuperAdmin: true}

	users, err := svc.ListUsers(context.Background(), actor)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("len(users) = %d, want 1", len(users))
	}
}

func TestService_ListUsers_NotPermittedBelowAdmin(t *testing.T) {
	store := newFakeUserStore(&db.User{ID: "user-1", Tier: db.TierPowerDev})
	svc := NewService(store, &fakeAuditRecorder{}, testLogger())
	actor := Actor{Tier: db.TierPowerDev, UserID: "user-1"}

	_, err := svc.ListUsers(context.Background(), actor)
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("ListUsers() error = %v, want ErrNotPermitted", err)
	}
}

func TestService_ListUsers_StoreFailurePropagates(t *testing.T) {
	store := newFakeUserStore()
	store.listErr = errors.New("database unreachable")
	svc := NewService(store, &fakeAuditRecorder{}, testLogger())
	actor := Actor{IsSuperAdmin: true}

	_, err := svc.ListUsers(context.Background(), actor)
	if err == nil {
		t.Fatal("ListUsers() succeeded despite a store List failure")
	}
	if errors.Is(err, ErrNotPermitted) {
		t.Error("ListUsers() returned ErrNotPermitted for an infrastructure failure")
	}
}
