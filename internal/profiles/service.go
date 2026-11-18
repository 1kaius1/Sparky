// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/launchopts"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// profileStore is the subset of *db.ProfileRepository this package
// needs, narrow enough to fake in tests - same pattern as
// internal/nodes' nodeStore.
type profileStore interface {
	Create(ctx context.Context, name, modelRef string, engineType db.ProfileEngineType, engineParams, env, containerOptions json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion *string, quantization string, format db.ModelFormat, image *string, targetNodeID string, port int, createdBy *string) (*db.Profile, error)
	Update(ctx context.Context, id, name, modelRef string, engineType db.ProfileEngineType, engineParams, env, containerOptions json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion *string, quantization string, format db.ModelFormat, image *string, targetNodeID string, port int, updatedBy *string) (*db.Profile, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*db.Profile, error)
	FindByID(ctx context.Context, id string) (*db.Profile, error)
}

// nodeLookup is the subset of *db.NodeRepository this package needs, to
// confirm a profile's TargetNodeID actually refers to a registered node.
type nodeLookup interface {
	FindByID(ctx context.Context, id string) (*db.Node, error)
}

// inventoryLookup is the subset of *inventory.Service this package needs -
// confirming a profile's (TargetNodeID, ModelRef, Quantization, Format)
// actually names a present Node model inventory entry before persisting,
// ahead of the raw FK violation the database's own
// model_profiles_target_inventory_fkey (migration 000035) would otherwise
// produce. A narrow, primitives-only interface (same pattern as
// nodeLookup) rather than depending on internal/inventory directly, to
// keep this package decoupled from internal/inventory's own API surface.
type inventoryLookup interface {
	Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error)
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
	profiles  profileStore
	nodes     nodeLookup
	inventory inventoryLookup
	adapters  adapterRegistry
	audit     auditRecorder
}

// NewService constructs a Service.
func NewService(profiles profileStore, nodes nodeLookup, inventory inventoryLookup, adapters adapterRegistry, audit auditRecorder) *Service {
	return &Service{profiles: profiles, nodes: nodes, inventory: inventory, adapters: adapters, audit: audit}
}

