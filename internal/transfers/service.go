// SPDX-License-Identifier: AGPL-3.0-or-later

package transfers

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// transferStore is the subset of *db.ModelTransferRepository this package
// needs, narrow enough to fake in tests - same pattern as
// internal/profiles' profileStore.
type transferStore interface {
	Create(ctx context.Context, destNodeID, modelRef string, sourceType db.TransferSourceType, sourceNodeID *string, requestedBy *string) (*db.ModelTransfer, error)
	FindByID(ctx context.Context, id string) (*db.ModelTransfer, error)
	UpdateProgress(ctx context.Context, id string, bytesTransferred, bytesTotal int64) error
	SetStatus(ctx context.Context, id string, status db.TransferStatus, errorMessage *string) error
	List(ctx context.Context) ([]*db.ModelTransfer, error)
}

// inventoryStore is the subset of *db.NodeModelInventoryRepository this
// package needs.
type inventoryStore interface {
	Upsert(ctx context.Context, nodeID, modelRef string, status db.InventoryStatus, sizeBytes int64, placedVia string) (*db.NodeModelInventory, error)
}

// overrideStore is the subset of *db.PermissionOverrideRepository this
// package needs, to resolve rbac.CanManageModelStore's hasOverride
// argument for a PowerDev actor.
type overrideStore interface {
	Get(ctx context.Context, userID string, capability db.Capability) (*db.PermissionOverride, error)
}

// dispatcher is the subset of *agentconn.Registry this package needs to
// reach a node without managing WebSocket state itself.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// auditRecorder is the subset of *audit.Recorder this package needs - same
// pattern as internal/nodes' auditRecorder.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is Model transfers' orchestration layer: check the rule,
// validate the input, confirm the destination is reachable, then persist,
// dispatch, and audit. See CLAUDE.md's Handler -> Service Layer ->
// Repository pattern - callers should never call ModelTransferRepository
// directly; this is the only path a transfer initiation should take. It
// also handles the reverse direction - progress reported back by the
// agent - via HandleTransferProgress, wired in as internal/agentconn's
// OnMessageFunc.
type Service struct {
	transfers transferStore
	inventory inventoryStore
	overrides overrideStore
	dispatch  dispatcher
	audit     auditRecorder
	logger    *log.Logger
}

// NewService constructs a Service. logger is used only by
// HandleTransferProgress, which - as an agentconn.OnMessageFunc - has no
// return value to propagate an error through, same reasoning as
// agentconn.Handler's own logger dependency.
func NewService(transfers transferStore, inventory inventoryStore, overrides overrideStore, dispatch dispatcher, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{transfers: transfers, inventory: inventory, overrides: overrides, dispatch: dispatch, audit: audit, logger: logger}
}

// canManageModelStore resolves rbac.CanManageModelStore's hasOverride
// argument, looking up actor's permission override only when it could
// possibly matter - Admin/SuperAdmin already have the capability
// implicitly and every tier other than PowerDev never has it, regardless
// of hasOverride, so there's no reason to query the overrides table for
// them.
func (s *Service) canManageModelStore(ctx context.Context, actor rbac.Actor) (bool, error) {
	hasOverride := false
	if !actor.IsSuperAdmin && actor.Tier == db.TierPowerDev {
		_, err := s.overrides.Get(ctx, actor.UserID, db.CapabilityManageModelStore)
		switch {
		case err == nil:
			hasOverride = true
		case errors.Is(err, db.ErrPermissionOverrideNotFound):
			// No grant - hasOverride stays false.
		default:
			return false, fmt.Errorf("check manage_model_store override: %w", err)
		}
	}
	return rbac.CanManageModelStore(actor, hasOverride), nil
}

