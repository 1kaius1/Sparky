// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

// createTestTransfer creates a throwaway internet-sourced, queued transfer
// to satisfy node_model_inventory's placed_via foreign key, cleaned up
// after the test.
func createTestTransfer(t *testing.T, transfers *ModelTransferRepository, destNodeID string, requestedBy *string) *ModelTransfer {
	t.Helper()
	tr, err := transfers.Create(context.Background(), destNodeID, "test-org/test-model", TransferSourceInternet, nil, requestedBy)
	if err != nil {
		t.Fatalf("create test transfer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = transfers.pool.Exec(context.Background(), `DELETE FROM model_transfers WHERE id = $1`, tr.ID)
	})
	return tr
}

func TestModelTransferRepository_Create_Internet(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	requester := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	tr, err := transfers.Create(ctx, dest.ID, "meta-llama/Llama-3-8B", TransferSourceInternet, nil, &requester.ID)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_transfers WHERE id = $1`, tr.ID)
	})

	if tr.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if tr.SourceType != TransferSourceInternet {
		t.Errorf("SourceType = %q, want %q", tr.SourceType, TransferSourceInternet)
	}
	if tr.SourceNodeID != nil {
		t.Errorf("SourceNodeID = %v, want nil for an internet transfer", *tr.SourceNodeID)
	}
	if tr.Status != TransferStatusQueued {
		t.Errorf("Status = %q, want default %q", tr.Status, TransferStatusQueued)
	}
	if tr.RequestedBy == nil || *tr.RequestedBy != requester.ID {
		t.Errorf("RequestedBy = %v, want %q", tr.RequestedBy, requester.ID)
	}
	if tr.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a newly-created transfer", tr.CompletedAt)
	}
}

func TestModelTransferRepository_Create_PeerNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-dest-%s", t.Name()))
	source := createTestNode(t, nodes, fmt.Sprintf("node-source-%s", t.Name()))

	// requestedBy is nil here for the same reason nodes.registered_by can
	// be: the break-glass SuperAdmin is not a Users row.
	tr, err := transfers.Create(ctx, dest.ID, "meta-llama/Llama-3-8B", TransferSourcePeerNode, &source.ID, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_transfers WHERE id = $1`, tr.ID)
	})

	if tr.SourceType != TransferSourcePeerNode {
		t.Errorf("SourceType = %q, want %q", tr.SourceType, TransferSourcePeerNode)
	}
	if tr.SourceNodeID == nil || *tr.SourceNodeID != source.ID {
		t.Errorf("SourceNodeID = %v, want %q", tr.SourceNodeID, source.ID)
	}
	if tr.RequestedBy != nil {
		t.Errorf("RequestedBy = %v, want nil (SuperAdmin is not a Users row)", *tr.RequestedBy)
	}
}

func TestModelTransferRepository_Create_SourceNodeMismatchRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-dest-%s", t.Name()))
	source := createTestNode(t, nodes, fmt.Sprintf("node-source-%s", t.Name()))

	if _, err := transfers.Create(ctx, dest.ID, "test/model", TransferSourcePeerNode, nil, nil); err == nil {
		t.Error("Create() succeeded for a peer_node transfer with no source_node_id, want the CHECK constraint to reject it")
	}

	if _, err := transfers.Create(ctx, dest.ID, "test/model", TransferSourceInternet, &source.ID, nil); err == nil {
		t.Error("Create() succeeded for an internet transfer with a source_node_id set, want the CHECK constraint to reject it")
	}
}

func TestModelTransferRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestTransfer(t, transfers, dest.ID, nil)

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ID != created.ID || got.ModelRef != created.ModelRef {
		t.Errorf("FindByID() = %+v, want ID=%q ModelRef=%q", got, created.ID, created.ModelRef)
	}
}

func TestModelTransferRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	transfers := NewModelTransferRepository(pool)

	_, err := transfers.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrModelTransferNotFound {
		t.Errorf("FindByID() error = %v, want ErrModelTransferNotFound", err)
	}
}

func TestModelTransferRepository_UpdateProgress(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestTransfer(t, transfers, dest.ID, nil)

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
	if got.Status != TransferStatusQueued {
		t.Errorf("Status = %q, want unchanged %q - UpdateProgress must not touch status", got.Status, TransferStatusQueued)
	}
}

func TestModelTransferRepository_SetStatus_NonTerminal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestTransfer(t, transfers, dest.ID, nil)

	if err := transfers.SetStatus(ctx, created.ID, TransferStatusTransferring, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != TransferStatusTransferring {
		t.Errorf("Status = %q, want %q", got.Status, TransferStatusTransferring)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a non-terminal status", got.CompletedAt)
	}
}

func TestModelTransferRepository_SetStatus_Terminal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	dest := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created := createTestTransfer(t, transfers, dest.ID, nil)

	errMsg := "connection reset by peer"
	if err := transfers.SetStatus(ctx, created.ID, TransferStatusFailed, &errMsg); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := transfers.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != TransferStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, TransferStatusFailed)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt is nil, want it set for a terminal status")
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != errMsg {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, errMsg)
	}
}

func TestModelTransferRepository_ListByDestNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	ctx := context.Background()

	destA := createTestNode(t, nodes, fmt.Sprintf("node-a-%s", t.Name()))
	destB := createTestNode(t, nodes, fmt.Sprintf("node-b-%s", t.Name()))

	trA := createTestTransfer(t, transfers, destA.ID, nil)
	_ = createTestTransfer(t, transfers, destB.ID, nil)

	got, err := transfers.ListByDestNode(ctx, destA.ID)
	if err != nil {
		t.Fatalf("ListByDestNode() error: %v", err)
	}
	if len(got) != 1 || got[0].ID != trA.ID {
		t.Errorf("ListByDestNode(%s) = %+v, want exactly [%q]", destA.ID, got, trA.ID)
	}
}