// resolve validates fields, looks up the engine adapter for EngineType
// and uses it to validate EngineParams and report
// RequiresFullGPUResidency, confirms TargetNodeID refers to a real node,
// and confirms (TargetNodeID, ModelRef, Quantization, Format) names a
// present Node model inventory entry on that node - a friendly error
// ahead of the raw FK violation model_profiles_target_inventory_fkey
// (migration 000035) would otherwise produce, and the one check that FK
// itself can't make (it enforces existence only, not status = 'present').
// Shared by CreateProfile and UpdateProfile - the same checks apply
// either way.
//
// It also validates Env and ContainerOptions (internal/launchopts) and
// returns them in the normalized form to store. existing is the profile being
// updated, nil on create: an actor who may not set the host-level container
// options (rbac.CanSetHostContainerOptions) cannot change them, and because
// their form never carries those fields, an edit keeps what existing already
// has instead of wiping it. Container options are refused for a node on the
// bare-metal backend, which has no container for them to apply to - accepting
// them there would be exactly the silent no-op engine_params rejects.
func (s *Service) resolve(ctx context.Context, actor rbac.Actor, f Fields, existing *db.Profile) (resolved, error) {
	if err := f.validate(); err != nil {
		return resolved{}, err
	}

	adapter, err := s.adapters.Adapter(f.EngineType)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := adapter.ValidateParams(f.EngineParams); err != nil {
		return resolved{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}

	node, err := s.nodes.FindByID(ctx, f.TargetNodeID)
	if err != nil {
		if errors.Is(err, db.ErrNodeNotFound) {
			return resolved{}, fmt.Errorf("%w: target_node_id does not refer to a registered node", ErrInvalidProfile)
		}
		return resolved{}, fmt.Errorf("look up target node: %w", err)
	}

	env, err := launchopts.ParseEnv(f.Env)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	opts, err := launchopts.ParseContainerOptions(f.ContainerOptions)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if !rbac.CanSetHostContainerOptions(actor) {
		var kept agentproto.ContainerOptions
		if existing != nil {
			// A stored value that no longer parses is a data problem, not the
			// caller's mistake; fall back to keeping nothing rather than
			// blocking every edit of the profile.
			if parsed, perr := launchopts.ParseContainerOptions(existing.ContainerOptions); perr == nil {
				kept = parsed
			}
		}
		if !launchopts.IsZero(launchopts.HostLevel(opts)) && !launchopts.HostLevelEqual(opts, kept) {
			return resolved{}, fmt.Errorf("%w: only an Admin can set extra mounts, devices, capabilities, privileged mode, ulimits, security options or the host PID namespace", ErrInvalidProfile)
		}
		opts = launchopts.WithHostLevel(opts, kept)
	}
	if node.RuntimeBackend == db.RuntimeBackendBareMetal && !launchopts.IsZero(opts) {
		return resolved{}, fmt.Errorf("%w: container options do not apply to a bare-metal node, which runs the engine as a plain process; clear them (environment variables still apply)", ErrInvalidProfile)
	}

	entry, err := s.inventory.Get(ctx, f.TargetNodeID, f.ModelRef, f.Quantization, f.Format)
	if err != nil {
		if errors.Is(err, db.ErrNodeModelInventoryNotFound) {
			return resolved{}, fmt.Errorf("%w: that model is not present in the target node's inventory", ErrInvalidProfile)
		}
		return resolved{}, fmt.Errorf("look up target inventory entry: %w", err)
	}
	if entry.Status != db.InventoryStatusPresent {
		return resolved{}, fmt.Errorf("%w: that model is not present in the target node's inventory", ErrInvalidProfile)
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return resolved{}, fmt.Errorf("encode env: %w", err)
	}
	optsJSON, err := launchopts.Marshal(opts)
	if err != nil {
		return resolved{}, err
	}
	return resolved{
		requiresFullGPUResidency: adapter.RequiresFullGPUResidency(),
		env:                      envJSON,
		containerOptions:         optsJSON,
		envNames:                 envNames(env),
		options:                  opts,
	}, nil
}

// resolved is what resolve hands back for the caller to persist and audit.
type resolved struct {
	requiresFullGPUResidency bool
	// env and containerOptions are the normalized JSON to store.
	env, containerOptions json.RawMessage
	// envNames and options are for the audit record: variable names only,
	// never their values, since a value may be a credential and audit entries
	// are forwarded to outside systems.
	envNames []string
	options  agentproto.ContainerOptions
}

// auditDetail is the part of the audit record that describes what the profile
// runs with, added to the name and engine type.
func (r resolved) auditDetail(detail map[string]any) map[string]any {
	if len(r.envNames) > 0 {
		detail["env_vars"] = r.envNames
	}
	if !launchopts.IsZero(r.options) {
		detail["container_options"] = r.options
	}
	return detail
}

func envNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// nameTakenError is the operator-facing form of db.ErrProfileNameTaken. The
// name is the model id clients use, so the message says to tell variants of
// the same model apart rather than just reporting the collision.
func nameTakenError(name string) error {
	return fmt.Errorf("%w: another profile is already named %q; each profile's name is the model id clients use, so give this one a name that tells it apart (for example by adding its engine or the parameter that differs)", ErrInvalidProfile, name)
}

// CreateProfile creates a new model profile, if actor is permitted to -
// see rbac.CanManageProfiles. A permitted creation is always audited
// ("created_profile" - see SCHEMA.md Audit log) after it persists,
// including when actor is the SuperAdmin.
func (s *Service) CreateProfile(ctx context.Context, actor rbac.Actor, params CreateParams) (*db.Profile, error) {
	if !rbac.CanManageProfiles(actor) {
		return nil, rbac.ErrNotPermitted
	}

	// Surrounding whitespace is a paste artifact, not part of the name; the
	// rest of the name is checked as a model id by Fields.validate.
	params.Name = strings.TrimSpace(params.Name)
	res, err := s.resolve(ctx, actor, params.Fields, nil)
	if err != nil {
		return nil, err
	}

	var createdBy *string
	if !actor.IsSuperAdmin {
		createdBy = &actor.UserID
	}

	p, err := s.profiles.Create(ctx, params.Name, params.ModelRef, params.EngineType, params.EngineParams, res.env, res.containerOptions,
		res.requiresFullGPUResidency, params.RequiredMemoryGB, params.EngineVersion, params.Quantization, params.Format, params.Image, params.TargetNodeID, params.Port, createdBy)
	if err != nil {
		if errors.Is(err, db.ErrProfileNameTaken) {
			return nil, nameTakenError(params.Name)
		}
		return nil, fmt.Errorf("create model profile: %w", err)
	}

	detail := res.auditDetail(map[string]any{
		"name":        p.Name,
		"engine_type": string(p.EngineType),
	})
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

	params.Name = strings.TrimSpace(params.Name)

	existing, err := s.profiles.FindByID(ctx, params.ID)
	if err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("look up model profile: %w", err)
	}
	res, err := s.resolve(ctx, actor, params.Fields, existing)
	if err != nil {
		return nil, err
	}

	var updatedBy *string
	if !actor.IsSuperAdmin {
		updatedBy = &actor.UserID
	}

	p, err := s.profiles.Update(ctx, params.ID, params.Name, params.ModelRef, params.EngineType, params.EngineParams, res.env, res.containerOptions,
		res.requiresFullGPUResidency, params.RequiredMemoryGB, params.EngineVersion, params.Quantization, params.Format, params.Image, params.TargetNodeID, params.Port, updatedBy)
	if err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return nil, err
		}
		if errors.Is(err, db.ErrProfileNameTaken) {
			return nil, nameTakenError(params.Name)
		}
		return nil, fmt.Errorf("update model profile: %w", err)
	}

	detail := res.auditDetail(map[string]any{
		"name":        p.Name,
		"engine_type": string(p.EngineType),
	})
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
