// SPDX-License-Identifier: AGPL-3.0-or-later

package engineprovision

import (
	"context"
	"fmt"
	"log"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// engineTransferStore is the subset of *db.EngineTransferRepository this
// package needs, narrow enough to fake in tests - same pattern as
// internal/transfers' transferStore.
type engineTransferStore interface {
	Create(ctx context.Context, destNodeID string, engineType db.ProfileEngineType, version string, requestedBy *string) (*db.EngineTransfer, error)
	FindByID(ctx context.Context, id string) (*db.EngineTransfer, error)
	UpdateProgress(ctx context.Context, id string, bytesTransferred, bytesTotal int64) error
	SetStatus(ctx context.Context, id string, status db.EngineTransferStatus, errorMessage *string) error
	List(ctx context.Context) ([]*db.EngineTransfer, error)
}

// engineInventoryStore is the subset of *db.NodeEngineInventoryRepository
// this package needs.
type engineInventoryStore interface {
	Upsert(ctx context.Context, nodeID string, engineType db.ProfileEngineType, version string, status db.InventoryStatus, installPath string, sizeBytes int64, placedVia string) (*db.NodeEngineInventory, error)
	List(ctx context.Context) ([]*db.NodeEngineInventory, error)
}

// dispatcher is the subset of *agentconn.Registry this package needs to
// reach a node without managing WebSocket state itself - same shape as
// internal/transfers' dispatcher.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// auditRecorder is the subset of *audit.Recorder this package needs - same
// pattern as internal/transfers' auditRecorder.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is Engine transfers' orchestration layer: check the rule,
// validate the input, confirm the destination is reachable, then persist,
// dispatch, and audit. See CLAUDE.md's Handler -> Service Layer ->
// Repository pattern - callers should never call EngineTransferRepository
// directly; this is the only path a provisioning run should take. It also
// handles the reverse direction - progress reported back by the agent - via
// HandleEngineTransferProgress, wired in as internal/agentconn's
// OnMessageFunc.
type Service struct {
	transfers engineTransferStore
	inventory engineInventoryStore
	dispatch  dispatcher
	audit     auditRecorder
	logger    *log.Logger
}

// NewService constructs a Service. logger is used only by
// HandleEngineTransferProgress, which - as an agentconn.OnMessageFunc - has
// no return value to propagate an error through, same reasoning as
// internal/transfers.Service's own logger dependency.
func NewService(transfers engineTransferStore, inventory engineInventoryStore, dispatch dispatcher, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{transfers: transfers, inventory: inventory, dispatch: dispatch, audit: audit, logger: logger}
}

// ProvisionEngine starts a new compiled-engine binary provisioning run onto
// params.DestNodeID, if actor is permitted to - see rbac.CanManageNodes.
// Unlike Model transfers' manage_model_store (a PowerDev-grantable
// permission override), this is node-level infrastructure provisioning:
// Admin/SuperAdmin only, no override path - see PLANNING.md's 2026-08-15
// Decisions Log entry. Confirms the destination node currently has a live
// agent connection (returns ErrDestNodeOffline if not) before creating the
// engine_transfers row, so an unreachable node never leaves behind a queued
// transfer nothing will ever pick up. A permitted, dispatched request is
// always audited ("initiated_engine_transfer" - see SCHEMA.md Audit log)
// after it persists, including when actor is the SuperAdmin.
func (s *Service) ProvisionEngine(ctx context.Context, actor rbac.Actor, params ProvisionEngineParams) (*db.EngineTransfer, error) {
	if !rbac.CanManageNodes(actor) {
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

	t, err := s.transfers.Create(ctx, params.DestNodeID, params.EngineType, params.Version, requestedBy)
	if err != nil {
		return nil, fmt.Errorf("create engine transfer: %w", err)
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeStartEngineTransfer, "", agentproto.StartEngineTransfer{
		TransferID: t.ID,
		EngineType: string(t.EngineType),
		Version:    t.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("build start_engine_transfer envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, params.DestNodeID, env); err != nil {
		return nil, fmt.Errorf("dispatch start_engine_transfer to node %s: %w", params.DestNodeID, err)
	}

	detail := map[string]any{
		"dest_node_id": t.DestNodeID,
		"engine_type":  string(t.EngineType),
		"version":      t.Version,
	}
	if err := s.audit.Record(ctx, requestedBy, actor.IsSuperAdmin, "initiated_engine_transfer", "engine_transfer", t.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return t, nil
}

// ListEngineTransfers returns every engine transfer across every node -
// unguarded by RBAC, since viewing is available at the lowest tier, same
// precedent as internal/transfers.Service.ListTransfers. Read/view actions
// are also never audited - see ARCHITECTURE.md Audit Log.
func (s *Service) ListEngineTransfers(ctx context.Context) ([]*db.EngineTransfer, error) {
	transfers, err := s.transfers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list engine transfers: %w", err)
	}
	return transfers, nil
}

// ListNodeEngineInventory returns every node's installed engine inventory
// across every node - unguarded by RBAC, same reasoning as
// ListEngineTransfers. Distinct from that method: this answers "what's
// actually installed right now" (SCHEMA.md Node engine inventory), not "what
// provisioning runs have happened" (SCHEMA.md Engine transfers).
func (s *Service) ListNodeEngineInventory(ctx context.Context) ([]*db.NodeEngineInventory, error) {
	entries, err := s.inventory.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list node engine inventory: %w", err)
	}
	return entries, nil
}

// HandleEngineTransferProgress implements agentconn.OnMessageFunc for
// agentproto.TypeEngineTransferProgress, the only message type Engine
// transfers dispatches or expects back - wire it in as the onMessage
// callback passed to agentconn.NewHandler. Every other message type is
// ignored, matching OnMessageFunc's contract.
//
// nodeID is the sending connection's authenticated identity (from
// agentconn's own handshake), not a value read out of the transfer row -
// trusted as the actual destination node reporting on its own
// provisioning run, same reasoning as internal/transfers'
// HandleTransferProgress.
func (s *Service) HandleEngineTransferProgress(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeEngineTransferProgress {
		return
	}

	var progress agentproto.EngineTransferProgress
	if err := env.DecodePayload(&progress); err != nil {
		s.logger.Printf("engineprovision: node %s sent a malformed engine_transfer_progress: %v", nodeID, err)
		return
	}

	// context.Background(), not a request context - this fires from
	// agentconn's readLoop, off the tail of the WebSocket read, not any
	// HTTP request.
	ctx := context.Background()
	status := db.EngineTransferStatus(progress.Status)

	if err := s.transfers.UpdateProgress(ctx, progress.TransferID, progress.BytesTransferred, progress.BytesTotal); err != nil {
		s.logger.Printf("engineprovision: update progress for transfer %s: %v", progress.TransferID, err)
	}

	var errMsg *string
	if progress.ErrorMessage != "" {
		errMsg = &progress.ErrorMessage
	}
	if err := s.transfers.SetStatus(ctx, progress.TransferID, status, errMsg); err != nil {
		s.logger.Printf("engineprovision: set status for transfer %s: %v", progress.TransferID, err)
		return
	}

	if status != db.EngineTransferStatusCompleted {
		return
	}

	t, err := s.transfers.FindByID(ctx, progress.TransferID)
	if err != nil {
		s.logger.Printf("engineprovision: look up completed transfer %s: %v", progress.TransferID, err)
		return
	}
	if _, err := s.inventory.Upsert(ctx, nodeID, t.EngineType, t.Version, db.InventoryStatusPresent, progress.InstallPath, progress.InstalledSizeBytes, t.ID); err != nil {
		s.logger.Printf("engineprovision: upsert inventory for node %s engine %s@%s: %v", nodeID, t.EngineType, t.Version, err)
	}
}
