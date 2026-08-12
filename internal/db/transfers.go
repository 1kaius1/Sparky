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

// TransferSourceType mirrors the transfer_source_type Postgres enum - see
// migrations/000008_create_model_transfers.up.sql and SCHEMA.md Model
// transfers.
type TransferSourceType string

const (
	TransferSourceInternet TransferSourceType = "internet"
	TransferSourcePeerNode TransferSourceType = "peer_node"
)

// TransferStatus mirrors the transfer_status Postgres enum.
type TransferStatus string

const (
	TransferStatusQueued       TransferStatus = "queued"
	TransferStatusTransferring TransferStatus = "transferring"
	TransferStatusCompleted    TransferStatus = "completed"
	TransferStatusFailed       TransferStatus = "failed"
	TransferStatusCancelled    TransferStatus = "cancelled"
)

// isTerminalTransferStatus reports whether status ends a transfer's
// lifecycle - SetStatus uses this to decide whether to also stamp
// completed_at.
func isTerminalTransferStatus(status TransferStatus) bool {
	return status == TransferStatusCompleted || status == TransferStatusFailed || status == TransferStatusCancelled
}

// ModelTransfer mirrors the model_transfers table - see SCHEMA.md Model
// transfers.
type ModelTransfer struct {
	ID               string
	DestNodeID       string
	ModelRef         string
	SourceType       TransferSourceType
	SourceNodeID     *string
	Status           TransferStatus
	BytesTransferred int64
	BytesTotal       int64
	RequestedBy      *string
	RequestedAt      time.Time
	CompletedAt      *time.Time
	ErrorMessage     *string
}

// ErrModelTransferNotFound is returned when a lookup finds no matching row.
var ErrModelTransferNotFound = errors.New("model transfer not found")

// ModelTransferRepository is the only component that queries the
// model_transfers table directly - see CLAUDE.md: the repository layer is
// the only place that accesses the database directly.
type ModelTransferRepository struct {
	pool *pgxpool.Pool
}

// NewModelTransferRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewModelTransferRepository(pool *pgxpool.Pool) *ModelTransferRepository {
	return &ModelTransferRepository{pool: pool}
}

const modelTransferColumns = `id, dest_node_id, model_ref, source_type, source_node_id, status,
	bytes_transferred, bytes_total, requested_by, requested_at, completed_at, error_message`

func scanModelTransfer(row pgx.Row) (*ModelTransfer, error) {
	var t ModelTransfer
	err := row.Scan(&t.ID, &t.DestNodeID, &t.ModelRef, &t.SourceType, &t.SourceNodeID, &t.Status,
		&t.BytesTransferred, &t.BytesTotal, &t.RequestedBy, &t.RequestedAt, &t.CompletedAt, &t.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrModelTransferNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan model transfer: %w", err)
	}
	return &t, nil
}

// Create inserts a new transfer in TransferStatusQueued. sourceNodeID must
// be nil for TransferSourceInternet and non-nil for TransferSourcePeerNode
// - the database enforces this via model_transfers_source_node_matches_type
// regardless of caller discipline. requestedBy is nil only for the
// break-glass SuperAdmin, which is not a Users row.
func (r *ModelTransferRepository) Create(ctx context.Context, destNodeID, modelRef string, sourceType TransferSourceType, sourceNodeID *string, requestedBy *string) (*ModelTransfer, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO model_transfers (dest_node_id, model_ref, source_type, source_node_id, requested_by)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+modelTransferColumns,
		destNodeID, modelRef, sourceType, sourceNodeID, requestedBy)

	t, err := scanModelTransfer(row)
	if err != nil {
		return nil, fmt.Errorf("create model transfer: %w", err)
	}
	return t, nil
}

// FindByID looks up a transfer by its internal ID. Returns
// ErrModelTransferNotFound if no row matches.
func (r *ModelTransferRepository) FindByID(ctx context.Context, id string) (*ModelTransfer, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+modelTransferColumns+` FROM model_transfers WHERE id = $1`, id)
	return scanModelTransfer(row)
}

// UpdateProgress records bytes transferred/total as an agent streams
// progress periodically (docs/AGENT.md Service Architecture Notes:
// Transfer goroutines) - it does not change status, since a progress
// update can arrive independently of a status transition.
func (r *ModelTransferRepository) UpdateProgress(ctx context.Context, id string, bytesTransferred, bytesTotal int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE model_transfers SET bytes_transferred = $1, bytes_total = $2 WHERE id = $3`,
		bytesTransferred, bytesTotal, id)
	if err != nil {
		return fmt.Errorf("update model transfer progress for %s: %w", id, err)
	}
	return nil
}

// SetStatus transitions a transfer's status. completed_at is also stamped
// when status is one of the terminal states (completed, failed,
// cancelled) - errorMessage is only meaningful for a failed transfer but
// is accepted for any status, since a caller may want to clear a
// previously-set message.
func (r *ModelTransferRepository) SetStatus(ctx context.Context, id string, status TransferStatus, errorMessage *string) error {
	var err error
	if isTerminalTransferStatus(status) {
		_, err = r.pool.Exec(ctx,
			`UPDATE model_transfers SET status = $1, error_message = $2, completed_at = now() WHERE id = $3`,
			status, errorMessage, id)
	} else {
		_, err = r.pool.Exec(ctx,
			`UPDATE model_transfers SET status = $1, error_message = $2 WHERE id = $3`,
			status, errorMessage, id)
	}
	if err != nil {
		return fmt.Errorf("set status for model transfer %s: %w", id, err)
	}
	return nil
}

// ListByDestNode returns every transfer targeting a node, most recently
// requested first - the Transfers dashboard's per-node history view.
func (r *ModelTransferRepository) ListByDestNode(ctx context.Context, destNodeID string) ([]*ModelTransfer, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+modelTransferColumns+` FROM model_transfers WHERE dest_node_id = $1 ORDER BY requested_at DESC`,
		destNodeID)
	if err != nil {
		return nil, fmt.Errorf("list model transfers for node %s: %w", destNodeID, err)
	}
	defer rows.Close()

	var transfers []*ModelTransfer
	for rows.Next() {
		t, err := scanModelTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("list model transfers for node %s: %w", destNodeID, err)
		}
		transfers = append(transfers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model transfers for node %s: %w", destNodeID, err)
	}
	return transfers, nil
}
