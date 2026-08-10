// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditRecord mirrors the audit_log table - see
// migrations/000004_create_audit_log.up.sql and SCHEMA.md Audit log.
type AuditRecord struct {
	ID                 string
	ActorID            *string
	IsSuperAdminAction bool
	Action             string
	ObjectType         string
	ObjectID           string
	Detail             json.RawMessage
	CreatedAt          time.Time
}

// AuditRepository is the only component that queries the audit_log table
// directly - see CLAUDE.md: the repository layer is the only place that
// accesses the database directly. There is deliberately no Update or
// Delete: the audit log is append-only, per SCHEMA.md Audit log.
type AuditRepository struct {
	pool *pgxpool.Pool
}

// NewAuditRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewAuditRepository(pool *pgxpool.Pool) *AuditRepository {
	return &AuditRepository{pool: pool}
}

// Write appends a record to the audit log. actorID is nil only for the
// break-glass SuperAdmin, which is not a Users row - see SCHEMA.md
// Break-glass credential; isSuperAdminAction distinguishes that case from
// an actor_id that is merely missing. detail may be nil.
func (r *AuditRepository) Write(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail json.RawMessage) (*AuditRecord, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO audit_log (actor_id, is_superadmin_action, action, object_type, object_id, detail)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, actor_id, is_superadmin_action, action, object_type, object_id, detail, created_at`,
		actorID, isSuperAdminAction, action, objectType, objectID, detail)

	var rec AuditRecord
	err := row.Scan(&rec.ID, &rec.ActorID, &rec.IsSuperAdminAction, &rec.Action,
		&rec.ObjectType, &rec.ObjectID, &rec.Detail, &rec.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("write audit record: %w", err)
	}
	return &rec, nil
}
