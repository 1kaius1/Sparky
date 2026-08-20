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

// NodeEngineInventory mirrors the node_engine_inventory table - see
// SCHEMA.md Node engine inventory. Current-state answer to "which engine
// binary versions are installed on this node right now", distinct from
// EngineTransfer (history) the same way NodeModelInventory is distinct from
// ModelTransfer. Unlike NodeModelInventory, multiple versions of the same
// (NodeID, EngineType) coexist by design - see the composite primary key on
// (node_id, engine_type, version) in the migration.
type NodeEngineInventory struct {
	NodeID      string
	EngineType  ProfileEngineType
	Version     string
	Status      InventoryStatus
	InstallPath string
	SizeBytes   int64
	PlacedAt    time.Time
	PlacedVia   string
}

// ErrNodeEngineInventoryNotFound is returned when a lookup finds no
// matching row.
var ErrNodeEngineInventoryNotFound = errors.New("node engine inventory entry not found")

// NodeEngineInventoryRepository is the only component that queries the
// node_engine_inventory table directly - see CLAUDE.md: the repository
// layer is the only place that accesses the database directly.
type NodeEngineInventoryRepository struct {
	pool *pgxpool.Pool
}

// NewNodeEngineInventoryRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewNodeEngineInventoryRepository(pool *pgxpool.Pool) *NodeEngineInventoryRepository {
	return &NodeEngineInventoryRepository{pool: pool}
}

const nodeEngineInventoryColumns = `node_id, engine_type, version, status, install_path, size_bytes, placed_at, placed_via`

func scanNodeEngineInventory(row pgx.Row) (*NodeEngineInventory, error) {
	var inv NodeEngineInventory
	err := row.Scan(&inv.NodeID, &inv.EngineType, &inv.Version, &inv.Status, &inv.InstallPath, &inv.SizeBytes, &inv.PlacedAt, &inv.PlacedVia)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeEngineInventoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan node engine inventory: %w", err)
	}
	return &inv, nil
}

// Upsert records (or replaces) a node's inventory entry for a specific
// engine version, keyed on the (node_id, engine_type, version) composite
// primary key - re-provisioning the same version replaces the existing row
// rather than erroring, the same ON CONFLICT pattern as
// NodeModelInventoryRepository.Upsert. Provisioning a *different* version
// inserts a new, separate row instead - see the type doc comment. placedVia
// must reference the EngineTransfer that produced this entry.
func (r *NodeEngineInventoryRepository) Upsert(ctx context.Context, nodeID string, engineType ProfileEngineType, version string, status InventoryStatus, installPath string, sizeBytes int64, placedVia string) (*NodeEngineInventory, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO node_engine_inventory (node_id, engine_type, version, status, install_path, size_bytes, placed_via)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (node_id, engine_type, version) DO UPDATE SET
		     status = EXCLUDED.status, install_path = EXCLUDED.install_path, size_bytes = EXCLUDED.size_bytes,
		     placed_at = now(), placed_via = EXCLUDED.placed_via
		 RETURNING `+nodeEngineInventoryColumns,
		nodeID, engineType, version, status, installPath, sizeBytes, placedVia)

	inv, err := scanNodeEngineInventory(row)
	if err != nil {
		return nil, fmt.Errorf("upsert node engine inventory: %w", err)
	}
	return inv, nil
}

// Get looks up a node's inventory entry for a specific engine version.
// Returns ErrNodeEngineInventoryNotFound if no row matches.
func (r *NodeEngineInventoryRepository) Get(ctx context.Context, nodeID string, engineType ProfileEngineType, version string) (*NodeEngineInventory, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+nodeEngineInventoryColumns+` FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`,
		nodeID, engineType, version)
	return scanNodeEngineInventory(row)
}

// List returns every engine inventory entry across every node - most
// recently placed first. Backs the Engine inventory page
// (internal/httpapi), which needs the full cross-node picture rather than
// one node at a time.
func (r *NodeEngineInventoryRepository) List(ctx context.Context) ([]*NodeEngineInventory, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+nodeEngineInventoryColumns+` FROM node_engine_inventory ORDER BY placed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list node engine inventory: %w", err)
	}
	defer rows.Close()

	var entries []*NodeEngineInventory
	for rows.Next() {
		inv, err := scanNodeEngineInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("list node engine inventory: %w", err)
		}
		entries = append(entries, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node engine inventory: %w", err)
	}
	return entries, nil
}

// ListByNode returns every engine inventory entry for a node - every
// installed version of every engine, most recently placed first.
func (r *NodeEngineInventoryRepository) ListByNode(ctx context.Context, nodeID string) ([]*NodeEngineInventory, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+nodeEngineInventoryColumns+` FROM node_engine_inventory WHERE node_id = $1 ORDER BY placed_at DESC`,
		nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node engine inventory for node %s: %w", nodeID, err)
	}
	defer rows.Close()

	var entries []*NodeEngineInventory
	for rows.Next() {
		inv, err := scanNodeEngineInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("list node engine inventory for node %s: %w", nodeID, err)
		}
		entries = append(entries, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node engine inventory for node %s: %w", nodeID, err)
	}
	return entries, nil
}
