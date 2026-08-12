// SPDX-License-Identifier: AGPL-3.0-or-later

package transfers

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// fakeTransferStore implements transferStore for tests without a real
// Postgres - same pattern as internal/nodes' fakeNodeStore.
type fakeTransferStore struct {
	createErr error
	nextID    string
	created   []*db.ModelTransfer

	findByIDResult *db.ModelTransfer
	findByIDErr    error

	progressCalls []progressCall
	statusCalls   []statusCall
}

type progressCall struct {
	id                           string
	bytesTransferred, bytesTotal int64
}

type statusCall struct {
	id     string
	status db.TransferStatus
	errMsg *string
}

func (f *fakeTransferStore) Create(_ context.Context, destNodeID, modelRef string, sourceType db.TransferSourceType, sourceNodeID *string, requestedBy *string) (*db.ModelTransfer, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "transfer-1"
	}
	t := &db.ModelTransfer{
		ID: id, DestNodeID: destNodeID, ModelRef: modelRef, SourceType: sourceType,
		SourceNodeID: sourceNodeID, Status: db.TransferStatusQueued, RequestedBy: requestedBy,
	}
	f.created = append(f.created, t)
	return t, nil
}

func (f *fakeTransferStore) FindByID(_ context.Context, id string) (*db.ModelTransfer, error) {
	if f.findByIDErr != nil {
		return nil, f.findByIDErr
	}
	if f.findByIDResult != nil {
		return f.findByIDResult, nil
	}
	return nil, db.ErrModelTransferNotFound
}

func (f *fakeTransferStore) UpdateProgress(_ context.Context, id string, bytesTransferred, bytesTotal int64) error {
	f.progressCalls = append(f.progressCalls, progressCall{id, bytesTransferred, bytesTotal})
	return nil
}

func (f *fakeTransferStore) SetStatus(_ context.Context, id string, status db.TransferStatus, errorMessage *string) error {
	f.statusCalls = append(f.statusCalls, statusCall{id, status, errorMessage})
	return nil
}

// fakeInventoryStore implements inventoryStore for tests.
type fakeInventoryStore struct {
	upsertErr error
	calls     []upsertCall
}

type upsertCall struct {
	nodeID, modelRef string
	status           db.InventoryStatus
	sizeBytes        int64
	placedVia        string
}

func (f *fakeInventoryStore) Upsert(_ context.Context, nodeID, modelRef string, status db.InventoryStatus, sizeBytes int64, placedVia string) (*db.NodeModelInventory, error) {
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	f.calls = append(f.calls, upsertCall{nodeID, modelRef, status, sizeBytes, placedVia})
	return &db.NodeModelInventory{NodeID: nodeID, ModelRef: modelRef, Status: status, SizeBytes: sizeBytes, PlacedVia: placedVia}, nil
}

// fakeOverrideStore implements overrideStore for tests.
type fakeOverrideStore struct {
	granted bool
	getErr  error
	calls   int
}

func (f *fakeOverrideStore) Get(_ context.Context, userID string, capability db.Capability) (*db.PermissionOverride, error) {
	f.calls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.granted {
		return nil, db.ErrPermissionOverrideNotFound
	}
	return &db.PermissionOverride{UserID: userID, Capability: capability}, nil
}

// fakeDispatcher implements dispatcher for tests without a real
// coder/websocket connection.
type fakeDispatcher struct {
	connected bool
	sendErr   error
	sent      []agentproto.Envelope
	sentTo    []string
}

func (f *fakeDispatcher) Connected(string) bool { return f.connected }

func (f *fakeDispatcher) Send(_ context.Context, nodeID string, env agentproto.Envelope) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, env)
	f.sentTo = append(f.sentTo, nodeID)
	return nil
}

// fakeAuditRecorder implements auditRecorder for tests - same pattern as
// internal/nodes' fakeAuditRecorder.
type fakeAuditRecorder struct {
	recordErr error
	calls     []auditCall
}

type auditCall struct {
	actorID            *string
	isSuperAdminAction bool
	action             string
	objectType         string
	objectID           string
	detail             map[string]any
}

