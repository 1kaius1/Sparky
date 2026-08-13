// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit is the cross-cutting audit log writer - see SCHEMA.md
// Audit log and ARCHITECTURE.md Audit Log. Every state-changing action, by
// anyone including the SuperAdmin, goes through Recorder.Record, which
// writes the authoritative record to Postgres and additionally emits it as
// a structured JSON line to a configured stream (stdout in production),
// which any log shipper already watching that output picks up automatically
// - see ARCHITECTURE.md Audit Log, Long-Term Forwarding. The optional
// active syslog/GELF push described in SCHEMA.md Audit settings is a
// separate mechanism, not implemented here - see PLANNING.md Decisions Log.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// store is the subset of *db.AuditRepository this package needs, narrow
// enough to fake in tests without a real Postgres instance - same pattern
// used throughout internal/httpapi and internal/rbac. Covers both the
// append path (Write) and the Audit log page's read path (List) - one
// interface, since *db.AuditRepository is Recorder's only sanctioned way
// to reach the audit_log table either way.
type store interface {
	Write(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail json.RawMessage) (*db.AuditRecord, error)
	List(ctx context.Context) ([]*db.AuditRecord, error)
}

// Recorder is the only sanctioned path to the audit log - callers never
// access *db.AuditRepository directly, matching the Handler -> Service
// Layer -> Repository pattern CLAUDE.md establishes elsewhere.
type Recorder struct {
	store  store
	stream io.Writer
}

// NewRecorder constructs a Recorder. In production, auditLog is a
// *db.AuditRepository; tests may pass a narrower fake instead, same
// pattern as internal/rbac's and internal/httpapi's userStore. stream
// receives one JSON line per recorded action - pass os.Stdout in
// production, per ARCHITECTURE.md Audit Log's always-on stdout stream. A
// nil stream disables that emission without affecting the Postgres write.
func NewRecorder(auditLog store, stream io.Writer) *Recorder {
	return &Recorder{store: auditLog, stream: stream}
}

// Record appends a state-changing action to the audit log. actorID is nil
// only for the SuperAdmin break-glass account (isSuperAdminAction true),
// which is not a Users row - see SCHEMA.md Break-glass credential. detail
// is optional, action-specific context.
//
// The Postgres write is authoritative; a failure there is returned to the
// caller, per ARCHITECTURE.md's "no exceptions" audit guarantee - callers
// should treat a Record failure as seriously as the state change it is
// documenting. The stdout JSON line, by contrast, is fire-and-forget and
// additive - see ARCHITECTURE.md Audit Log, Long-Term Forwarding: it never
// gates or fails the call.
func (r *Recorder) Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error {
	var raw json.RawMessage
	if detail != nil {
		encoded, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("encode audit detail: %w", err)
		}
		raw = encoded
	}

	rec, err := r.store.Write(ctx, actorID, isSuperAdminAction, action, objectType, objectID, raw)
	if err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}

	r.emit(rec)
	return nil
}

// List returns every audit record, most recently created first, if actor
// is permitted to view the Audit log - see rbac.CanViewAuditLog. The RBAC
// check lives here, not only at the HTTP layer, so the guarantee travels
// with the method itself regardless of caller - the same reasoning
// internal/transfers.Service.InitiateTransfer's own internal check
// documents. Returns rbac.ErrNotPermitted if actor is not permitted.
func (r *Recorder) List(ctx context.Context, actor rbac.Actor) ([]*db.AuditRecord, error) {
	if !rbac.CanViewAuditLog(actor) {
		return nil, rbac.ErrNotPermitted
	}

	records, err := r.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	return records, nil
}

// streamLine is the JSON shape written to stream for each record -
// "distinguishable from general app logs" per ARCHITECTURE.md Audit Log,
// Long-Term Forwarding, hence the fixed Type field.
type streamLine struct {
	Type               string          `json:"type"`
	ID                 string          `json:"id"`
	ActorID            *string         `json:"actor_id,omitempty"`
	IsSuperAdminAction bool            `json:"is_superadmin_action"`
	Action             string          `json:"action"`
	ObjectType         string          `json:"object_type"`
	ObjectID           string          `json:"object_id"`
	Detail             json.RawMessage `json:"detail,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

func (r *Recorder) emit(rec *db.AuditRecord) {
	if r.stream == nil {
		return
	}

	encoded, err := json.Marshal(streamLine{
		Type:               "audit",
		ID:                 rec.ID,
		ActorID:            rec.ActorID,
		IsSuperAdminAction: rec.IsSuperAdminAction,
		Action:             rec.Action,
		ObjectType:         rec.ObjectType,
		ObjectID:           rec.ObjectID,
		Detail:             rec.Detail,
		CreatedAt:          rec.CreatedAt,
	})
	if err != nil {
		// Unreachable in practice - streamLine's fields are all simple
		// JSON-safe types copied from a record Postgres already
		// accepted. Not worth failing the call over.
		return
	}

	_, _ = r.stream.Write(append(encoded, '\n'))
}
