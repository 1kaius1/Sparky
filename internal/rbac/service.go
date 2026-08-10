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
}

// Service is the RBAC orchestration layer: check a rule, then persist the
// decision. See CLAUDE.md's Handler -> Service Layer -> Repository
// pattern - callers should never call UserRepository.UpdateTier directly;
// this is the only path a tier change should take.
type Service struct {
	users userStore
}

// NewService constructs a Service.
func NewService(users userStore) *Service {
	return &Service{users: users}
}

// ElevateTier changes targetUserID's tier to toTier, if actor is permitted
// to. The target's current tier is looked up fresh rather than trusted
// from the caller, so the rule check is always against real state.
func (s *Service) ElevateTier(ctx context.Context, actor Actor, targetUserID string, toTier db.Tier) error {
	target, err := s.users.FindByID(ctx, targetUserID)
	if err != nil {
		return fmt.Errorf("find target user: %w", err)
	}

	if !CanElevate(actor, target.Tier, toTier) {
		return ErrNotPermitted
	}

	var elevatedBy *string
	if !actor.IsSuperAdmin {
		elevatedBy = &actor.UserID
	}

	if err := s.users.UpdateTier(ctx, targetUserID, toTier, elevatedBy, time.Now().UTC()); err != nil {
		return fmt.Errorf("update tier: %w", err)
	}
	return nil
}
