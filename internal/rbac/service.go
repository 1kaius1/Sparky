// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrNotPermitted is returned when an actor attempts an action CanElevate
// or CanManageModelStore would refuse.
var ErrNotPermitted = errors.New("not permitted")

// userStore is the subset of *db.UserRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/httpapi's userStore and internal/auth's ldapConn.
type userStore interface {
	FindByID(ctx context.Context, id string) (*db.User, error)
	UpdateTier(ctx context.Context, id string, tier db.Tier, elevatedBy *string, elevatedAt time.Time) error
	List(ctx context.Context) ([]*db.User, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as userStore. Defined here rather than importing internal/audit
// directly, matching how the rest of this codebase keeps dependency
// interfaces local to the consuming package.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is the RBAC orchestration layer: check a rule, then persist the
// decision. See CLAUDE.md's Handler -> Service Layer -> Repository
// pattern - callers should never call UserRepository.UpdateTier directly;
// this is the only path a tier change should take.
type Service struct {
	users userStore
	audit auditRecorder
}

// NewService constructs a Service.
func NewService(users userStore, audit auditRecorder) *Service {
	return &Service{users: users, audit: audit}
}

// ElevateTier changes targetUserID's tier to toTier, if actor is permitted
// to. The target's current tier is looked up fresh rather than trusted
// from the caller, so the rule check is always against real state.
//
// A permitted elevation is always audited ("elevated_user" - see
// SCHEMA.md Audit log) after it persists, including when actor is the
// SuperAdmin - see ARCHITECTURE.md's "no exceptions" audit guarantee. An
// audit write failure is returned like any other error: the tier change
// has already been persisted at that point (this package does not use a
// database transaction spanning both writes - see PLANNING.md Known
// Issues and Technical Debt), but the caller still needs to know
// something went wrong.
func (s *Service) ElevateTier(ctx context.Context, actor Actor, targetUserID string, toTier db.Tier) error {
	target, err := s.users.FindByID(ctx, targetUserID)
	if err != nil {
		return fmt.Errorf("find target user: %w", err)
	}

	if !CanElevate(actor, target.Tier, toTier) {
		return ErrNotPermitted
	}

	// Captured before UpdateTier runs, not read back off target
	// afterward - target may be the same backing value UpdateTier just
	// changed, depending on the userStore implementation.
	fromTier := target.Tier

	var elevatedBy *string
	if !actor.IsSuperAdmin {
		elevatedBy = &actor.UserID
	}

	if err := s.users.UpdateTier(ctx, targetUserID, toTier, elevatedBy, time.Now().UTC()); err != nil {
		return fmt.Errorf("update tier: %w", err)
	}

	detail := map[string]any{
		"from_tier": string(fromTier),
		"to_tier":   string(toTier),
	}
	if err := s.audit.Record(ctx, elevatedBy, actor.IsSuperAdmin, "elevated_user", "user", targetUserID, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// ListUsers returns every user, ordered by display name, if actor is
// permitted to view the Users & permissions roster - see
// rbac.CanViewUsers. The RBAC check lives here, not only at the HTTP
// layer, matching audit.Recorder.List's own reasoning: the guarantee
// travels with the method regardless of caller. Returns ErrNotPermitted
// if actor is not permitted - this is a read of the full roster (AD SID,
// tier, elevation history), not the narrower per-ID FindByID lookup every
// RBAC-gated handler already does to resolve its own actor, so it gets
// its own check rather than reusing CanManageNodes or CanViewAuditLog.
func (s *Service) ListUsers(ctx context.Context, actor Actor) ([]*db.User, error) {
	if !CanViewUsers(actor) {
		return nil, ErrNotPermitted
	}

	users, err := s.users.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}
