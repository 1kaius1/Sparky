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
	Create(ctx context.Context, destNodeID, modelRef string, sourceType db.TransferSourceType, sourceNodeID *string, quantization, requestedBy *string) (*db.ModelTransfer, error)
	FindByID(ctx context.Context, id string) (*db.ModelTransfer, error)
	UpdateProgress(ctx context.Context, id string, bytesTransferred, bytesTotal int64) error
	SetStatus(ctx context.Context, id string, status db.TransferStatus, errorMessage *string) error
	SetSourceInterface(ctx context.Context, id string, interfaceName *string) error
	SetFormat(ctx context.Context, id string, format *db.ModelFormat) error
	List(ctx context.Context) ([]*db.ModelTransfer, error)
}

// inventoryStore is the subset of *db.NodeModelInventoryRepository this
// package needs.
type inventoryStore interface {
	Upsert(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat, status db.InventoryStatus, sizeBytes int64, placedVia string) (*db.NodeModelInventory, error)
	Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error)
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

	// peers/sourceInv back peer_node transfers and the connectivity check;
	// nil disables them (InitiateTransfer then refuses a peer_node request).
	peers     nodeDirectory
	sourceInv sourceInventory
	checks    checkRegistry
}

// NewService constructs a Service. logger is used only by
// HandleTransferProgress, which - as an agentconn.OnMessageFunc - has no
// return value to propagate an error through, same reasoning as
// agentconn.Handler's own logger dependency.
func NewService(transfers transferStore, inventory inventoryStore, overrides overrideStore, dispatch dispatcher, audit auditRecorder, peers nodeDirectory, sourceInv sourceInventory, logger *log.Logger) *Service {
	return &Service{transfers: transfers, inventory: inventory, overrides: overrides, dispatch: dispatch, audit: audit, peers: peers, sourceInv: sourceInv, logger: logger}
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

// CanInitiateTransfer reports whether actor is currently permitted to call
// InitiateTransfer - exported so internal/httpapi can decide whether to
// show the "New transfer" link/form at all, same non-security-boundary
// reasoning as engineTransfersPageData.CanProvision. The real enforcement
// is still InitiateTransfer's own canManageModelStore check; this exists
// only because that check needs an overrides-table lookup for a PowerDev
// actor, which httpapi has no direct access to (CLAUDE.md: never bypass
// the repository layer).
func (s *Service) CanInitiateTransfer(ctx context.Context, actor rbac.Actor) (bool, error) {
	return s.canManageModelStore(ctx, actor)
}

// InitiateTransfer starts a new model transfer onto params.DestNodeID - an
// internet-sourced (Hugging Face) download by default, or a peer_node
// pull (see initiatePeer) - if actor is permitted to - see
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

	if params.SourceType == db.TransferSourcePeerNode {
		return s.initiatePeer(ctx, actor, params)
	}

	if !s.dispatch.Connected(params.DestNodeID) {
		return nil, ErrDestNodeOffline
	}

	var requestedBy *string
	if !actor.IsSuperAdmin {
		requestedBy = &actor.UserID
	}

	var quantization *string
	if params.Quantization != "" {
		quantization = &params.Quantization
	}

	t, err := s.transfers.Create(ctx, params.DestNodeID, params.ModelRef, db.TransferSourceInternet, nil, quantization, requestedBy)
	if err != nil {
		return nil, fmt.Errorf("create model transfer: %w", err)
	}

	var startQuantization string
	if t.Quantization != nil {
		startQuantization = *t.Quantization
	}
	env, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "", agentproto.StartTransfer{
		TransferID:   t.ID,
		ModelRef:     t.ModelRef,
		Quantization: startQuantization,
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
	if params.RetryOf != "" {
		detail["retry_of"] = params.RetryOf
	}
	if err := s.audit.Record(ctx, requestedBy, actor.IsSuperAdmin, "initiated_transfer", "model_transfer", t.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return t, nil
}

// isTerminal reports whether status ends a transfer's lifecycle.
func isTerminal(status db.TransferStatus) bool {
	return status == db.TransferStatusCompleted || status == db.TransferStatusFailed || status == db.TransferStatusCancelled
}

// CancelTransfer stops a queued or running transfer - internet download or
// peer pull alike - if actor is permitted to (rbac.CanManageModelStore).
// The row is marked cancelled first, so any progress report still in flight
// from the node cannot resurrect it (HandleTransferProgress ignores updates
// to a finished transfer); then the destination - the node actually doing
// the work for either kind - is told to stop, and for a peer transfer the
// source is told to drop its authorization (also automatic, since cancelled
// is a terminal status). Stopping the node is best-effort: if it is offline
// the transfer is still cancelled here, and HandleTransferProgress re-sends
// the cancel if that node later reports activity for it. Partial data is
// kept, as for a failure, so a later retry can resume it. Audited as
// "cancelled_transfer".
func (s *Service) CancelTransfer(ctx context.Context, actor rbac.Actor, transferID string) error {
	permitted, err := s.canManageModelStore(ctx, actor)
	if err != nil {
		return err
	}
	if !permitted {
		return rbac.ErrNotPermitted
	}

	t, err := s.transfers.FindByID(ctx, transferID)
	if err != nil {
		return err
	}
	if isTerminal(t.Status) {
		return ErrNotCancelable
	}
	previous := t.Status

	if err := s.transfers.SetStatus(ctx, t.ID, db.TransferStatusCancelled, nil); err != nil {
		return fmt.Errorf("mark transfer cancelled: %w", err)
	}
	s.sendCancel(ctx, t.DestNodeID, t.ID)
	s.revokePeer(ctx, t)
	// A transfer that had started leaves partial data on the node. The
	// agent's own final report is ignored (the row is already cancelled), so
	// the last progress the server saw is the best size available.
	if previous == db.TransferStatusTransferring {
		s.recordIncomplete(ctx, t, t.BytesTransferred)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	detail := map[string]any{
		"dest_node_id":    t.DestNodeID,
		"model_ref":       t.ModelRef,
		"previous_status": string(previous),
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "cancelled_transfer", "model_transfer", t.ID, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// sendCancel tells a node to stop running a transfer. Best-effort: an
// offline node just logs.
func (s *Service) sendCancel(ctx context.Context, nodeID, transferID string) {
	if !s.dispatch.Connected(nodeID) {
		s.logger.Printf("transfers: node %s is offline - cancelled transfer %s will be stopped if it reports activity", nodeID, transferID)
		return
	}
	env, err := agentproto.NewEnvelope(agentproto.TypeCancelTransfer, "", agentproto.CancelTransfer{TransferID: transferID})
	if err != nil {
		s.logger.Printf("transfers: build cancel_transfer for %s: %v", transferID, err)
		return
	}
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		s.logger.Printf("transfers: dispatch cancel_transfer for %s to node %s: %v", transferID, nodeID, err)
	}
}

// RetryTransfer re-runs a failed transfer as a new transfer with the same
// parameters, if actor is permitted to (rbac.CanManageModelStore). It goes
// through InitiateTransfer, so everything is re-validated against the
// current state - a retry of a peer transfer whose source has since gone
// offline, or lost the model, is refused like any new request would be.
// The failed row is left untouched as history, and the new transfer's audit
// record carries retry_of. Only a failed transfer is retryable: a running
// one is still in flight, and a cancelled one was stopped on purpose.
func (s *Service) RetryTransfer(ctx context.Context, actor rbac.Actor, transferID string) (*db.ModelTransfer, error) {
	permitted, err := s.canManageModelStore(ctx, actor)
	if err != nil {
		return nil, err
	}
	if !permitted {
		return nil, rbac.ErrNotPermitted
	}

	t, err := s.transfers.FindByID(ctx, transferID)
	if err != nil {
		return nil, err
	}
	if t.Status != db.TransferStatusFailed {
		return nil, ErrNotRetryable
	}

	params := InitiateTransferParams{DestNodeID: t.DestNodeID, ModelRef: t.ModelRef, SourceType: t.SourceType, RetryOf: t.ID}
	if t.Quantization != nil {
		params.Quantization = *t.Quantization
	}
	if t.SourceType == db.TransferSourcePeerNode {
		if t.SourceNodeID == nil || t.Format == nil {
			return nil, fmt.Errorf("%w: the failed transfer has no recorded source or format", ErrInvalidTransfer)
		}
		params.SourceNodeID = *t.SourceNodeID
		params.Format = *t.Format
		if t.SourceInterface != nil {
			params.SourceInterface = *t.SourceInterface
		}
	}
	return s.InitiateTransfer(ctx, actor, params)
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

// inventoryKey is the (quantization, format) an inventory entry for this
// transfer is filed under - the same for the finished model and for the
// partial data a cancelled or failed attempt leaves, so a later successful
// transfer replaces the incomplete entry rather than sitting beside it.
func inventoryKey(t *db.ModelTransfer) (quantization string, format db.ModelFormat) {
	if t.Quantization != nil {
		quantization = *t.Quantization
	}
	// Same convention-based inference migrations/000030_add_model_format.up.sql
	// uses to backfill historical rows: a non-empty quantization is only
	// ever meaningful for a GGUF file today. This is a stopgap, not the
	// real answer - a later PR (agent/modelinspect) determines format by
	// inspecting the downloaded file itself instead of guessing from
	// whether a quantization string happens to be present.
	format = db.ModelFormatSafetensors
	if quantization != "" {
		format = db.ModelFormatGGUF
	}
	// A peer transfer copies a known inventory entry, so its real format
	// (recorded at initiation) wins over the guess.
	if t.Format != nil {
		format = *t.Format
	}
	return quantization, format
}

// recordIncomplete files partial data a cancelled or failed transfer left on
// its destination as an "incomplete" inventory entry, so it shows up on the
// Inventory page and can be deleted - otherwise it would sit on the node's
// disk (kept on purpose, so a new transfer can resume it) with nothing in
// the UI to free it. An existing usable entry for the same model is never
// downgraded: a failed re-download must not make a working model look
// broken. Best-effort - a failure here is logged, never surfaced, since the
// transfer's own status is already recorded.
func (s *Service) recordIncomplete(ctx context.Context, t *db.ModelTransfer, bytes int64) {
	quantization, format := inventoryKey(t)
	existing, err := s.inventory.Get(ctx, t.DestNodeID, t.ModelRef, quantization, format)
	switch {
	case err == nil && (existing.Status == db.InventoryStatusPresent || existing.Status == db.InventoryStatusStale):
		return
	case err != nil && !errors.Is(err, db.ErrNodeModelInventoryNotFound):
		s.logger.Printf("transfers: look up inventory for incomplete %s on node %s: %v", t.ModelRef, t.DestNodeID, err)
		return
	}
	if _, err := s.inventory.Upsert(ctx, t.DestNodeID, t.ModelRef, quantization, format, db.InventoryStatusIncomplete, bytes, t.ID); err != nil {
		s.logger.Printf("transfers: record incomplete %s on node %s: %v", t.ModelRef, t.DestNodeID, err)
	}
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

	// nodeID is the sending connection's authenticated identity: only the
	// transfer's own destination may report on it. Without this, any
	// connected node could mark another node's transfer completed or
	// failed, or plant an inventory row for it.
	t, err := s.transfers.FindByID(ctx, progress.TransferID)
	if err != nil {
		s.logger.Printf("transfers: progress for unknown transfer %s from node %s: %v", progress.TransferID, nodeID, err)
		return
	}
	if t.DestNodeID != nodeID {
		s.logger.Printf("transfers: ignoring progress for transfer %s from node %s - not its destination", t.ID, nodeID)
		return
	}

	// A finished transfer's row is never touched again. Most importantly a
	// cancelled one: the node's last reports race the cancel, and a late
	// "failed" (its own context-cancelled error) or even "completed" must
	// not overwrite what the operator decided. If the node is still
	// reporting the transfer as running - it was offline when the cancel was
	// sent - tell it again.
	if isTerminal(t.Status) {
		if t.Status == db.TransferStatusCancelled && status == db.TransferStatusTransferring {
			s.sendCancel(ctx, nodeID, t.ID)
		}
		return
	}

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

	if status == db.TransferStatusCompleted || status == db.TransferStatusFailed || status == db.TransferStatusCancelled {
		s.revokePeer(ctx, t)
	}
	if status == db.TransferStatusFailed && progress.BytesTransferred > 0 {
		s.recordIncomplete(ctx, t, progress.BytesTransferred)
	}

	if status != db.TransferStatusCompleted {
		return
	}

	quantization, format := inventoryKey(t)
	if _, err := s.inventory.Upsert(ctx, nodeID, t.ModelRef, quantization, format, db.InventoryStatusPresent, progress.BytesTotal, t.ID); err != nil {
		s.logger.Printf("transfers: upsert inventory for node %s model %s: %v", nodeID, t.ModelRef, err)
	}
}
