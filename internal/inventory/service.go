// SPDX-License-Identifier: AGPL-3.0-or-later

// Package inventory is the read side of node_model_inventory - see
// SCHEMA.md Node model inventory. Before this package existed, the table
// was write-only: internal/transfers upserts into it, but nothing ever
// read it back (PLANNING.md's Decisions Log, the Models redesign). This
// is that missing read side, grouped for the Inventory page's Simple/
// Advanced views rather than exposing raw per-node rows.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// ErrModelInUse is returned by Delete when a Model profile still targets
// the entry - deleting it would leave that profile pointing at nothing.
// The planned inventory foreign key on model_profiles cannot enforce this
// (a deleted entry is only marked removed, never hard-deleted, so no FK
// ever fires), so it is checked here in Go.
var ErrModelInUse = errors.New("model is still referenced by a profile")

// ErrDeleteInProgress is returned by Delete when a delete of the same entry
// has been sent and the node has not answered yet.
var ErrDeleteInProgress = errors.New("a delete of this model is already in progress")

// Delete states reported by DeleteState.
const (
	DeleteNone     = ""
	DeleteRemoving = "removing"
	DeleteFailed   = "failed"
)

const (
	// deleteStateTTL bounds how long a pending or failed delete is shown.
	// A node that never answers must not leave a row stuck on "removing"
	// forever, and a failure notice should not outlive its usefulness.
	deleteStateTTL = 15 * time.Minute
	maxFailureLen  = 200
)

type deleteState struct {
	at     time.Time
	failed bool
	reason string
}

// ErrNodeOffline is returned by Delete when the node holding the model has
// no live agent connection - deletion needs the node online to actually
// free the disk space, so there is nothing sensible to queue.
var ErrNodeOffline = errors.New("node is not connected")

// inventoryStore is the subset of *db.NodeModelInventoryRepository this
// package needs, narrow enough to fake in tests - same pattern as
// internal/transfers' inventoryStore.
type inventoryStore interface {
	List(ctx context.Context) ([]*db.NodeModelInventory, error)
	ListByNode(ctx context.Context, nodeID string) ([]*db.NodeModelInventory, error)
	Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error)
	SetStatus(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat, status db.InventoryStatus) error
}

// profileStore is the subset of *db.ProfileRepository Delete needs to
// refuse removing a model a profile still uses.
type profileStore interface {
	List(ctx context.Context) ([]*db.Profile, error)
}

// overrideStore is the subset of *db.PermissionOverrideRepository needed
// to resolve rbac.CanManageModelStore's hasOverride argument.
type overrideStore interface {
	Get(ctx context.Context, userID string, capability db.Capability) (*db.PermissionOverride, error)
}

// dispatcher is the subset of *agentconn.Registry this package needs.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is Inventory's read layer. Every method here is unguarded by
// RBAC - viewing is available at the lowest tier (CLAUDE.md Frontend
// Conventions' Models group sidebar tier), the same "Read-only view"
// floor internal/transfers.ListTransfers and internal/nodes.ListNodes
// already sit at. Read/view actions are also never audited - see
// ARCHITECTURE.md Audit Log.
type Service struct {
	inventory inventoryStore
	profiles  profileStore
	overrides overrideStore
	dispatch  dispatcher
	audit     auditRecorder
	logger    *log.Logger

	// deletes tracks deletes that have been dispatched but not yet
	// confirmed (and recent failures), so the Inventory page can show
	// "removing..." the moment the operator confirms rather than a row
	// that looks untouched until the node answers. In-memory only: a
	// server restart forgets it, which is harmless - the entry simply
	// looks present until the agent's answer (which still updates the
	// database) arrives.
	deletesMu sync.Mutex
	deletes   map[string]deleteState
}

func deleteKey(nodeID, modelRef, quantization string, format db.ModelFormat) string {
	return nodeID + "\x00" + modelRef + "\x00" + quantization + "\x00" + string(format)
}