func (f *fakeAuditRecorder) Record(_ context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.calls = append(f.calls, auditCall{actorID, isSuperAdminAction, action, objectType, objectID, detail})
	return nil
}

func testLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

func validParams() InitiateTransferParams {
	return InitiateTransferParams{DestNodeID: "node-1", ModelRef: "meta-llama/Llama-3-8B"}
}

func TestService_InitiateTransfer_PermittedByAdmin(t *testing.T) {
	transferStore := &fakeTransferStore{nextID: "transfer-1"}
	inventory := &fakeInventoryStore{}
	overrides := &fakeOverrideStore{}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, inventory, overrides, dispatch, audit, testLogger())
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	got, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err != nil {
		t.Fatalf("InitiateTransfer() error: %v", err)
	}
	if got.ID != "transfer-1" {
		t.Errorf("ID = %q, want %q", got.ID, "transfer-1")
	}
	if len(transferStore.created) != 1 {
		t.Fatalf("transferStore.Create called %d times, want 1", len(transferStore.created))
	}
	created := transferStore.created[0]
	if created.SourceType != db.TransferSourceInternet {
		t.Errorf("SourceType = %q, want %q", created.SourceType, db.TransferSourceInternet)
	}
	if created.SourceNodeID != nil {
		t.Errorf("SourceNodeID = %v, want nil for an internet-sourced transfer", created.SourceNodeID)
	}
	if created.RequestedBy == nil || *created.RequestedBy != "admin-1" {
		t.Errorf("RequestedBy = %v, want %q", created.RequestedBy, "admin-1")
	}

	if len(dispatch.sent) != 1 {
		t.Fatalf("dispatch.Send called %d times, want 1", len(dispatch.sent))
	}
	if dispatch.sentTo[0] != "node-1" {
		t.Errorf("dispatch.Send nodeID = %q, want %q", dispatch.sentTo[0], "node-1")
	}
	if dispatch.sent[0].Type != agentproto.TypeStartTransfer {
		t.Errorf("dispatch.Send envelope type = %q, want %q", dispatch.sent[0].Type, agentproto.TypeStartTransfer)
	}
	var payload agentproto.StartTransfer
	if err := dispatch.sent[0].DecodePayload(&payload); err != nil {
		t.Fatalf("decode start_transfer payload: %v", err)
	}
	if payload.TransferID != "transfer-1" || payload.ModelRef != "meta-llama/Llama-3-8B" {
		t.Errorf("start_transfer payload = %+v, want TransferID=transfer-1 ModelRef=meta-llama/Llama-3-8B", payload)
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	if audit.calls[0].action != "initiated_transfer" || audit.calls[0].objectType != "model_transfer" || audit.calls[0].objectID != "transfer-1" {
		t.Errorf("audit call = %+v, want action=initiated_transfer objectType=model_transfer objectID=transfer-1", audit.calls[0])
	}

	// Admin has the capability implicitly - no reason to consult the
	// overrides table.
	if overrides.calls != 0 {
		t.Errorf("overrideStore.Get called %d times, want 0 for an Admin actor", overrides.calls)
	}
}

func TestService_InitiateTransfer_PermittedBySuperAdmin_NilRequestedBy(t *testing.T) {
	transferStore := &fakeTransferStore{nextID: "transfer-1"}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err != nil {
		t.Fatalf("InitiateTransfer() error: %v", err)
	}
	if transferStore.created[0].RequestedBy != nil {
		t.Errorf("RequestedBy = %v, want nil (SuperAdmin is not a Users row)", *transferStore.created[0].RequestedBy)
	}
}

