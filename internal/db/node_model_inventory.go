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
	// InventoryStatusIncomplete means the node holds partial data for the
	// model - left by a cancelled or failed transfer, kept so a new one can
	// resume it. Not usable (never offered as a copy source or a profile's
	// model) but visible, so the operator can free the disk space.
	InventoryStatusIncomplete InventoryStatus = "incomplete"
)

// ModelFormat mirrors the model_format Postgres enum - see
// migrations/000030_add_model_format.up.sql and SCHEMA.md. An explicit,
// independent field from quantization and from engine_type - Aphrodite,
// for instance, can load either format, so format is never reliably
// derivable from engine_type alone.
type ModelFormat string

const (
	ModelFormatSafetensors ModelFormat = "safetensors"
	ModelFormatGGUF        ModelFormat = "gguf"
)

// NodeModelInventory mirrors the node_model_inventory table - see
// SCHEMA.md Node model inventory. Current-state answer to "does this node
// have this model right now", distinct from ModelTransfer (history).
type NodeModelInventory struct {
	NodeID   string
	ModelRef string
	// Quantization is part of the composite primary key alongside NodeID,
	// ModelRef, and Format - "" means "whole repo" (vLLM/Aphrodite, or a
	// single-file GGUF repo), the same meaning Model profiles/Model
	// transfers express as NULL, just NOT NULL-compatible for a PK
	// column. Without this, two quantizations of the same model_ref on
	// the same node would collide under the old (node_id, model_ref) key.
	// "" is retired as the meaning for "not yet determined" going
	// forward (migrations/000030_add_model_format.up.sql) - the literal
	// string "UNKNOWN" is used for that instead, so "" keeps meaning only
	// "whole repo", never "we don't know".
	Quantization string
	// Format is part of the composite primary key alongside NodeID,
	// ModelRef, and Quantization - see ModelFormat.
	Format    ModelFormat
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

const nodeModelInventoryColumns = `node_id, model_ref, quantization, format, status, size_bytes, placed_at, placed_via`

func scanNodeModelInventory(row pgx.Row) (*NodeModelInventory, error) {
	var inv NodeModelInventory
	err := row.Scan(&inv.NodeID, &inv.ModelRef, &inv.Quantization, &inv.Format, &inv.Status, &inv.SizeBytes, &inv.PlacedAt, &inv.PlacedVia)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeModelInventoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan node model inventory: %w", err)
	}
	return &inv, nil
}

// Upsert records (or replaces) a node's inventory entry for a model
// quantization+format, keyed on the (node_id, model_ref, quantization,
// format) composite primary key - completing a new transfer for a model
// the node already has replaces the existing row rather than erroring,
// the same ON CONFLICT pattern as PermissionOverrideRepository.Grant.
// quantization "" means "whole repo" - two different quantizations of the
// same model_ref coexist as separate rows rather than colliding, and so
// do two different formats. placedVia must reference the ModelTransfer
// that produced this entry.
func (r *NodeModelInventoryRepository) Upsert(ctx context.Context, nodeID, modelRef, quantization string, format ModelFormat, status InventoryStatus, sizeBytes int64, placedVia string) (*NodeModelInventory, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO node_model_inventory (node_id, model_ref, quantization, format, status, size_bytes, placed_via)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (node_id, model_ref, quantization, format) DO UPDATE SET
		     status = EXCLUDED.status, size_bytes = EXCLUDED.size_bytes, placed_at = now(), placed_via = EXCLUDED.placed_via
		 RETURNING `+nodeModelInventoryColumns,
		nodeID, modelRef, quantization, format, status, sizeBytes, placedVia)

	inv, err := scanNodeModelInventory(row)
	if err != nil {
		return nil, fmt.Errorf("upsert node model inventory: %w", err)
	}
	return inv, nil
}

// Get looks up a node's inventory entry for a specific model
// quantization+format. Returns ErrNodeModelInventoryNotFound if no row
// matches. format is required, not optional - without it, a node_ref/
// quantization pair that happens to exist in both formats (unusual, but
// not impossible - a GGUF repo and an unrelated safetensors repo could in
// principle share a model_ref string) would resolve to whichever row the
// database happened to return first, silently.
func (r *NodeModelInventoryRepository) Get(ctx context.Context, nodeID, modelRef, quantization string, format ModelFormat) (*NodeModelInventory, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+nodeModelInventoryColumns+` FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2 AND quantization = $3 AND format = $4`,
		nodeID, modelRef, quantization, format)
	return scanNodeModelInventory(row)
}

// ListByNode returns every model inventory entry for a node - the raw
// input to the launch-eligibility (Green/Blue/Red) evaluation described in
// ARCHITECTURE.md, and to the Profile-creation cascading picker
// (internal/inventory).
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

// List returns every model inventory entry across every node, ordered by
// model_ref/quantization/format - the raw input to the Inventory page's
// cross-node grouping (internal/inventory.ListGrouped/ListGroupedSimple).
// Unlike ListByNode, this was never callable before this method existed -
// node_model_inventory had no cross-node read path at all.
func (r *NodeModelInventoryRepository) List(ctx context.Context) ([]*NodeModelInventory, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+nodeModelInventoryColumns+` FROM node_model_inventory ORDER BY model_ref, quantization, format, node_id`)
	if err != nil {
		return nil, fmt.Errorf("list node model inventory: %w", err)
	}
	defer rows.Close()

	var entries []*NodeModelInventory
	for rows.Next() {
		inv, err := scanNodeModelInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("list node model inventory: %w", err)
		}
		entries = append(entries, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node model inventory: %w", err)
	}
	return entries, nil
}

// SetStatus updates an existing entry's status without touching any other
// column - used to mark a deleted model copy "removed" rather than hard-
// deleting the row, the same "never delete rows, mark status" precedent
// as Running instances. Returns ErrNodeModelInventoryNotFound if no row
// matches the full composite key.
func (r *NodeModelInventoryRepository) SetStatus(ctx context.Context, nodeID, modelRef, quantization string, format ModelFormat, status InventoryStatus) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE node_model_inventory SET status = $5 WHERE node_id = $1 AND model_ref = $2 AND quantization = $3 AND format = $4`,
		nodeID, modelRef, quantization, format, status)
	if err != nil {
		return fmt.Errorf("set node model inventory status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNodeModelInventoryNotFound
	}
	return nil
}