// InitiateTransfer starts a new internet-sourced (Hugging Face) model
// download onto params.DestNodeID, if actor is permitted to - see
// rbac.CanManageModelStore. Confirms the destination node currently has a
// live agent connection (returns ErrDestNodeOffline if not) before
// creating the model_transfers row, so an unreachable node never leaves
// behind a queued transfer nothing will ever pick up. A permitted,
// dispatched initiation is always audited ("initiated_transfer" - see
// SCHEMA.md Audit log) after it persists, including when actor is the
// SuperAdmin.
func (s *Service) InitiateTransfer(ctx context.Context, actor rbac.Actor, params InitiateTransferParams) (*db.ModelTransfer, error) {
	permitted, err := s.canManageModelStore(ctx, actor)
	if err != nil {
		return nil, err
	}
	if !permitted {
		return nil, rbac.ErrNotPermitted
	}

	if err := params.validate(); err != nil {
		return nil, err
	}

	if !s.dispatch.Connected(params.DestNodeID) {
		return nil, ErrDestNodeOffline
	}

	var requestedBy *string
	if !actor.IsSuperAdmin {
		requestedBy = &actor.UserID
	}

	t, err := s.transfers.Create(ctx, params.DestNodeID, params.ModelRef, db.TransferSourceInternet, nil, requestedBy)
	if err != nil {
		return nil, fmt.Errorf("create model transfer: %w", err)
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "", agentproto.StartTransfer{
		TransferID: t.ID,
		ModelRef:   t.ModelRef,
	})
	if err != nil {
		return nil, fmt.Errorf("build start_transfer envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, params.DestNodeID, env); err != nil {
		return nil, fmt.Errorf("dispatch start_transfer to node %s: %w", params.DestNodeID, err)
	}

	detail := map[string]any{
		"dest_node_id": t.DestNodeID,
		"model_ref":    t.ModelRef,
	}
	if err := s.audit.Record(ctx, requestedBy, actor.IsSuperAdmin, "initiated_transfer", "model_transfer", t.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return t, nil
}

// ListTransfers returns every transfer across every node - unguarded by
// RBAC, since viewing is available at the lowest tier (CLAUDE.md Frontend
// Conventions, Transfers' sidebar tier "Read-only view"). Read/view
// actions are also never audited - see ARCHITECTURE.md Audit Log.
func (s *Service) ListTransfers(ctx context.Context) ([]*db.ModelTransfer, error) {
	transfers, err := s.transfers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}
	return transfers, nil
}

// HandleTransferProgress implements agentconn.OnMessageFunc for
// agentproto.TypeTransferProgress, the only message type Model transfers
// dispatches or expects back - wire it in as the onMessage callback passed
// to agentconn.NewHandler. Every other message type is ignored, matching
// OnMessageFunc's contract that a caller only ever sees types it doesn't
// already recognize.
//
// nodeID is the sending connection's authenticated identity (from
// agentconn's own handshake), not a value read out of the transfer row -
// it is trusted as the actual destination node reporting on its own
// download, matching internal/agentconn's framing as the only component
// that speaks the agent protocol.
func (s *Service) HandleTransferProgress(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeTransferProgress {
		return
	}

	var progress agentproto.TransferProgress
	if err := env.DecodePayload(&progress); err != nil {
		s.logger.Printf("transfers: node %s sent a malformed transfer_progress: %v", nodeID, err)
		return
	}

	// context.Background(), not a request context - this fires from
	// agentconn's readLoop, off the tail of the WebSocket read, not any
	// HTTP request.
	ctx := context.Background()
	status := db.TransferStatus(progress.Status)

	if err := s.transfers.UpdateProgress(ctx, progress.TransferID, progress.BytesTransferred, progress.BytesTotal); err != nil {
		s.logger.Printf("transfers: update progress for transfer %s: %v", progress.TransferID, err)
	}

	var errMsg *string
	if progress.ErrorMessage != "" {
		errMsg = &progress.ErrorMessage
	}
	if err := s.transfers.SetStatus(ctx, progress.TransferID, status, errMsg); err != nil {
		s.logger.Printf("transfers: set status for transfer %s: %v", progress.TransferID, err)
		return
	}

	if status != db.TransferStatusCompleted {
		return
	}

	t, err := s.transfers.FindByID(ctx, progress.TransferID)
	if err != nil {
		s.logger.Printf("transfers: look up completed transfer %s: %v", progress.TransferID, err)
		return
	}
	if _, err := s.inventory.Upsert(ctx, nodeID, t.ModelRef, db.InventoryStatusPresent, progress.BytesTotal, t.ID); err != nil {
		s.logger.Printf("transfers: upsert inventory for node %s model %s: %v", nodeID, t.ModelRef, err)
	}
}
