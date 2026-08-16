// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// profileStore is the subset of *db.ProfileRepository this package
// needs, narrow enough to fake in tests - same pattern as
// internal/nodes' nodeStore.
type profileStore interface {
	Create(ctx context.Context, name, modelRef string, engineType db.ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion *string, targetNodeID string, port int, createdBy *string) (*db.Profile, error)
	Update(ctx context.Context, id, name, modelRef string, engineType db.ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion *string, targetNodeID string, port int, updatedBy *string) (*db.Profile, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*db.Profile, error)
	FindByID(ctx context.Context, id string) (*db.Profile, error)
}

// nodeLookup is the subset of *db.NodeRepository this package needs, to
// confirm a profile's TargetNodeID actually refers to a registered node.
type nodeLookup interface {
	FindByID(ctx context.Context, id string) (*db.Node, error)
}

// adapterRegistry is the subset of *engines.Registry this package needs.
type adapterRegistry interface {
	Adapter(engineType db.ProfileEngineType) (engines.Adapter, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs -
// same pattern as internal/nodes' auditRecorder.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is Model profile CRUD's orchestration layer: check the rule,
// validate the input, validate engine_params through the right adapter,
// confirm the target node exists, then persist and audit. See
// CLAUDE.md's Handler -> Service Layer -> Repository pattern - callers
// should never call ProfileRepository directly; this is the only path a
// profile create/update/delete should take.
type Service struct {
	profiles profileStore
	nodes    nodeLookup
	adapters adapterRegistry
	audit    auditRecorder
}

// NewService constructs a Service.
func NewService(profiles profileStore, nodes nodeLookup, adapters adapterRegistry, audit auditRecorder) *Service {
	return &Service{profiles: profiles, nodes: nodes, adapters: adapters, audit: audit}
}

// resolve validates fields, looks up the engine adapter for EngineType
// and uses it to validate EngineParams and report
// RequiresFullGPUResidency, and confirms TargetNodeID refers to a real
// node. Shared by CreateProfile and UpdateProfile - the same checks
// apply either way.
func (s *Service) resolve(ctx context.Context, f Fields) (requiresFullGPUResidency bool, err error) {
	if err := f.validate(); err != nil {
		return false, err
	}

	adapter, err := s.adapters.Adapter(f.EngineType)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := adapter.ValidateParams(f.EngineParams); err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}

	if _, err := s.nodes.FindByID(ctx, f.TargetNodeID); err != nil {
		if errors.Is(err, db.ErrNodeNotFound) {
			return false, fmt.Errorf("%w: target_node_id does not refer to a registered node", ErrInvalidProfile)
		}
		return false, fmt.Errorf("look up target node: %w", err)
	}

	return adapter.RequiresFullGPUResidency(), nil
}

// CreateProfile creates a new model profile, if actor is permitted to -
// see rbac.CanManageProfiles. A permitted creation is always audited
// ("created_profile" - see SCHEMA.md Audit log) after it persists,
// including when actor is the SuperAdmin.
func (s *Service) CreateProfile(ctx context.Context, actor rbac.Actor, params CreateParams) (*db.Profile, error) {
	if !rbac.CanManageProfiles(actor) {
		return nil, rbac.ErrNotPermitted
	}

	requiresFullGPUResidency, err := s.resolve(ctx, params.Fields)
	if err != nil {
		return nil, err
	}

	var createdBy *string
	if !actor.IsSuperAdmin {
		createdBy = &actor.UserID
	}

	p, err := s.profiles.Create(ctx, params.Name, params.ModelRef, params.EngineType, params.EngineParams,
		requiresFullGPUResidency, params.RequiredMemoryGB, params.EngineVersion, params.TargetNodeID, params.Port, createdBy)
	if err != nil {
		return nil, fmt.Errorf("create model profile: %w", err)
	}

	detail := map[string]any{
		"name":        p.Name,
		"engine_type": string(p.EngineType),
	}
	if err := s.audit.Record(ctx, createdBy, actor.IsSuperAdmin, "created_profile", "model_profile", p.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return p, nil
}

// UpdateProfile replaces an existing profile's mutable fields, if actor
// is permitted to. Returns db.ErrProfileNotFound (unwrapped, matching
// ProfileRepository.Update) if id doesn't exist. A permitted update is
// always audited ("updated_profile") after it persists.
func (s *Service) UpdateProfile(ctx context.Context, actor rbac.Actor, params UpdateParams) (*db.Profile, error) {
	if !rbac.CanManageProfiles(actor) {
		return nil, rbac.ErrNotPermitted
	}

	requiresFullGPUResidency, err := s.resolve(ctx, params.Fields)
	if err != nil {
		return nil, err
	}

	var updatedBy *string
	if !actor.IsSuperAdmin {
		updatedBy = &actor.UserID
	}

	p, err := s.profiles.Update(ctx, params.ID, params.Name, params.ModelRef, params.EngineType, params.EngineParams,
		requiresFullGPUResidency, params.RequiredMemoryGB, params.EngineVersion, params.TargetNodeID, params.Port, updatedBy)
	if err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("update model profile: %w", err)
	}

	detail := map[string]any{
		"name":        p.Name,
		"engine_type": string(p.EngineType),
	}
	if err := s.audit.Record(ctx, updatedBy, actor.IsSuperAdmin, "updated_profile", "model_profile", p.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return p, nil
}

// DeleteProfile deletes a profile by ID, if actor is permitted to.
// Returns db.ErrProfileNotFound if id doesn't exist. A permitted
// deletion is always audited ("deleted_profile") after it persists.
func (s *Service) DeleteProfile(ctx context.Context, actor rbac.Actor, id string) error {
	if !rbac.CanManageProfiles(actor) {
		return rbac.ErrNotPermitted
	}

	if err := s.profiles.Delete(ctx, id); err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return err
		}
		return fmt.Errorf("delete model profile: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "deleted_profile", "model_profile", id, nil); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// ListProfiles returns every model profile - unguarded by RBAC, since
// viewing profiles is available at the lowest tier (CLAUDE.md Frontend
// Conventions, Model profiles' sidebar tier "Read-only view"). Read/view
// actions are also never audited - see ARCHITECTURE.md Audit Log.
func (s *Service) ListProfiles(ctx context.Context) ([]*db.Profile, error) {
	profiles, err := s.profiles.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list model profiles: %w", err)
	}
	return profiles, nil
}

// GetProfile returns a single model profile by ID - unguarded by RBAC,
// same reasoning as ListProfiles (this is a read, not a mutation; the
// Dashboard UI's edit form uses it to prefill itself, and the actual
// authorization check happens on submission, inside UpdateProfile).
// Returns db.ErrProfileNotFound if id doesn't exist.
func (s *Service) GetProfile(ctx context.Context, id string) (*db.Profile, error) {
	p, err := s.profiles.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("get model profile: %w", err)
	}
	return p, nil
}
