// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// profileLookup is the subset of *db.ProfileRepository this package
// needs, narrow enough to fake in tests - same pattern as
// internal/profiles' nodeLookup.
type profileLookup interface {
	FindByID(ctx context.Context, id string) (*db.Profile, error)
}

// instanceStore is the subset of *db.RunningInstanceRepository this
// package needs.
type instanceStore interface {
	Create(ctx context.Context, profileID, primaryNodeID string, startedBy *string) (*db.RunningInstance, error)
	FindByID(ctx context.Context, id string) (*db.RunningInstance, error)
	FindActiveByProfileID(ctx context.Context, profileID string) (*db.RunningInstance, error)
	SetStatus(ctx context.Context, id string, status db.RunningInstanceStatus, actualPort *int, errorMessage *string) error
	List(ctx context.Context) ([]*db.RunningInstance, error)
	ListRunningByNode(ctx context.Context, nodeID string) ([]*db.RunningInstance, error)
}

// adapterRegistry is the subset of *engines.Registry this package needs -
// same pattern as internal/profiles' adapterRegistry.
type adapterRegistry interface {
	Adapter(engineType db.ProfileEngineType) (engines.Adapter, error)
}

// dispatcher is the subset of *agentconn.Registry this package needs to
// reach a node without managing WebSocket state itself - same pattern as
// internal/transfers' dispatcher.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is Running instances' orchestration layer: check the rule,
// validate the input, resolve the profile's engine adapter into a launch
// spec, confirm the target node is reachable, then persist, dispatch, and
// audit. See CLAUDE.md's Handler -> Service Layer -> Repository pattern -
// callers should never call RunningInstanceRepository directly; this is
// the only path a load/unload should take. It also handles the reverse
// direction - the result an agent reports back - via
// HandleInstanceResult, wired in as internal/agentconn's OnMessageFunc.
type Service struct {
	profiles  profileLookup
	instances instanceStore
	adapters  adapterRegistry
	dispatch  dispatcher
	audit     auditRecorder
	logger    *log.Logger
}

// NewService constructs a Service. logger is used only by
// HandleInstanceResult, which - as an agentconn.OnMessageFunc - has no
// return value to propagate an error through, same reasoning as
// internal/transfers.Service's logger dependency.
func NewService(profiles profileLookup, instances instanceStore, adapters adapterRegistry, dispatch dispatcher, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{profiles: profiles, instances: instances, adapters: adapters, dispatch: dispatch, audit: audit, logger: logger}
}

