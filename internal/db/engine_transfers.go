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

// EngineTransferStatus mirrors the engine_transfer_status Postgres enum -
// see migrations/000015_create_engine_transfers.up.sql and SCHEMA.md Engine
// transfers.
type EngineTransferStatus string

const (
	EngineTransferStatusQueued       EngineTransferStatus = "queued"
	EngineTransferStatusTransferring EngineTransferStatus = "transferring"
	EngineTransferStatusCompleted    EngineTransferStatus = "completed"
	EngineTransferStatusFailed       EngineTransferStatus = "failed"
	EngineTransferStatusCancelled    EngineTransferStatus = "cancelled"
)

// isTerminalEngineTransferStatus reports whether status ends an engine
// transfer's lifecycle - SetStatus uses this to decide whether to also
// stamp completed_at, mirroring isTerminalTransferStatus.
func isTerminalEngineTransferStatus(status EngineTransferStatus) bool {
	return status == EngineTransferStatusCompleted || status == EngineTransferStatusFailed || status == EngineTransferStatusCancelled
}

// EngineTransfer mirrors the engine_transfers table - see SCHEMA.md Engine
// transfers.
type EngineTransfer struct {
	ID               string
	DestNodeID       string
	EngineType       ProfileEngineType
	Version          string
	Status           EngineTransferStatus
	BytesTransferred int64
	BytesTotal       int64
	RequestedBy      *string
	RequestedAt      time.Time
	CompletedAt      *time.Time
	ErrorMessage     *string
}

// ErrEngineTransferNotFound is returned when a lookup finds no matching row.
var ErrEngineTransferNotFound = errors.New("engine transfer not found")

// EngineTransferRepository is the only component that queries the
// engine_transfers table directly - see CLAUDE.md: the repository layer is
// the only place that accesses the database directly.
type EngineTransferRepository struct {
	pool *pgxpool.Pool
}

// NewEngineTransferRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewEngineTransferRepository(pool *pgxpool.Pool) *EngineTransferRepository {
	return &EngineTransferRepository{pool: pool}
}

const engineTransferColumns = `id, dest_node_id, engine_type, version, status,
	bytes_transferred, bytes_total, requested_by, requested_at, completed_at, error_message`

func scanEngineTransfer(row pgx.Row) (*EngineTransfer, error) {
	var t EngineTransfer
	err := row.Scan(&t.ID, &t.DestNodeID, &t.EngineType, &t.Version, &t.Status,
		&t.BytesTransferred, &t.BytesTotal, &t.RequestedBy, &t.RequestedAt, &t.CompletedAt, &t.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEngineTransferNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan engine transfer: %w", err)
	}
	return &t, nil
}

// Create inserts a new engine transfer in EngineTransferStatusQueued.
// requestedBy is nil only for the break-glass SuperAdmin, which is not a
// Users row.
func (r *EngineTransferRepository) Create(ctx context.Context, destNodeID string, engineType ProfileEngineType, version string, requestedBy *string) (*EngineTransfer, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO engine_transfers (dest_node_id, engine_type, version, requested_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+engineTransferColumns,
		destNodeID, engineType, version, requestedBy)

	t, err := scanEngineTransfer(row)
	if err != nil {
		return nil, fmt.Errorf("create engine transfer: %w", err)
	}
	return t, nil
}

// FindByID looks up an engine transfer by its internal ID. Returns
// ErrEngineTransferNotFound if no row matches.
func (r *EngineTransferRepository) FindByID(ctx context.Context, id string) (*EngineTransfer, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+engineTransferColumns+` FROM engine_transfers WHERE id = $1`, id)
	return scanEngineTransfer(row)
}

// UpdateProgress records bytes transferred/total as an agent streams
// progress periodically - it does not change status, mirroring
// ModelTransferRepository.UpdateProgress.
func (r *EngineTransferRepository) UpdateProgress(ctx context.Context, id string, bytesTransferred, bytesTotal int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE engine_transfers SET bytes_transferred = $1, bytes_total = $2 WHERE id = $3`,
		bytesTransferred, bytesTotal, id)
	if err != nil {
		return fmt.Errorf("update engine transfer progress for %s: %w", id, err)
	}
	return nil
}

// SetStatus transitions an engine transfer's status. completed_at is also
// stamped when status is one of the terminal states (completed, failed,
// cancelled), mirroring ModelTransferRepository.SetStatus.
func (r *EngineTransferRepository) SetStatus(ctx context.Context, id string, status EngineTransferStatus, errorMessage *string) error {
	var err error
	if isTerminalEngineTransferStatus(status) {
		_, err = r.pool.Exec(ctx,
			`UPDATE engine_transfers SET status = $1, error_message = $2, completed_at = now() WHERE id = $3`,
			status, errorMessage, id)
	} else {
		_, err = r.pool.Exec(ctx,
			`UPDATE engine_transfers SET status = $1, error_message = $2 WHERE id = $3`,
			status, errorMessage, id)
	}
	if err != nil {
		return fmt.Errorf("set status for engine transfer %s: %w", id, err)
	}
	return nil
}

// List returns every engine transfer across every node, most recently
// requested first.
func (r *EngineTransferRepository) List(ctx context.Context) ([]*EngineTransfer, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+engineTransferColumns+` FROM engine_transfers ORDER BY requested_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list engine transfers: %w", err)
	}
	defer rows.Close()

	var transfers []*EngineTransfer
	for rows.Next() {
		t, err := scanEngineTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("list engine transfers: %w", err)
		}
		transfers = append(transfers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list engine transfers: %w", err)
	}
	return transfers, nil
}

// ListByDestNode returns every engine transfer targeting a node, most
// recently requested first.
func (r *EngineTransferRepository) ListByDestNode(ctx context.Context, destNodeID string) ([]*EngineTransfer, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+engineTransferColumns+` FROM engine_transfers WHERE dest_node_id = $1 ORDER BY requested_at DESC`,
		destNodeID)
	if err != nil {
		return nil, fmt.Errorf("list engine transfers for node %s: %w", destNodeID, err)
	}
	defer rows.Close()

	var transfers []*EngineTransfer
	for rows.Next() {
		t, err := scanEngineTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("list engine transfers for node %s: %w", destNodeID, err)
		}
		transfers = append(transfers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list engine transfers for node %s: %w", destNodeID, err)
	}
	return transfers, nil
}