// DeleteState reports whether a delete of this entry is in flight
// (DeleteRemoving) or recently failed (DeleteFailed, with the node's
// reason), for the Inventory page.
func (s *Service) DeleteState(nodeID, modelRef, quantization string, format db.ModelFormat) (state, reason string) {
	s.deletesMu.Lock()
	defer s.deletesMu.Unlock()
	key := deleteKey(nodeID, modelRef, quantization, format)
	d, ok := s.deletes[key]
	if !ok {
		return DeleteNone, ""
	}
	if time.Since(d.at) > deleteStateTTL {
		delete(s.deletes, key)
		return DeleteNone, ""
	}
	if d.failed {
		return DeleteFailed, d.reason
	}
	return DeleteRemoving, ""
}

func (s *Service) setDeleteState(key string, d deleteState) {
	s.deletesMu.Lock()
	defer s.deletesMu.Unlock()
	if s.deletes == nil {
		s.deletes = make(map[string]deleteState)
	}
	s.deletes[key] = d
}

func (s *Service) clearDeleteState(key string) {
	s.deletesMu.Lock()
	defer s.deletesMu.Unlock()
	delete(s.deletes, key)
}

// NewService constructs a Service. logger is used only by
// HandleDeleteModelResult, which - as an agentconn.OnMessageFunc - has no
// return value to propagate an error through.
func NewService(inventory inventoryStore, profiles profileStore, overrides overrideStore, dispatch dispatcher, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{inventory: inventory, profiles: profiles, overrides: overrides, dispatch: dispatch, audit: audit, logger: logger}
}

// Group is every node's entry for one (model_ref, quantization, format)
// combination - the Advanced Inventory view's own unit, grouped here in
// Go rather than in SQL, the same split httpapi already uses elsewhere
// for node-name-map building (e.g. internal/httpapi's handleTransfers).
type Group struct {
	ModelRef     string
	Quantization string
	Format       db.ModelFormat
	Entries      []*db.NodeModelInventory
}

// ListGrouped returns every distinct (model_ref, quantization, format)
// combination across every node, each carrying its own per-node entries -
// the Advanced Inventory view's raw input (PLANNING.md's Models redesign
// decision 14). Entries marked removed (see Delete) are excluded - the row
// is kept for history, but the model is no longer on the node. Group order matches
// *db.NodeModelInventoryRepository.List's own ORDER BY (model_ref,
// quantization, format, node_id) - a group is emitted the first time its
// key is seen, so no separate sort is needed here.
func (s *Service) ListGrouped(ctx context.Context) ([]Group, error) {
	entries, err := s.inventory.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list inventory: %w", err)
	}

	type key struct {
		modelRef     string
		quantization string
		format       db.ModelFormat
	}
	index := make(map[key]*Group)
	var order []key
	for _, e := range entries {
		if e.Status == db.InventoryStatusRemoved {
			continue
		}
		k := key{modelRef: e.ModelRef, quantization: e.Quantization, format: e.Format}
		g, ok := index[k]
		if !ok {
			g = &Group{ModelRef: e.ModelRef, Quantization: e.Quantization, Format: e.Format}
			index[k] = g
			order = append(order, k)
		}
		g.Entries = append(g.Entries, e)
	}

	groups := make([]Group, 0, len(order))
	for _, k := range order {
		groups = append(groups, *index[k])
	}
	return groups, nil
}

// SimpleRow is one model_ref's totals across every quantization, format,
// and node it exists in - the Simple Inventory view's own unit
// (PLANNING.md's Models redesign decision 14). TotalSizeBytes sums every
// placement - SCHEMA.md's Node model inventory records size_bytes per
// (node, quantization, format) placement, not per model_ref, so Simple
// mode's whole-model total has to be computed, not read directly.
type SimpleRow struct {
	ModelRef       string
	Quantizations  []string
	TotalSizeBytes int64
	// IncompleteCount is how many partial copies (cancelled/failed
	// transfers' leftovers) exist across nodes. They are counted apart, and
	// excluded from Quantizations and TotalSizeBytes: partial data is not
	// an available quantization and would misstate what is usable.
	IncompleteCount int
}

