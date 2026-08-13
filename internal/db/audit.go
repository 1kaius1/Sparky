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

// List returns every audit record, most recently created first - the
// Audit log page's default chronological view (SCHEMA.md Audit log;
// migrations/000004_create_audit_log.up.sql's created_at index exists for
// exactly this query). RBAC gating (Admin-tier only, per CLAUDE.md
// Frontend Conventions) happens above this layer, in internal/audit.
func (r *AuditRepository) List(ctx context.Context) ([]*AuditRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, actor_id, is_superadmin_action, action, object_type, object_id, detail, created_at
		 FROM audit_log ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	defer rows.Close()

	var records []*AuditRecord
	for rows.Next() {
		var rec AuditRecord
		if err := rows.Scan(&rec.ID, &rec.ActorID, &rec.IsSuperAdminAction, &rec.Action,
			&rec.ObjectType, &rec.ObjectID, &rec.Detail, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("list audit records: %w", err)
		}
		records = append(records, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	return records, nil
}
