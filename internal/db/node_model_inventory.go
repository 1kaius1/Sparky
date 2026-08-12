// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InventoryStatus mirrors the inventory_status Postgres enum - see
// migrations/000009_create_node_model_inventory.up.sql and SCHEMA.md Node
// model inventory.
type InventoryStatus string

const (
	InventoryStatusPresent InventoryStatus = "present"
	InventoryStatusStale   InventoryStatus = "stale"
	InventoryStatusRemoved InventoryStatus = "removed"
)

// NodeModelInventory mirrors the node_model_inventory table - see
// SCHEMA.md Node model inventory. Current-state answer to "does this node
// have this model right now", distinct from ModelTransfer (history).
type NodeModelInventory struct {
	NodeID    string
	ModelRef  string
	Status    InventoryStatus
	SizeBytes int64
	PlacedAt  time.Time
	PlacedVia string
}

// ErrNodeModelInventoryNotFound is returned when a lookup finds no
// matching row.
var ErrNodeModelInventoryNotFound = errors.New("node model inventory entry not found")

// NodeModelInventoryRepository is the only component that queries the
// node_model_inventory table directly - see CLAUDE.md: the repository
// layer is the only place that accesses the database directly.
type NodeModelInventoryRepository struct {
	pool *pgxpool.Pool
}

// NewNodeModelInventoryRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewNodeModelInventoryRepository(pool *pgxpool.Pool) *NodeModelInventoryRepository {
	return &NodeModelInventoryRepository{pool: pool}
}

const nodeModelInventoryColumns = `node_id, model_ref, status, size_bytes, placed_at, placed_via`

func scanNodeModelInventory(row pgx.Row) (*NodeModelInventory, error) {
	var inv NodeModelInventory
	err := row.Scan(&inv.NodeID, &inv.ModelRef, &inv.Status, &inv.SizeBytes, &inv.PlacedAt, &inv.PlacedVia)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeModelInventoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan node model inventory: %w", err)
	}
	return &inv, nil
}

// Upsert records (or replaces) a node's inventory entry for a model,
// keyed on the (node_id, model_ref) composite primary key - completing a
// new transfer for a model the node already has replaces the existing row
// rather than erroring, the same ON CONFLICT pattern as
// PermissionOverrideRepository.Grant. placedVia must reference the
// ModelTransfer that produced this entry.
func (r *NodeModelInventoryRepository) Upsert(ctx context.Context, nodeID, modelRef string, status InventoryStatus, sizeBytes int64, placedVia string) (*NodeModelInventory, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO node_model_inventory (node_id, model_ref, status, size_bytes, placed_via)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (node_id, model_ref) DO UPDATE SET
		     status = EXCLUDED.status, size_bytes = EXCLUDED.size_bytes, placed_at = now(), placed_via = EXCLUDED.placed_via
		 RETURNING `+nodeModelInventoryColumns,
		nodeID, modelRef, status, sizeBytes, placedVia)

	inv, err := scanNodeModelInventory(row)
	if err != nil {
		return nil, fmt.Errorf("upsert node model inventory: %w", err)
	}
	return inv, nil
}

// Get looks up a node's inventory entry for a specific model. Returns
// ErrNodeModelInventoryNotFound if no row matches.
func (r *NodeModelInventoryRepository) Get(ctx context.Context, nodeID, modelRef string) (*NodeModelInventory, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+nodeModelInventoryColumns+` FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`,
		nodeID, modelRef)
	return scanNodeModelInventory(row)
}

// ListByNode returns every model inventory entry for a node - the raw
// input to the launch-eligibility (Green/Blue/Red) evaluation described in
// ARCHITECTURE.md.
func (r *NodeModelInventoryRepository) ListByNode(ctx context.Context, nodeID string) ([]*NodeModelInventory, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+nodeModelInventoryColumns+` FROM node_model_inventory WHERE node_id = $1 ORDER BY model_ref`,
		nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node model inventory for node %s: %w", nodeID, err)
	}
	defer rows.Close()

	var entries []*NodeModelInventory
	for rows.Next() {
		inv, err := scanNodeModelInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("list node model inventory for node %s: %w", nodeID, err)
		}
		entries = append(entries, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node model inventory for node %s: %w", nodeID, err)
	}
	return entries, nil
}