// ListGroupedSimple collapses ListGrouped's own output further, by
// model_ref alone - a pure presentation reshape of the same underlying
// rows, not a second repository query. Quantizations preserves the order
// ListGrouped already produces (which itself matches the repository's own
// ORDER BY), so it comes out already sorted without a separate sort step
// here.
func (s *Service) ListGroupedSimple(ctx context.Context) ([]SimpleRow, error) {
	groups, err := s.ListGrouped(ctx)
	if err != nil {
		return nil, err
	}

	index := make(map[string]*SimpleRow)
	seenQuant := make(map[string]map[string]bool)
	var order []string
	for _, g := range groups {
		row, ok := index[g.ModelRef]
		if !ok {
			row = &SimpleRow{ModelRef: g.ModelRef}
			index[g.ModelRef] = row
			seenQuant[g.ModelRef] = make(map[string]bool)
			order = append(order, g.ModelRef)
		}
		for _, e := range g.Entries {
			if e.Status == db.InventoryStatusIncomplete {
				row.IncompleteCount++
				continue
			}
			if !seenQuant[g.ModelRef][g.Quantization] {
				seenQuant[g.ModelRef][g.Quantization] = true
				row.Quantizations = append(row.Quantizations, g.Quantization)
			}
			row.TotalSizeBytes += e.SizeBytes
		}
	}

	rows := make([]SimpleRow, 0, len(order))
	for _, ref := range order {
		rows = append(rows, *index[ref])
	}
	return rows, nil
}

// ListByNode returns a single node's usable inventory entries, excluding
// entries marked removed and incomplete (partial data is not something a
// profile can run or another node can copy) - backs the
// Profile-creation cascading picker (PLANNING.md's Models redesign PR 9),
// not yet wired to anything.
func (s *Service) ListByNode(ctx context.Context, nodeID string) ([]*db.NodeModelInventory, error) {
	entries, err := s.inventory.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list inventory for node %s: %w", nodeID, err)
	}
	present := make([]*db.NodeModelInventory, 0, len(entries))
	for _, e := range entries {
		if e.Status != db.InventoryStatusRemoved && e.Status != db.InventoryStatusIncomplete {
			present = append(present, e)
		}
	}
	return present, nil
}

