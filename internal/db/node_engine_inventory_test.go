// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

func TestNodeEngineInventoryRepository_Upsert_InsertsNewRow(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transfer := createTestEngineTransfer(t, transfers, node.ID, nil)
	installPath := "/opt/sparky/serviceloop/engines/llamacpp/b4610"

	inv, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, installPath, 1024, transfer.ID)
	if err != nil {
		t.Fatalf("Upsert() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4610")
	})

	if inv.Status != InventoryStatusPresent {
		t.Errorf("Status = %q, want %q", inv.Status, InventoryStatusPresent)
	}
	if inv.InstallPath != installPath {
		t.Errorf("InstallPath = %q, want %q", inv.InstallPath, installPath)
	}
	if inv.SizeBytes != 1024 {
		t.Errorf("SizeBytes = %d, want 1024", inv.SizeBytes)
	}
	if inv.PlacedVia != transfer.ID {
		t.Errorf("PlacedVia = %q, want %q", inv.PlacedVia, transfer.ID)
	}
}

func TestNodeEngineInventoryRepository_Upsert_SameVersionReplacesRow(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	firstTransfer := createTestEngineTransfer(t, transfers, node.ID, nil)
	secondTransfer := createTestEngineTransfer(t, transfers, node.ID, nil)

	if _, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, "/path/b4610", 1024, firstTransfer.ID); err != nil {
		t.Fatalf("first Upsert() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4610")
	})

	// Re-provisioning the exact same version must replace the row in
	// place, not insert a second one - (node_id, engine_type, version) is
	// the composite primary key.
	updated, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, "/path/b4610", 2048, secondTransfer.ID)
	if err != nil {
		t.Fatalf("second Upsert() error: %v", err)
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
		t.Errorf("ListByNode() returned %d entries, want exactly 1 (same-version upsert must replace, not insert)", len(entries))
	}
}

func TestNodeEngineInventoryRepository_Upsert_DifferentVersionsCoexist(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transferA := createTestEngineTransfer(t, transfers, node.ID, nil)
	transferB := createTestEngineTransfer(t, transfers, node.ID, nil)

	// Two different versions of the same engine on the same node must
	// coexist as separate rows, not overwrite each other - this is the
	// whole point of keying on (node_id, engine_type, version) rather than
	// (node_id, engine_type), so a future profile can pin either one for
	// side-by-side comparison.
	if _, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4523", InventoryStatusPresent, "/path/b4523", 1024, transferA.ID); err != nil {
		t.Fatalf("Upsert(b4523) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4523")
	})
	if _, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, "/path/b4610", 2048, transferB.ID); err != nil {
		t.Fatalf("Upsert(b4610) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4610")
	})

	entries, err := inventory.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("ListByNode() returned %d entries, want 2 (different versions must coexist)", len(entries))
	}
}

func TestNodeEngineInventoryRepository_Get_NotFound(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	_, err := inventory.Get(context.Background(), node.ID, ProfileEngineLlamaCPP, "no-such-version")
	if err != ErrNodeEngineInventoryNotFound {
		t.Errorf("Get() error = %v, want ErrNodeEngineInventoryNotFound", err)
	}
}

func TestNodeEngineInventoryRepository_List(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)
	ctx := context.Background()

	nodeA := createTestNode(t, nodes, fmt.Sprintf("node-a-%s", t.Name()))
	nodeB := createTestNode(t, nodes, fmt.Sprintf("node-b-%s", t.Name()))
	transferA := createTestEngineTransfer(t, transfers, nodeA.ID, nil)
	transferB := createTestEngineTransfer(t, transfers, nodeB.ID, nil)

	if _, err := inventory.Upsert(ctx, nodeA.ID, ProfileEngineLlamaCPP, "b4523", InventoryStatusPresent, "/path/b4523", 1024, transferA.ID); err != nil {
		t.Fatalf("Upsert(nodeA) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, nodeA.ID, ProfileEngineLlamaCPP, "b4523")
	})
	if _, err := inventory.Upsert(ctx, nodeB.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, "/path/b4610", 2048, transferB.ID); err != nil {
		t.Fatalf("Upsert(nodeB) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, nodeB.ID, ProfileEngineLlamaCPP, "b4610")
	})

	got, err := inventory.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, inv := range got {
		if inv.NodeID == nodeA.ID && inv.Version == "b4523" {
			foundA = true
		}
		if inv.NodeID == nodeB.ID && inv.Version == "b4610" {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d entries, missing one or both of the two just created (across different nodes)", len(got))
	}
}

func TestNodeEngineInventoryRepository_ListByNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	transfers := NewEngineTransferRepository(pool)
	inventory := NewNodeEngineInventoryRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	transferA := createTestEngineTransfer(t, transfers, node.ID, nil)
	transferB := createTestEngineTransfer(t, transfers, node.ID, nil)

	if _, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4523", InventoryStatusPresent, "/path/b4523", 1024, transferA.ID); err != nil {
		t.Fatalf("Upsert(b4523) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4523")
	})
	if _, err := inventory.Upsert(ctx, node.ID, ProfileEngineLlamaCPP, "b4610", InventoryStatusPresent, "/path/b4610", 2048, transferB.ID); err != nil {
		t.Fatalf("Upsert(b4610) error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_engine_inventory WHERE node_id = $1 AND engine_type = $2 AND version = $3`, node.ID, ProfileEngineLlamaCPP, "b4610")
	})

	got, err := inventory.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListByNode() returned %d entries, want 2", len(got))
	}
}
