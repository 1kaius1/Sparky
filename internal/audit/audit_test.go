// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// fakeWriter implements writer for tests without a real Postgres - same
// pattern as internal/rbac's fakeUserStore.
type fakeWriter struct {
	writeErr error
	calls    []call
}

type call struct {
	actorID            *string
	isSuperAdminAction bool
	action             string
	objectType         string
	objectID           string
	detail             json.RawMessage
}

func (f *fakeWriter) Write(_ context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail json.RawMessage) (*db.AuditRecord, error) {
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	f.calls = append(f.calls, call{actorID, isSuperAdminAction, action, objectType, objectID, detail})
	return &db.AuditRecord{
		ID:                 "audit-1",
		ActorID:            actorID,
		IsSuperAdminAction: isSuperAdminAction,
		Action:             action,
		ObjectType:         objectType,
		ObjectID:           objectID,
		Detail:             detail,
		CreatedAt:          time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}, nil
}

func TestRecorder_Record_WritesAndEmitsStreamLine(t *testing.T) {
	fw := &fakeWriter{}
	var stream bytes.Buffer
	r := NewRecorder(fw, &stream)

	actorID := "admin-1"
	err := r.Record(context.Background(), &actorID, false, "elevated_user", "user", "target-1",
		map[string]any{"from_tier": "read_only", "to_tier": "developer"})
	if err != nil {
		t.Fatalf("Record() error: %v", err)
	}

	if len(fw.calls) != 1 {
		t.Fatalf("writer.Write called %d times, want 1", len(fw.calls))
	}
	got := fw.calls[0]
	if got.actorID == nil || *got.actorID != actorID {
		t.Errorf("actorID = %v, want %q", got.actorID, actorID)
	}
	if got.action != "elevated_user" || got.objectType != "user" || got.objectID != "target-1" {
		t.Errorf("call = %+v, want action=elevated_user objectType=user objectID=target-1", got)
	}
	if !strings.Contains(string(got.detail), "developer") {
		t.Errorf("detail = %s, want it to contain the marshaled map", got.detail)
	}

	line := stream.String()
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("stream output = %q, want a trailing newline", line)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("stream output is not valid JSON: %v", err)
	}
	if decoded["type"] != "audit" {
		t.Errorf(`stream "type" = %v, want "audit"`, decoded["type"])
	}
	if decoded["action"] != "elevated_user" {
		t.Errorf(`stream "action" = %v, want "elevated_user"`, decoded["action"])
	}
}

func TestRecorder_Record_SuperAdminActionOmitsActorID(t *testing.T) {
	fw := &fakeWriter{}
	var stream bytes.Buffer
	r := NewRecorder(fw, &stream)

	if err := r.Record(context.Background(), nil, true, "elevated_user", "user", "target-1", nil); err != nil {
		t.Fatalf("Record() error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(stream.Bytes(), &decoded); err != nil {
		t.Fatalf("stream output is not valid JSON: %v", err)
	}
	if _, present := decoded["actor_id"]; present {
		t.Errorf(`stream output has "actor_id" = %v, want it omitted for a SuperAdmin action`, decoded["actor_id"])
	}
	if decoded["is_superadmin_action"] != true {
		t.Errorf(`stream "is_superadmin_action" = %v, want true`, decoded["is_superadmin_action"])
	}
}

func TestRecorder_Record_NilStreamSkipsEmission(t *testing.T) {
	fw := &fakeWriter{}
	r := NewRecorder(fw, nil)

	if err := r.Record(context.Background(), nil, true, "elevated_user", "user", "target-1", nil); err != nil {
		t.Fatalf("Record() error: %v", err)
	}
	if len(fw.calls) != 1 {
		t.Fatalf("writer.Write called %d times, want 1", len(fw.calls))
	}
}

func TestRecorder_Record_WriteFailurePropagates(t *testing.T) {
	fw := &fakeWriter{writeErr: errors.New("database unreachable")}
	var stream bytes.Buffer
	r := NewRecorder(fw, &stream)

	err := r.Record(context.Background(), nil, true, "elevated_user", "user", "target-1", nil)
	if err == nil {
		t.Fatal("Record() succeeded despite a Write failure")
	}
	if stream.Len() != 0 {
		t.Errorf("stream got a line despite the write failing: %q", stream.String())
	}
}
