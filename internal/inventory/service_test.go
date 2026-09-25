// SPDX-License-Identifier: AGPL-3.0-or-later

package inventory

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
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

	setStatusCalls []db.InventoryStatus
	setStatusErr   error
}

func (f *fakeInventoryStore) SetStatus(_ context.Context, _, _, _ string, _ db.ModelFormat, status db.InventoryStatus) error {
	f.setStatusCalls = append(f.setStatusCalls, status)
	return f.setStatusErr
}

type fakeProfileStore struct {
	profiles []*db.Profile
	err      error
}

func (f *fakeProfileStore) List(context.Context) ([]*db.Profile, error) { return f.profiles, f.err }

type fakeOverrideStore struct{ granted bool }

func (f *fakeOverrideStore) Get(context.Context, string, db.Capability) (*db.PermissionOverride, error) {
	if f.granted {
		return &db.PermissionOverride{}, nil
	}
	return nil, db.ErrPermissionOverrideNotFound
}

type fakeDispatcher struct {
	connected bool
	sent      []agentproto.Envelope
	err       error
}

func (f *fakeDispatcher) Connected(string) bool { return f.connected }
func (f *fakeDispatcher) Send(_ context.Context, _ string, env agentproto.Envelope) error {
	f.sent = append(f.sent, env)
	return f.err
}

type fakeAudit struct {
	actions []string
	details []map[string]any
	objIDs  []string
}

func (f *fakeAudit) Record(_ context.Context, _ *string, _ bool, action, _, objectID string, detail map[string]any) error {
	f.actions = append(f.actions, action)
	f.objIDs = append(f.objIDs, objectID)
	f.details = append(f.details, detail)
	return nil
}

func newTestService(store *fakeInventoryStore) *Service {
	return NewService(store, &fakeProfileStore{}, &fakeOverrideStore{}, &fakeDispatcher{connected: true}, &fakeAudit{}, log.New(&bytes.Buffer{}, "", 0))
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
	svc := newTestService(store)

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
	svc := newTestService(&fakeInventoryStore{listErr: wantErr})

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
	svc := newTestService(store)

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
	svc := newTestService(&fakeInventoryStore{listByNodeResult: want})

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
	svc := newTestService(&fakeInventoryStore{listByNodeErr: wantErr})

	_, err := svc.ListByNode(context.Background(), "node-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ListByNode() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestService_Get(t *testing.T) {
	want := entry("node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF, 4096)
	svc := newTestService(&fakeInventoryStore{getResult: want})

	got, err := svc.Get(context.Background(), "node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got != want {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestService_Get_NotFound(t *testing.T) {
	svc := newTestService(&fakeInventoryStore{getErr: db.ErrNodeModelInventoryNotFound})

	_, err := svc.Get(context.Background(), "node-1", "meta-llama/Llama-3-8B", "Q4_K_M", db.ModelFormatGGUF)
	if !errors.Is(err, db.ErrNodeModelInventoryNotFound) {
		t.Errorf("Get() error = %v, want db.ErrNodeModelInventoryNotFound", err)
	}
}

func TestService_ListGrouped_ExcludesRemoved(t *testing.T) {
	removed := entry("node-1", "org/gone", "FP16", db.ModelFormatSafetensors, 1)
	removed.Status = db.InventoryStatusRemoved
	svc := newTestService(&fakeInventoryStore{listResult: []*db.NodeModelInventory{removed, entry("node-1", "org/kept", "FP16", db.ModelFormatSafetensors, 1)}})

	groups, err := svc.ListGrouped(context.Background())
	if err != nil {
		t.Fatalf("ListGrouped() error: %v", err)
	}
	if len(groups) != 1 || groups[0].ModelRef != "org/kept" {
		t.Errorf("groups = %+v, want only org/kept", groups)
	}
}

type deleteFixture struct {
	svc      *Service
	store    *fakeInventoryStore
	profiles *fakeProfileStore
	dispatch *fakeDispatcher
	audit    *fakeAudit
}

func newDeleteFixture() *deleteFixture {
	f := &deleteFixture{
		store:    &fakeInventoryStore{getResult: entry("node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF, 10)},
		profiles: &fakeProfileStore{},
		dispatch: &fakeDispatcher{connected: true},
		audit:    &fakeAudit{},
	}
	f.svc = NewService(f.store, f.profiles, &fakeOverrideStore{}, f.dispatch, f.audit, log.New(&bytes.Buffer{}, "", 0))
	return f
}

var adminActor = rbac.Actor{UserID: "admin-1", Tier: db.TierAdmin}

func TestService_Delete_DispatchesAndAudits(t *testing.T) {
	f := newDeleteFixture()
	if err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if len(f.dispatch.sent) != 1 || f.dispatch.sent[0].Type != agentproto.TypeDeleteModel {
		t.Fatalf("sent = %+v, want one delete_model", f.dispatch.sent)
	}
	var got agentproto.DeleteModel
	if err := f.dispatch.sent[0].DecodePayload(&got); err != nil {
		t.Fatal(err)
	}
	if got != (agentproto.DeleteModel{ModelRef: "org/m", Quantization: "Q4_K_M", Format: "gguf"}) {
		t.Errorf("payload = %+v", got)
	}
	if len(f.audit.actions) != 1 || f.audit.actions[0] != "deleted_model_copy" || f.audit.objIDs[0] != "node-1" {
		t.Errorf("audit = %+v %+v", f.audit.actions, f.audit.objIDs)
	}
	if len(f.store.setStatusCalls) != 0 {
		t.Error("Delete must not mark removed before the agent confirms")
	}
}

func TestService_Delete_NotPermitted(t *testing.T) {
	f := newDeleteFixture()
	err := f.svc.Delete(context.Background(), rbac.Actor{UserID: "u", Tier: db.TierDeveloper}, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF)
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("error = %v, want ErrNotPermitted", err)
	}
	if len(f.dispatch.sent) != 0 {
		t.Error("nothing should be dispatched")
	}
}

func TestService_Delete_PowerDevNeedsOverride(t *testing.T) {
	f := newDeleteFixture()
	powerDev := rbac.Actor{UserID: "u", Tier: db.TierPowerDev}
	if err := f.svc.Delete(context.Background(), powerDev, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("without override error = %v, want ErrNotPermitted", err)
	}
	f.svc.overrides = &fakeOverrideStore{granted: true}
	if err := f.svc.Delete(context.Background(), powerDev, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); err != nil {
		t.Errorf("with override error = %v", err)
	}
}

func TestService_Delete_NotFoundAndAlreadyRemoved(t *testing.T) {
	f := newDeleteFixture()
	f.store.getErr = db.ErrNodeModelInventoryNotFound
	if err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); !errors.Is(err, db.ErrNodeModelInventoryNotFound) {
		t.Errorf("error = %v", err)
	}
	f.store.getErr = nil
	f.store.getResult.Status = db.InventoryStatusRemoved
	if err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); !errors.Is(err, db.ErrNodeModelInventoryNotFound) {
		t.Errorf("already-removed error = %v", err)
	}
}

func TestService_Delete_BlockedByProfile(t *testing.T) {
	f := newDeleteFixture()
	node, quant := "node-1", "Q4_K_M"
	f.profiles.profiles = []*db.Profile{
		{Name: "other-node", TargetNodeID: strPtr("node-2"), ModelRef: "org/m", Quantization: &quant, Format: db.ModelFormatGGUF},
		{Name: "uses-it", TargetNodeID: &node, ModelRef: "org/m", Quantization: &quant, Format: db.ModelFormatGGUF},
	}
	err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF)
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("error = %v, want ErrModelInUse", err)
	}
	if len(f.dispatch.sent) != 0 {
		t.Error("nothing should be dispatched")
	}
}