// Get looks up a single node's inventory entry for one model
// quantization+format - backs the Profile-save presence check and the
// peer-transfer source-presence check (PLANNING.md's Models redesign PRs
// 9 and 6), not yet wired to anything. Returns
// db.ErrNodeModelInventoryNotFound if no row matches, same as the
// repository itself - callers are expected to check for that sentinel.
func (s *Service) Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error) {
	entry, err := s.inventory.Get(ctx, nodeID, modelRef, quantization, format)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// canManageModelStore resolves rbac.CanManageModelStore's hasOverride
// argument, querying the overrides table only for a PowerDev actor - the
// only tier where it can matter. Same logic as
// internal/transfers.Service's own copy, kept local rather than shared to
// avoid a dependency between the two packages.
func (s *Service) canManageModelStore(ctx context.Context, actor rbac.Actor) (bool, error) {
	hasOverride := false
	if !actor.IsSuperAdmin && actor.Tier == db.TierPowerDev {
		_, err := s.overrides.Get(ctx, actor.UserID, db.CapabilityManageModelStore)
		switch {
		case err == nil:
			hasOverride = true
		case errors.Is(err, db.ErrPermissionOverrideNotFound):
		default:
			return false, fmt.Errorf("check manage_model_store override: %w", err)
		}
	}
	return rbac.CanManageModelStore(actor, hasOverride), nil
}

// CanDelete reports whether actor may call Delete - exported so
// internal/httpapi can decide whether to show the Delete action at all,
// not a security boundary (Delete re-checks).
func (s *Service) CanDelete(ctx context.Context, actor rbac.Actor) (bool, error) {
	return s.canManageModelStore(ctx, actor)
}

// Delete asks the node holding a model copy to remove it from disk.
// Gated by rbac.CanManageModelStore (already documented as covering
// "download and delete"). Refuses if a profile still targets the entry
// (ErrModelInUse) or the node has no live connection (ErrNodeOffline).
// The request is asynchronous, same as a transfer: this returns once the
// delete_model command is dispatched, and HandleDeleteModelResult marks
// the entry removed when the agent confirms. A permitted, dispatched
// request is audited as "deleted_model_copy" against the node (audit
// object ids are strict uuids and an inventory entry has none of its own,
// so the model identity goes in the detail).
func (s *Service) Delete(ctx context.Context, actor rbac.Actor, nodeID, modelRef, quantization string, format db.ModelFormat) error {
	permitted, err := s.canManageModelStore(ctx, actor)
	if err != nil {
		return err
	}
	if !permitted {
		return rbac.ErrNotPermitted
	}

	entry, err := s.inventory.Get(ctx, nodeID, modelRef, quantization, format)
	if err != nil {
		return err
	}
	if entry.Status == db.InventoryStatusRemoved {
		return db.ErrNodeModelInventoryNotFound
	}
	key := deleteKey(nodeID, modelRef, quantization, format)
	if state, _ := s.DeleteState(nodeID, modelRef, quantization, format); state == DeleteRemoving {
		return ErrDeleteInProgress
	}

	profiles, err := s.profiles.List(ctx)
	if err != nil {
		return fmt.Errorf("list profiles for delete check: %w", err)
	}
	for _, p := range profiles {
		if p.TargetNodeID == nil || *p.TargetNodeID != nodeID || p.ModelRef != modelRef || p.Format != format {
			continue
		}
		profileQuant := ""
		if p.Quantization != nil {
			profileQuant = *p.Quantization
		}
		if profileQuant == quantization {
			return fmt.Errorf("%w: profile %q", ErrModelInUse, p.Name)
		}
	}

	if !s.dispatch.Connected(nodeID) {
		return ErrNodeOffline
	}
	env, err := agentproto.NewEnvelope(agentproto.TypeDeleteModel, "", agentproto.DeleteModel{
		ModelRef: modelRef, Quantization: quantization, Format: string(format),
	})
	if err != nil {
		return fmt.Errorf("build delete_model envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		return fmt.Errorf("dispatch delete_model to node %s: %w", nodeID, err)
	}
	s.setDeleteState(key, deleteState{at: time.Now()})

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	detail := map[string]any{"model_ref": modelRef, "quantization": quantization, "format": string(format)}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "deleted_model_copy", "node", nodeID, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// HandleDeleteModelResult implements agentconn.OnMessageFunc for
// agentproto.TypeDeleteModelResult. nodeID is the sending connection's
// authenticated identity, not a wire value. A confirmed removal marks the
// entry removed; a failure is logged and leaves the entry present, since
// the files may still be on disk.
func (s *Service) HandleDeleteModelResult(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeDeleteModelResult {
		return
	}
	var result agentproto.DeleteModelResult
	if err := env.DecodePayload(&result); err != nil {
		s.logger.Printf("inventory: node %s sent a malformed delete_model_result: %v", nodeID, err)
		return
	}
	key := deleteKey(nodeID, result.ModelRef, result.Quantization, db.ModelFormat(result.Format))
	if !result.Success {
		s.logger.Printf("inventory: node %s failed to delete %s (%s, %s): %s", nodeID, result.ModelRef, result.Quantization, result.Format, result.Reason)
		// Remember why, so the page can tell the operator instead of the
		// row silently going back to looking untouched.
		reason := result.Reason
		if len(reason) > maxFailureLen {
			reason = reason[:maxFailureLen]
		}
		if reason == "" {
			reason = "the node reported a failure"
		}
		s.setDeleteState(key, deleteState{at: time.Now(), failed: true, reason: reason})
		return
	}
	s.clearDeleteState(key)
	err := s.inventory.SetStatus(context.Background(), nodeID, result.ModelRef, result.Quantization, db.ModelFormat(result.Format), db.InventoryStatusRemoved)
	if err != nil {
		s.logger.Printf("inventory: mark %s removed on node %s: %v", result.ModelRef, nodeID, err)
	}
}
