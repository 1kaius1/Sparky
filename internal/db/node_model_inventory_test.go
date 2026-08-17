// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

func TestNodeModelInventoryRepository_Upsert_InsertsNewRow(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	inventory := NewNodeModelInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transfer := createTestTransfer(t, transfers, node.ID, nil)
	modelRef := "test-org/test-model"

	inv, err := inventory.Upsert(ctx, node.ID, modelRef, "", InventoryStatusPresent, 1024, transfer.ID)
	if err != nil {
		t.Fatalf("Upsert() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`, node.ID, modelRef)
	})

	if inv.Status != InventoryStatusPresent {
		t.Errorf("Status = %q, want %q", inv.Status, InventoryStatusPresent)
	}
	if inv.SizeBytes != 1024 {
		t.Errorf("SizeBytes = %d, want 1024", inv.SizeBytes)
	}
	if inv.PlacedVia != transfer.ID {
		t.Errorf("PlacedVia = %q, want %q", inv.PlacedVia, transfer.ID)
	}
	if inv.Quantization != "" {
		t.Errorf("Quantization = %q, want empty", inv.Quantization)
	}
}

func TestNodeModelInventoryRepository_Upsert_ReplacesExistingRow(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	inventory := NewNodeModelInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	firstTransfer := createTestTransfer(t, transfers, node.ID, nil)
	secondTransfer := createTestTransfer(t, transfers, node.ID, nil)
	modelRef := "test-org/test-model"

	if _, err := inventory.Upsert(ctx, node.ID, modelRef, "", InventoryStatusPresent, 1024, firstTransfer.ID); err != nil {
		t.Fatalf("first Upsert() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`, node.ID, modelRef)
	})

	// Re-placing the same model+quantization via a second transfer must
	// replace the row in place, not insert a second one - (node_id,
	// model_ref, quantization) is the composite primary key.
	updated, err := inventory.Upsert(ctx, node.ID, modelRef, "", InventoryStatusStale, 2048, secondTransfer.ID)
	if err != nil {
		t.Fatalf("second Upsert() error: %v", err)
	}
	if updated.Status != InventoryStatusStale {
		t.Errorf("Status = %q, want %q", updated.Status, InventoryStatusStale)
	}
	if updated.SizeBytes != 2048 {
		t.Errorf("SizeBytes = %d, want 2048", updated.SizeBytes)
	}
	if updated.PlacedVia != secondTransfer.ID {
		t.Errorf("PlacedVia = %q, want %q", updated.PlacedVia, secondTransfer.ID)
	}

	entries, err := inventory.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ListByNode() returned %d entries, want exactly 1 (upsert must replace, not insert)", len(entries))
	}
}

// TestNodeModelInventoryRepository_Upsert_DistinctQuantizationsCoexist is
// the actual bug this feature closes: two quantizations of the same
// model_ref downloaded to the same node used to collide under the old
// (node_id, model_ref) primary key, with the second Upsert silently
// overwriting the first's row. With quantization as a third key
// component, both rows must persist independently.
func TestNodeModelInventoryRepository_Upsert_DistinctQuantizationsCoexist(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	inventory := NewNodeModelInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transferA := createTestTransfer(t, transfers, node.ID, nil)
	transferB := createTestTransfer(t, transfers, node.ID, nil)
	modelRef := "test-org/multi-quant-model"

	if _, err := inventory.Upsert(ctx, node.ID, modelRef, "Q4_K_M", InventoryStatusPresent, 1024, transferA.ID); err != nil {
		t.Fatalf("Upsert(Q4_K_M) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`, node.ID, modelRef)
	})
	if _, err := inventory.Upsert(ctx, node.ID, modelRef, "Q5_K_M", InventoryStatusPresent, 2048, transferB.ID); err != nil {
		t.Fatalf("Upsert(Q5_K_M) error: %v", err)
	}

	entries, err := inventory.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListByNode() returned %d entries, want 2 (distinct quantizations must not collide)", len(entries))
	}

	q4, err := inventory.Get(ctx, node.ID, modelRef, "Q4_K_M")
	if err != nil {
		t.Fatalf("Get(Q4_K_M) error: %v", err)
	}
	if q4.SizeBytes != 1024 {
		t.Errorf("Get(Q4_K_M).SizeBytes = %d, want 1024 (unaffected by the Q5_K_M upsert)", q4.SizeBytes)
	}

	q5, err := inventory.Get(ctx, node.ID, modelRef, "Q5_K_M")
	if err != nil {
		t.Fatalf("Get(Q5_K_M) error: %v", err)
	}
	if q5.SizeBytes != 2048 {
		t.Errorf("Get(Q5_K_M).SizeBytes = %d, want 2048", q5.SizeBytes)
	}
}

func TestNodeModelInventoryRepository_Get_NotFound(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	inventory := NewNodeModelInventoryRepository(pool)

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	_, err := inventory.Get(context.Background(), node.ID, "no-such/model", "")
	if err != ErrNodeModelInventoryNotFound {
		t.Errorf("Get() error = %v, want ErrNodeModelInventoryNotFound", err)
	}
}

func TestNodeModelInventoryRepository_ListByNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewModelTransferRepository(pool)
	inventory := NewNodeModelInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transferA := createTestTransfer(t, transfers, node.ID, nil)
	transferB := createTestTransfer(t, transfers, node.ID, nil)

	modelA := "test-org/model-a"
	modelB := "test-org/model-b"

	if _, err := inventory.Upsert(ctx, node.ID, modelA, "", InventoryStatusPresent, 1024, transferA.ID); err != nil {
		t.Fatalf("Upsert(modelA) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`, node.ID, modelA)
	})
	if _, err := inventory.Upsert(ctx, node.ID, modelB, "", InventoryStatusPresent, 2048, transferB.ID); err != nil {
		t.Fatalf("Upsert(modelB) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_model_inventory WHERE node_id = $1 AND model_ref = $2`, node.ID, modelB)
	})

	got, err := inventory.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListByNode() returned %d entries, want 2", len(got))
	}
}