func TestService_Delete_OtherQuantizationProfileDoesNotBlock(t *testing.T) {
	f := newDeleteFixture()
	node, quant := "node-1", "Q8_0"
	f.profiles.profiles = []*db.Profile{{Name: "q8", TargetNodeID: &node, ModelRef: "org/m", Quantization: &quant, Format: db.ModelFormatGGUF}}
	if err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); err != nil {
		t.Errorf("error = %v", err)
	}
}

func TestService_Delete_NodeOffline(t *testing.T) {
	f := newDeleteFixture()
	f.dispatch.connected = false
	if err := f.svc.Delete(context.Background(), adminActor, "node-1", "org/m", "Q4_K_M", db.ModelFormatGGUF); !errors.Is(err, ErrNodeOffline) {
		t.Errorf("error = %v, want ErrNodeOffline", err)
	}
}

func strPtr(s string) *string { return &s }

func deleteResultEnv(t *testing.T, r agentproto.DeleteModelResult) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeDeleteModelResult, "", r)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestService_HandleDeleteModelResult_SuccessMarksRemoved(t *testing.T) {
	f := newDeleteFixture()
	f.svc.HandleDeleteModelResult("node-1", deleteResultEnv(t, agentproto.DeleteModelResult{ModelRef: "org/m", Quantization: "Q4_K_M", Format: "gguf", Success: true}))
	if len(f.store.setStatusCalls) != 1 || f.store.setStatusCalls[0] != db.InventoryStatusRemoved {
		t.Errorf("setStatusCalls = %v, want [removed]", f.store.setStatusCalls)
	}
}

func TestService_HandleDeleteModelResult_FailureLeavesPresent(t *testing.T) {
	f := newDeleteFixture()
	f.svc.HandleDeleteModelResult("node-1", deleteResultEnv(t, agentproto.DeleteModelResult{ModelRef: "org/m", Format: "gguf", Success: false, Reason: "busy"}))
	if len(f.store.setStatusCalls) != 0 {
		t.Errorf("setStatusCalls = %v, want none", f.store.setStatusCalls)
	}
}
