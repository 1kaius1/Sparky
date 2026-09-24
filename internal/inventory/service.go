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
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
)

// inventoryStore is the subset of *db.NodeModelInventoryRepository this
// package needs, narrow enough to fake in tests - same pattern as
// internal/transfers' inventoryStore.
type inventoryStore interface {
	List(ctx context.Context) ([]*db.NodeModelInventory, error)
	ListByNode(ctx context.Context, nodeID string) ([]*db.NodeModelInventory, error)
	Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error)
}

// Service is Inventory's read layer. Every method here is unguarded by
// RBAC - viewing is available at the lowest tier (CLAUDE.md Frontend
// Conventions' Models group sidebar tier), the same "Read-only view"
// floor internal/transfers.ListTransfers and internal/nodes.ListNodes
// already sit at. Read/view actions are also never audited - see
// ARCHITECTURE.md Audit Log.
type Service struct {
	inventory inventoryStore
}

// NewService constructs a Service.
func NewService(inventory inventoryStore) *Service {
	return &Service{inventory: inventory}
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
// decision 14). Group order matches
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
		if !seenQuant[g.ModelRef][g.Quantization] {
			seenQuant[g.ModelRef][g.Quantization] = true
			row.Quantizations = append(row.Quantizations, g.Quantization)
		}
		for _, e := range g.Entries {
			row.TotalSizeBytes += e.SizeBytes
		}
	}

	rows := make([]SimpleRow, 0, len(order))
	for _, ref := range order {
		rows = append(rows, *index[ref])
	}
	return rows, nil
}

// ListByNode returns a single node's inventory entries - backs the
// Profile-creation cascading picker (PLANNING.md's Models redesign PR 9),
// not yet wired to anything.
func (s *Service) ListByNode(ctx context.Context, nodeID string) ([]*db.NodeModelInventory, error) {
	entries, err := s.inventory.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list inventory for node %s: %w", nodeID, err)
	}
	return entries, nil
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
