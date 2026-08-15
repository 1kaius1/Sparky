// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

// createTestEngineTransfer creates a throwaway queued engine transfer to
// satisfy node_engine_inventory's placed_via foreign key, cleaned up after
// the test - mirrors createTestTransfer.
func createTestEngineTransfer(t *testing.T, transfers *EngineTransferRepository, destNodeID string, requestedBy *string) *EngineTransfer {
	t.Helper()
	tr, err := transfers.Create(context.Background(), destNodeID, ProfileEngineLlamaCPP, "b4610", requestedBy)
	if err != nil {
		t.Fatalf("create test engine transfer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = transfers.pool.Exec(context.Background(), `DELETE FROM engine_transfers WHERE id = $1`, tr.ID)
	})
	return tr
}

func TestEngineTransferRepository_Create(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	requester := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	tr, err := transfers.Create(ctx, dest.ID, ProfileEngineLlamaCPP, "b4610", &requester.ID)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM engine_transfers WHERE id = $1`, tr.ID)
	})

	if tr.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if tr.EngineType != ProfileEngineLlamaCPP {
		t.Errorf("EngineType = %q, want %q", tr.EngineType, ProfileEngineLlamaCPP)
	}
	if tr.Version != "b4610" {
		t.Errorf("Version = %q, want %q", tr.Version, "b4610")
	}
	if tr.Status != EngineTransferStatusQueued {
		t.Errorf("Status = %q, want default %q", tr.Status, EngineTransferStatusQueued)
	}
	if tr.RequestedBy == nil || *tr.RequestedBy != requester.ID {
		t.Errorf("RequestedBy = %v, want %q", tr.RequestedBy, requester.ID)
	}
	if tr.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a newly-created transfer", tr.CompletedAt)
	}
}

func TestEngineTransferRepository_Create_NilRequestedBy(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	// requestedBy is nil for the same reason model_transfers' can be: the
	// break-glass SuperAdmin is not a Users row.
	tr, err := transfers.Create(ctx, dest.ID, ProfileEngineLlamaCPP, "b4610", nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM engine_transfers WHERE id = $1`, tr.ID)
	})

	if tr.RequestedBy != nil {
		t.Errorf("RequestedBy = %v, want nil", *tr.RequestedBy)
	}
}

func TestEngineTransferRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestEngineTransfer(t, transfers, dest.ID, nil)

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ID != created.ID || got.Version != created.Version {
		t.Errorf("FindByID() = %+v, want ID=%q Version=%q", got, created.ID, created.Version)
	}
}

func TestEngineTransferRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	transfers := NewEngineTransferRepository(pool)

	_, err := transfers.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrEngineTransferNotFound {
		t.Errorf("FindByID() error = %v, want ErrEngineTransferNotFound", err)
	}
}

func TestEngineTransferRepository_UpdateProgress(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestEngineTransfer(t, transfers, dest.ID, nil)

	if err := transfers.UpdateProgress(ctx, created.ID, 4096, 8192); err != nil {
		t.Fatalf("UpdateProgress() error: %v", err)
	}

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.BytesTransferred != 4096 || got.BytesTotal != 8192 {
		t.Errorf("BytesTransferred/BytesTotal = %d/%d, want 4096/8192", got.BytesTransferred, got.BytesTotal)
	}
	if got.Status != EngineTransferStatusQueued {
		t.Errorf("Status = %q, want unchanged %q - UpdateProgress must not touch status", got.Status, EngineTransferStatusQueued)
	}
}

func TestEngineTransferRepository_SetStatus_NonTerminal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestEngineTransfer(t, transfers, dest.ID, nil)

	if err := transfers.SetStatus(ctx, created.ID, EngineTransferStatusTransferring, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != EngineTransferStatusTransferring {
		t.Errorf("Status = %q, want %q", got.Status, EngineTransferStatusTransferring)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a non-terminal status", got.CompletedAt)
	}
}

func TestEngineTransferRepository_SetStatus_Terminal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestEngineTransfer(t, transfers, dest.ID, nil)

	errMsg := "checksum mismatch"
	if err := transfers.SetStatus(ctx, created.ID, EngineTransferStatusFailed, &errMsg); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != EngineTransferStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, EngineTransferStatusFailed)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt is nil, want it set for a terminal status")
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != errMsg {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, errMsg)
	}
}

func TestEngineTransferRepository_ListByDestNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	destA := createTestNode(t, nodes, fmt.Sprintf("node-a-%s", t.Name()))
	destB := createTestNode(t, nodes, fmt.Sprintf("node-b-%s", t.Name()))

	trA := createTestEngineTransfer(t, transfers, destA.ID, nil)
	_ = createTestEngineTransfer(t, transfers, destB.ID, nil)

	got, err := transfers.ListByDestNode(ctx, destA.ID)
	if err != nil {
		t.Fatalf("ListByDestNode() error: %v", err)
	}
	if len(got) != 1 || got[0].ID != trA.ID {
		t.Errorf("ListByDestNode(%s) = %+v, want exactly [%q]", destA.ID, got, trA.ID)
	}
}

func TestEngineTransferRepository_List(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	ctx := context.Background()

	destA := createTestNode(t, nodes, fmt.Sprintf("node-a-%s", t.Name()))
	destB := createTestNode(t, nodes, fmt.Sprintf("node-b-%s", t.Name()))

	trA := createTestEngineTransfer(t, transfers, destA.ID, nil)
	trB := createTestEngineTransfer(t, transfers, destB.ID, nil)

	got, err := transfers.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, tr := range got {
		if tr.ID == trA.ID {
			foundA = true
		}
		if tr.ID == trB.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d transfers, missing one or both of the two just created (across different dest nodes)", len(got))
	}
}
