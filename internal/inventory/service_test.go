// SPDX-License-Identifier: AGPL-3.0-or-later

package inventory

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// fakeInventoryStore implements inventoryStore for tests without a real
// Postgres - same pattern as internal/transfers' fakeTransferStore.
type fakeInventoryStore struct {
	listResult []*db.NodeModelInventory
	listErr    error

	listByNodeResult []*db.NodeModelInventory
	listByNodeErr    error

	getResult *db.NodeModelInventory
	getErr    error
}

func (f *fakeInventoryStore) List(_ context.Context) ([]*db.NodeModelInventory, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeInventoryStore) ListByNode(_ context.Context, _ string) ([]*db.NodeModelInventory, error) {
	if f.listByNodeErr != nil {
		return nil, f.listByNodeErr
	}
	return f.listByNodeResult, nil
}

func (f *fakeInventoryStore) Get(_ context.Context, _, _, _ string, _ db.ModelFormat) (*db.NodeModelInventory, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func entry(nodeID, modelRef, quantization string, format db.ModelFormat, sizeBytes int64) *db.NodeModelInventory {
	return &db.NodeModelInventory{
		NodeID: nodeID, ModelRef: modelRef, Quantization: quantization, Format: format,
		Status: db.InventoryStatusPresent, SizeBytes: sizeBytes, PlacedAt: time.Now(),
	}
}

func TestService_ListGrouped(t *testing.T) {
	store := &fakeInventoryStore{listResult: []*db.NodeModelInventory{
		entry("node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096),
		entry("node-2", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096),
		entry("node-1", "meta-llama/Llama-3-8B", "Q8_0", db.ModelFormatGGUF, 8192),
		entry("node-1", "org/other-model", "FP16", db.ModelFormatSafetensors, 16384),
	}}
	svc := NewService(store)

	groups, err := svc.ListGrouped(context.Background())
	if err != nil {
		t.Fatalf("ListGrouped() error: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("len(groups) = %d, want 3", len(groups))
	}

	first := groups[0]
	if first.ModelRef != "meta-llama/Llama-3-8B" || first.Quantization != "Q4_K_M" || first.Format != db.ModelFormatGGUF {
		t.Errorf("groups[0] = %+v, want the Q4_K_M group", first)
	}
	if len(first.Entries) != 2 {
		t.Errorf("len(groups[0].Entries) = %d, want 2 (one per node)", len(first.Entries))
	}

	second := groups[1]
	if second.Quantization != "Q8_0" || len(second.Entries) != 1 {
		t.Errorf("groups[1] = %+v, want the single-entry Q8_0 group", second)
	}

	third := groups[2]
	if third.ModelRef != "org/other-model" || third.Format != db.ModelFormatSafetensors {
		t.Errorf("groups[2] = %+v, want the other-model group", third)
	}
}

func TestService_ListGrouped_Error(t *testing.T) {
	wantErr := errors.New("boom")
	svc := NewService(&fakeInventoryStore{listErr: wantErr})

	_, err := svc.ListGrouped(context.Background())
	if !errors.Is(err, wantErr) {
		t.Errorf("ListGrouped() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestService_ListGroupedSimple(t *testing.T) {
	store := &fakeInventoryStore{listResult: []*db.NodeModelInventory{
		entry("node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096),
		entry("node-2", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096),
		entry("node-1", "meta-llama/Llama-3-8B", "Q8_0", db.ModelFormatGGUF, 8192),
		entry("node-1", "org/other-model", "FP16", db.ModelFormatSafetensors, 16384),
	}}
	svc := NewService(store)

	rows, err := svc.ListGroupedSimple(context.Background())
	if err != nil {
		t.Fatalf("ListGroupedSimple() error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	llama := rows[0]
	if llama.ModelRef != "meta-llama/Llama-3-8B" {
		t.Fatalf("rows[0].ModelRef = %q, want meta-llama/Llama-3-8B", llama.ModelRef)
	}
	if !reflect.DeepEqual(llama.Quantizations, []string{"Q4_K_M", "Q8_0"}) {
		t.Errorf("rows[0].Quantizations = %v, want [Q4_K_M Q8_0]", llama.Quantizations)
	}
	// 4096 (node-1) + 4096 (node-2) + 8192 (node-1, Q8_0) = 16384.
	if llama.TotalSizeBytes != 16384 {
		t.Errorf("rows[0].TotalSizeBytes = %d, want 16384", llama.TotalSizeBytes)
	}

	other := rows[1]
	if other.ModelRef != "org/other-model" || other.TotalSizeBytes != 16384 {
		t.Errorf("rows[1] = %+v, want the other-model row summing to 16384", other)
	}
}

func TestService_ListByNode(t *testing.T) {
	want := []*db.NodeModelInventory{entry("node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096)}
	svc := NewService(&fakeInventoryStore{listByNodeResult: want})

	got, err := svc.ListByNode(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListByNode() = %+v, want %+v", got, want)
	}
}

func TestService_ListByNode_Error(t *testing.T) {
	wantErr := errors.New("boom")
	svc := NewService(&fakeInventoryStore{listByNodeErr: wantErr})

	_, err := svc.ListByNode(context.Background(), "node-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ListByNode() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestService_Get(t *testing.T) {
	want := entry("node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096)
	svc := NewService(&fakeInventoryStore{getResult: want})

	got, err := svc.Get(context.Background(), "node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got != want {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestService_Get_NotFound(t *testing.T) {
	svc := NewService(&fakeInventoryStore{getErr: db.ErrNodeModelInventoryNotFound})

	_, err := svc.Get(context.Background(), "node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF)
	if !errors.Is(err, db.ErrNodeModelInventoryNotFound) {
		t.Errorf("Get() error = %v, want db.ErrNodeModelInventoryNotFound", err)
	}
}
