// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Each test below registers its own audit_log cleanup after calling
// createTestUser, so it runs first (t.Cleanup is LIFO) and the actor_id
// foreign key never blocks the user row's deletion.

func TestAuditRepository_Write(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	auditLog := NewAuditRepository(pool)
	ctx := context.Background()

	actor := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-actor", t.Name()))
	target := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-target", t.Name()))

	detail, err := json.Marshal(map[string]string{"from_tier": "read_only", "to_tier": "developer"})
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}

	rec, err := auditLog.Write(ctx, &actor.ID, false, "elevated_user", "user", target.ID, detail)
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1`, rec.ID)
	})

	if rec.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if rec.ActorID == nil || *rec.ActorID != actor.ID {
		t.Errorf("ActorID = %v, want %q", rec.ActorID, actor.ID)
	}
	if rec.IsSuperAdminAction {
		t.Error("IsSuperAdminAction = true, want false")
	}
	if rec.Action != "elevated_user" {
		t.Errorf("Action = %q, want %q", rec.Action, "elevated_user")
	}
	if rec.ObjectType != "user" {
		t.Errorf("ObjectType = %q, want %q", rec.ObjectType, "user")
	}
	if rec.ObjectID != target.ID {
		t.Errorf("ObjectID = %q, want %q", rec.ObjectID, target.ID)
	}
	// Compared as decoded values, not raw bytes - Postgres's jsonb type
	// does not preserve key order or formatting.
	var gotDetail, wantDetail map[string]string
	if err := json.Unmarshal(rec.Detail, &gotDetail); err != nil {
		t.Fatalf("unmarshal returned Detail: %v", err)
	}
	if err := json.Unmarshal(detail, &wantDetail); err != nil {
		t.Fatalf("unmarshal expected detail: %v", err)
	}
	if gotDetail["from_tier"] != wantDetail["from_tier"] || gotDetail["to_tier"] != wantDetail["to_tier"] {
		t.Errorf("Detail = %v, want %v", gotDetail, wantDetail)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a timestamp")
	}
}

func TestAuditRepository_Write_SuperAdminActionHasNilActor(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	auditLog := NewAuditRepository(pool)
	ctx := context.Background()

	target := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-target", t.Name()))

	// The break-glass SuperAdmin is not a Users row, so it cannot be an
	// actor_id - see SCHEMA.md Break-glass credential. is_superadmin_action
	// is the signal that distinguishes this from an actor that is merely
	// unknown.
	rec, err := auditLog.Write(ctx, nil, true, "elevated_user", "user", target.ID, nil)
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1`, rec.ID)
	})

	if rec.ActorID != nil {
		t.Errorf("ActorID = %v, want nil", *rec.ActorID)
	}
	if !rec.IsSuperAdminAction {
		t.Error("IsSuperAdminAction = false, want true")
	}
	if rec.Detail != nil {
		t.Errorf("Detail = %s, want nil", rec.Detail)
	}
}

func TestAuditRepository_List(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	auditLog := NewAuditRepository(pool)
	ctx := context.Background()

	actor := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-actor", t.Name()))
	target := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-target", t.Name()))

	recA, err := auditLog.Write(ctx, &actor.ID, false, "elevated_user", "user", target.ID, nil)
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1`, recA.ID)
	})

	recB, err := auditLog.Write(ctx, nil, true, "set_superadmin_password", "break_glass_credential", target.ID, nil)
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1`, recB.ID)
	})

	got, err := auditLog.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, rec := range got {
		if rec.ID == recA.ID {
			foundA = true
		}
		if rec.ID == recB.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d records, missing one or both of the two just written", len(got))
	}
}