func TestService_InitiateTransfer_PowerDevWithOverride(t *testing.T) {
	transferStore := &fakeTransferStore{nextID: "transfer-1"}
	overrides := &fakeOverrideStore{granted: true}
	svc := NewService(transferStore, &fakeInventoryStore{}, overrides, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err != nil {
		t.Fatalf("InitiateTransfer() error: %v", err)
	}
	if overrides.calls != 1 {
		t.Errorf("overrideStore.Get called %d times, want 1", overrides.calls)
	}
}

func TestService_InitiateTransfer_PowerDevWithoutOverride_NotPermitted(t *testing.T) {
	transferStore := &fakeTransferStore{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{granted: false}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("InitiateTransfer() error = %v, want rbac.ErrNotPermitted", err)
	}
	if len(transferStore.created) != 0 {
		t.Error("transferStore.Create was called despite the actor not being permitted")
	}
}

func TestService_InitiateTransfer_NotPermitted(t *testing.T) {
	for _, tier := range []db.Tier{db.TierReadOnly, db.TierDeveloper} {
		t.Run(string(tier), func(t *testing.T) {
			transferStore := &fakeTransferStore{}
			dispatch := &fakeDispatcher{connected: true}
			audit := &fakeAuditRecorder{}
			svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, audit, testLogger())
			actor := rbac.Actor{Tier: tier, UserID: "user-1"}

			_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
			if !errors.Is(err, rbac.ErrNotPermitted) {
				t.Errorf("InitiateTransfer() error = %v, want rbac.ErrNotPermitted", err)
			}
			if len(transferStore.created) != 0 {
				t.Error("transferStore.Create was called despite the actor not being permitted")
			}
			if len(dispatch.sent) != 0 {
				t.Error("dispatch.Send was called despite the actor not being permitted")
			}
			if len(audit.calls) != 0 {
				t.Error("audit.Record was called despite a refused initiation")
			}
		})
	}
}

func TestService_InitiateTransfer_InvalidParamsNotPersistedOrDispatched(t *testing.T) {
	transferStore := &fakeTransferStore{}
	dispatch := &fakeDispatcher{connected: true}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	params := validParams()
	params.ModelRef = ""

	_, err := svc.InitiateTransfer(context.Background(), actor, params)
	if !errors.Is(err, ErrInvalidTransfer) {
		t.Errorf("InitiateTransfer() error = %v, want ErrInvalidTransfer", err)
	}
	if len(transferStore.created) != 0 {
		t.Error("transferStore.Create was called despite invalid params")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite invalid params")
	}
}

func TestService_InitiateTransfer_DestNodeOffline(t *testing.T) {
	transferStore := &fakeTransferStore{}
	dispatch := &fakeDispatcher{connected: false}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if !errors.Is(err, ErrDestNodeOffline) {
		t.Errorf("InitiateTransfer() error = %v, want ErrDestNodeOffline", err)
	}
	if len(transferStore.created) != 0 {
		t.Error("transferStore.Create was called despite an offline destination node - a transfer that will never be picked up should never be queued")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite an offline destination node")
	}
}

func TestService_InitiateTransfer_CreateFails(t *testing.T) {
	transferStore := &fakeTransferStore{createErr: errors.New("database unreachable")}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("InitiateTransfer() succeeded despite a Create failure")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite a Create failure")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Create failure")
	}
}

func TestService_InitiateTransfer_DispatchFails(t *testing.T) {
	transferStore := &fakeTransferStore{nextID: "transfer-1"}
	dispatch := &fakeDispatcher{connected: true, sendErr: errors.New("connection reset")}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("InitiateTransfer() succeeded despite a Send failure")
	}
	// The transfer row was already created - not rolled back, same known
	// limitation as rbac.Service.ElevateTier - see PLANNING.md Known
	// Issues and Technical Debt.
	if len(transferStore.created) != 1 {
		t.Error("transferStore.Create was not called, want the transfer to have been persisted before dispatch was attempted")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Send failure")
	}
}

func TestService_InitiateTransfer_AuditFailurePropagates(t *testing.T) {
	transferStore := &fakeTransferStore{nextID: "transfer-1"}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.InitiateTransfer(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("InitiateTransfer() succeeded despite an audit Record failure")
	}
	if len(dispatch.sent) != 1 {
		t.Error("dispatch.Send was not called, want start_transfer dispatched before the audit write was attempted")
	}
}

func newEnvelope(t *testing.T, msgType agentproto.MessageType, payload any) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(msgType, "", payload)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