// LoadInstance starts a new running instance of params.ProfileID's model,
// if actor is permitted to - see rbac.CanLaunchInstances. Refuses if the
// profile already has a non-terminal running instance (ErrAlreadyRunning)
// or its target node has no live agent connection (ErrTargetNodeOffline) -
// both checked before a running_instances row is even created, so a
// refused load never leaves one behind. A permitted, dispatched load is
// always audited ("loaded_model" - see SCHEMA.md Audit log, whose own
// example this action is) after it persists, including when actor is the
// SuperAdmin.
func (s *Service) LoadInstance(ctx context.Context, actor rbac.Actor, params LoadParams) (*db.RunningInstance, error) {
	if !rbac.CanLaunchInstances(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if err := params.validate(); err != nil {
		return nil, err
	}

	profile, err := s.profiles.FindByID(ctx, params.ProfileID)
	if err != nil {
		if errors.Is(err, db.ErrProfileNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("look up profile: %w", err)
	}
	if profile.TargetNodeID == nil {
		// Guaranteed not to happen by model_profiles_single_node_only
		// (migrations/000007) - v0.1.0 cannot create a profile without
		// one - but checked explicitly rather than dereferencing blindly,
		// since a nil pointer panic here would take down the whole
		// service over one bad row.
		return nil, fmt.Errorf("%w: profile %s has no target_node_id (clustered profiles are not supported yet)", ErrInvalidLoad, profile.ID)
	}
	targetNodeID := *profile.TargetNodeID

	if _, err := s.instances.FindActiveByProfileID(ctx, profile.ID); err == nil {
		return nil, ErrAlreadyRunning
	} else if !errors.Is(err, db.ErrRunningInstanceNotFound) {
		return nil, fmt.Errorf("check for an active running instance: %w", err)
	}

	if !s.dispatch.Connected(targetNodeID) {
		return nil, ErrTargetNodeOffline
	}

	adapter, err := s.adapters.Adapter(profile.EngineType)
	if err != nil {
		return nil, fmt.Errorf("look up engine adapter: %w", err)
	}
	spec, err := adapter.BuildLaunchSpec(profile.EngineParams)
	if err != nil {
		return nil, fmt.Errorf("build launch spec: %w", err)
	}

	var startedBy *string
	if !actor.IsSuperAdmin {
		startedBy = &actor.UserID
	}

	inst, err := s.instances.Create(ctx, profile.ID, targetNodeID, startedBy)
	if err != nil {
		return nil, fmt.Errorf("create running instance: %w", err)
	}

	// EngineVersion is "" (unpinned - resolve to whatever the node's
	// latest symlink currently points to) unless the profile pins one -
	// agentproto.LoadInstance carries plain values, not pointers, same
	// reasoning as EngineType/ModelRef.
	var engineVersion string
	if profile.EngineVersion != nil {
		engineVersion = *profile.EngineVersion
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               inst.ID,
		ModelRef:                 profile.ModelRef,
		EngineType:               string(profile.EngineType),
		EngineVersion:            engineVersion,
		Image:                    spec.Image,
		Args:                     spec.Args,
		Port:                     profile.Port,
		RequiresFullGPUResidency: profile.RequiresFullGPUResidency,
	})
	if err != nil {
		return nil, fmt.Errorf("build load_instance envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, targetNodeID, env); err != nil {
		dispatchErr := fmt.Errorf("dispatch load_instance to node %s: %w", targetNodeID, err)
		// The agent was never actually told to start anything, so the row
		// this call just created should not be left stuck at starting -
		// move it to the same terminal state a real launch failure
		// reported by the agent would produce (mapInstanceStatus /
		// HandleInstanceResult), so it stops looking like a live instance
		// and the operator sees why. Best-effort: log and still return
		// dispatchErr if this second write also fails, same precedent as
		// ReconcileNode's per-instance dispatch failures.
		errMsg := dispatchErr.Error()
		if setErr := s.instances.SetStatus(ctx, inst.ID, db.RunningInstanceStatusFailed, nil, &errMsg); setErr != nil {
			s.logger.Printf("lifecycle: mark running instance %s failed after dispatch error: %v", inst.ID, setErr)
		}
		return nil, dispatchErr
	}

	detail := map[string]any{
		"profile_id": profile.ID,
		"model_ref":  profile.ModelRef,
	}
	if err := s.audit.Record(ctx, startedBy, actor.IsSuperAdmin, "loaded_model", "running_instance", inst.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return inst, nil
}

// UnloadInstance stops a running instance, if actor is permitted to - see
// rbac.CanLaunchInstances. Only an instance currently
// RunningInstanceStatusRunning can be unloaded (ErrInstanceNotRunning
// otherwise); refuses if the instance's node has no live agent connection
// (ErrTargetNodeOffline), checked before the instance is transitioned to
// RunningInstanceStatusStopping. A permitted, dispatched unload is always
// audited ("unloaded_model") after it persists.
func (s *Service) UnloadInstance(ctx context.Context, actor rbac.Actor, instanceID string) (*db.RunningInstance, error) {
	if !rbac.CanLaunchInstances(actor) {
		return nil, rbac.ErrNotPermitted
	}

	inst, err := s.instances.FindByID(ctx, instanceID)
	if err != nil {
		if errors.Is(err, db.ErrRunningInstanceNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("look up running instance: %w", err)
	}
	if inst.Status != db.RunningInstanceStatusRunning {
		return nil, ErrInstanceNotRunning
	}
	if !s.dispatch.Connected(inst.PrimaryNodeID) {
		return nil, ErrTargetNodeOffline
	}

	if err := s.instances.SetStatus(ctx, inst.ID, db.RunningInstanceStatusStopping, nil, nil); err != nil {
		return nil, fmt.Errorf("set status for running instance %s: %w", inst.ID, err)
	}
	inst.Status = db.RunningInstanceStatusStopping

	env, err := agentproto.NewEnvelope(agentproto.TypeUnloadInstance, "", agentproto.UnloadInstance{InstanceID: inst.ID})
	if err != nil {
		return nil, fmt.Errorf("build unload_instance envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, inst.PrimaryNodeID, env); err != nil {
		dispatchErr := fmt.Errorf("dispatch unload_instance to node %s: %w", inst.PrimaryNodeID, err)
		// The stop request never reached the agent, so the instance is
		// presumably still actually running fine - revert the stopping
		// transition above rather than leaving the row stuck, which also
		// unblocks a retry (UnloadInstance requires status == running).
		// Best-effort: log and still return dispatchErr if this second
		// write also fails.
		errMsg := dispatchErr.Error()
		if setErr := s.instances.SetStatus(ctx, inst.ID, db.RunningInstanceStatusRunning, nil, &errMsg); setErr != nil {
			s.logger.Printf("lifecycle: revert running instance %s to running after dispatch error: %v", inst.ID, setErr)
		}
		return nil, dispatchErr
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "unloaded_model", "running_instance", inst.ID, nil); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return inst, nil
}

// mapInstanceStatus translates an agentproto.InstanceStatus* wire value
// into its internal/db.RunningInstanceStatus equivalent.
func mapInstanceStatus(status string) (db.RunningInstanceStatus, error) {
	switch status {
	case agentproto.InstanceStatusRunning:
		return db.RunningInstanceStatusRunning, nil
	case agentproto.InstanceStatusFailed:
		return db.RunningInstanceStatusFailed, nil
	case agentproto.InstanceStatusStopped:
		return db.RunningInstanceStatusStopped, nil
	default:
		return "", fmt.Errorf("unknown instance status %q", status)
	}
}

// HandleInstanceResult implements agentconn.OnMessageFunc for
// agentproto.TypeInstanceResult, the only message type Running instances
// expects back from an agent - wire it in as the onMessage callback
// passed to agentconn.NewHandler. Every other message type is ignored,
// matching OnMessageFunc's contract.
func (s *Service) HandleInstanceResult(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeInstanceResult {
		return
	}

	var result agentproto.InstanceResult
	if err := env.DecodePayload(&result); err != nil {
		s.logger.Printf("lifecycle: node %s sent a malformed instance_result: %v", nodeID, err)
		return
	}

	status, err := mapInstanceStatus(result.Status)
	if err != nil {
		s.logger.Printf("lifecycle: node %s reported instance %s: %v", nodeID, result.InstanceID, err)
		return
	}

	var actualPort *int
	if status == db.RunningInstanceStatusRunning && result.ActualPort != 0 {
		actualPort = &result.ActualPort
	}
	var errMsg *string
	if result.ErrorMessage != "" {
		errMsg = &result.ErrorMessage
	}

	// context.Background(), not a request context - this fires from
	// agentconn's readLoop, off the tail of the WebSocket read, not any
	// HTTP request - same reasoning as internal/transfers.Service's
	// HandleTransferProgress.
	ctx := context.Background()
	if err := s.instances.SetStatus(ctx, result.InstanceID, status, actualPort, errMsg); err != nil {
		s.logger.Printf("lifecycle: set status for running instance %s: %v", result.InstanceID, err)
	}
}

// ReconcileNode implements agentconn.OnConnectFunc - the running_instances
// staleness fix (PLANNING.md's Decisions Log): fires once nodeID's agent
// connection is freshly established (including the very first connection
// ever, where ListRunningByNode simply returns nothing to check), asking
// the agent to confirm, one at a time, whether each row this table still
// believes is running on that node actually still is. The agent's answer
// arrives back through the ordinary TypeInstanceResult path
// (HandleInstanceResult above) - this method only needs to ask the
// question, not handle the answer itself.
//
// Not audited - system-internal, not actor-driven, same precedent as
// HandleInstanceResult/internal/transfers.HandleTransferProgress, neither
// of which is audited either. A dispatch failure for one instance is
// logged and does not stop the sweep from asking about the rest.
func (s *Service) ReconcileNode(ctx context.Context, nodeID string) {
	instances, err := s.instances.ListRunningByNode(ctx, nodeID)
	if err != nil {
		s.logger.Printf("lifecycle: list running instances for node %s: %v", nodeID, err)
		return
	}

	for _, inst := range instances {
		env, err := agentproto.NewEnvelope(agentproto.TypeCheckInstance, "", agentproto.CheckInstance{InstanceID: inst.ID})
		if err != nil {
			s.logger.Printf("lifecycle: build check_instance envelope for instance %s: %v", inst.ID, err)
			continue
		}
		if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
			s.logger.Printf("lifecycle: dispatch check_instance for instance %s to node %s: %v", inst.ID, nodeID, err)
		}
	}
}

// ListInstances returns every running instance - unguarded by RBAC, since
// viewing them is available at the lowest tier (the Dashboard overview
// page's fleet summary, CLAUDE.md Frontend Conventions' Dashboard sidebar
// tier "Read-only"). Read/view actions are also never audited - see
// ARCHITECTURE.md Audit Log.
func (s *Service) ListInstances(ctx context.Context) ([]*db.RunningInstance, error) {
	instances, err := s.instances.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}
	return instances, nil
}
