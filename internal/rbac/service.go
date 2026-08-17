// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	UpdateTier(ctx context.Context, id string, tier db.Tier, elevatedBy *string, elevatedAt *time.Time) error
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
	users  userStore
	audit  auditRecorder
	logger *log.Logger
}

// NewService constructs a Service. logger is used only by ElevateTier's
// best-effort revert-on-audit-failure path, which - like
// lifecycle.NewService's own logger dependency - has no return value to
// propagate a secondary failure through.
func NewService(users userStore, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{users: users, audit: audit, logger: logger}
}

// ElevateTier changes targetUserID's tier to toTier, if actor is permitted
// to. The target's current tier is looked up fresh rather than trusted
// from the caller, so the rule check is always against real state.
//
// A permitted elevation is always audited ("elevated_user" - see
// SCHEMA.md Audit log) after it persists, including when actor is the
// SuperAdmin - see ARCHITECTURE.md's "no exceptions" audit guarantee. This
// package does not use a database transaction spanning both writes - no
// such cross-repository pattern exists anywhere in this codebase, and the
// failure mode (the audit Postgres write itself failing immediately after
// a successful update) is rare enough not to warrant building one. Instead,
// an audit write failure reverts the tier change rather than leaving an
// unaudited elevation in place - see the revert block below.
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
	// changed, depending on the userStore implementation. fromElevatedBy/
	// fromElevatedAt let a revert below restore the exact prior state,
	// including nil/NULL for a user who was never previously elevated.
	fromTier := target.Tier
	fromElevatedBy := target.ElevatedBy
	fromElevatedAt := target.ElevatedAt

	var elevatedBy *string
	if !actor.IsSuperAdmin {
		elevatedBy = &actor.UserID
	}
	now := time.Now().UTC()

	if err := s.users.UpdateTier(ctx, targetUserID, toTier, elevatedBy, &now); err != nil {
		return fmt.Errorf("update tier: %w", err)
	}

	detail := map[string]any{
		"from_tier": string(fromTier),
		"to_tier":   string(toTier),
	}
	if err := s.audit.Record(ctx, elevatedBy, actor.IsSuperAdmin, "elevated_user", "user", targetUserID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		// The tier change already persisted but was never audited - revert
		// it rather than leaving an unaudited elevation in place (CLAUDE.md
		// Audit Logging: every state-changing action must be audited, no
		// exceptions). Best-effort: log and still return auditErr if the
		// revert itself also fails, same precedent as the running_instances
		// dispatch-recovery fix (PLANNING.md Decisions Log, 2026-08-16).
		if revertErr := s.users.UpdateTier(ctx, targetUserID, fromTier, fromElevatedBy, fromElevatedAt); revertErr != nil {
			s.logger.Printf("rbac: revert tier for user %s after audit failure: %v", targetUserID, revertErr)
		}
		return auditErr
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