func TestService_HandleTransferProgress_UpdatesProgressAndStatus(t *testing.T) {
	transferStore := &fakeTransferStore{}
	inventory := &fakeInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeOverrideStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{
		TransferID: "transfer-1", BytesTransferred: 1024, BytesTotal: 4096, Status: string(db.TransferStatusTransferring),
	})
	svc.HandleTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 1 {
		t.Fatalf("UpdateProgress called %d times, want 1", len(transferStore.progressCalls))
	}
	got := transferStore.progressCalls[0]
	if got.id != "transfer-1" || got.bytesTransferred != 1024 || got.bytesTotal != 4096 {
		t.Errorf("UpdateProgress call = %+v, want id=transfer-1 bytesTransferred=1024 bytesTotal=4096", got)
	}

	if len(transferStore.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(transferStore.statusCalls))
	}
	if transferStore.statusCalls[0].status != db.TransferStatusTransferring {
		t.Errorf("SetStatus status = %q, want %q", transferStore.statusCalls[0].status, db.TransferStatusTransferring)
	}
	if transferStore.statusCalls[0].errMsg != nil {
		t.Errorf("SetStatus errMsg = %v, want nil for a non-error progress update", *transferStore.statusCalls[0].errMsg)
	}

	if len(inventory.calls) != 0 {
		t.Error("inventory.Upsert was called for a non-completed transfer")
	}
}

func TestService_HandleTransferProgress_Completed_UpsertsInventory(t *testing.T) {
	transferStore := &fakeTransferStore{
		findByIDResult: &db.ModelTransfer{ID: "transfer-1", DestNodeID: "node-1", ModelRef: "meta-llama/Llama-3-8B"},
	}
	inventory := &fakeInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeOverrideStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{
		TransferID: "transfer-1", BytesTransferred: 4096, BytesTotal: 4096, Status: string(db.TransferStatusCompleted),
	})
	svc.HandleTransferProgress("node-1", env)

	if len(inventory.calls) != 1 {
		t.Fatalf("inventory.Upsert called %d times, want 1", len(inventory.calls))
	}
	got := inventory.calls[0]
	if got.nodeID != "node-1" || got.modelRef != "meta-llama/Llama-3-8B" || got.status != db.InventoryStatusPresent || got.sizeBytes != 4096 || got.placedVia != "transfer-1" {
		t.Errorf("inventory.Upsert call = %+v, want nodeID=node-1 modelRef=meta-llama/Llama-3-8B status=present sizeBytes=4096 placedVia=transfer-1", got)
	}
}

func TestService_HandleTransferProgress_Failed_SetsErrorMessage_NoInventoryUpsert(t *testing.T) {
	transferStore := &fakeTransferStore{}
	inventory := &fakeInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeOverrideStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{
		TransferID: "transfer-1", Status: string(db.TransferStatusFailed), ErrorMessage: "connection reset",
	})
	svc.HandleTransferProgress("node-1", env)

	if len(transferStore.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(transferStore.statusCalls))
	}
	got := transferStore.statusCalls[0]
	if got.errMsg == nil || *got.errMsg != "connection reset" {
		t.Errorf("SetStatus errMsg = %v, want %q", got.errMsg, "connection reset")
	}
	if len(inventory.calls) != 0 {
		t.Error("inventory.Upsert was called for a failed transfer")
	}
}

func TestService_HandleTransferProgress_IgnoresOtherMessageTypes(t *testing.T) {
	transferStore := &fakeTransferStore{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeHeartbeat, agentproto.Heartbeat{})
	svc.HandleTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 0 || len(transferStore.statusCalls) != 0 {
		t.Error("HandleTransferProgress acted on a non-transfer_progress message type")
	}
}

func TestService_HandleTransferProgress_MalformedPayload_Ignored(t *testing.T) {
	transferStore := &fakeTransferStore{}
	svc := NewService(transferStore, &fakeInventoryStore{}, &fakeOverrideStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := agentproto.Envelope{Type: agentproto.TypeTransferProgress, Payload: []byte(`{"transfer_id": 123}`)}
	svc.HandleTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 0 || len(transferStore.statusCalls) != 0 {
		t.Error("HandleTransferProgress acted on a malformed payload")
	}
}
