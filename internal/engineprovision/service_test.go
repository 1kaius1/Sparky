// SPDX-License-Identifier: AGPL-3.0-or-later

package engineprovision

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

// fakeEngineTransferStore implements engineTransferStore for tests without
// a real Postgres - same pattern as internal/transfers' fakeTransferStore.
type fakeEngineTransferStore struct {
	createErr error
	nextID    string
	created   []*db.EngineTransfer

	findByIDResult *db.EngineTransfer
	findByIDErr    error

	progressCalls []progressCall
	statusCalls   []statusCall

	listResult []*db.EngineTransfer
	listErr    error
}

type progressCall struct {
	id                           string
	bytesTransferred, bytesTotal int64
}

type statusCall struct {
	id     string
	status db.EngineTransferStatus
	errMsg *string
}

func (f *fakeEngineTransferStore) Create(_ context.Context, destNodeID string, engineType db.ProfileEngineType, version string, requestedBy *string) (*db.EngineTransfer, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "engine-transfer-1"
	}
	t := &db.EngineTransfer{
		ID: id, DestNodeID: destNodeID, EngineType: engineType, Version: version,
		Status: db.EngineTransferStatusQueued, RequestedBy: requestedBy,
	}
	f.created = append(f.created, t)
	return t, nil
}

func (f *fakeEngineTransferStore) FindByID(_ context.Context, id string) (*db.EngineTransfer, error) {
	if f.findByIDErr != nil {
		return nil, f.findByIDErr
	}
	if f.findByIDResult != nil {
		return f.findByIDResult, nil
	}
	return nil, db.ErrEngineTransferNotFound
}

func (f *fakeEngineTransferStore) UpdateProgress(_ context.Context, id string, bytesTransferred, bytesTotal int64) error {
	f.progressCalls = append(f.progressCalls, progressCall{id, bytesTransferred, bytesTotal})
	return nil
}

func (f *fakeEngineTransferStore) SetStatus(_ context.Context, id string, status db.EngineTransferStatus, errorMessage *string) error {
	f.statusCalls = append(f.statusCalls, statusCall{id, status, errorMessage})
	return nil
}

func (f *fakeEngineTransferStore) List(_ context.Context) ([]*db.EngineTransfer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

// fakeEngineInventoryStore implements engineInventoryStore for tests.
type fakeEngineInventoryStore struct {
	upsertErr  error
	calls      []upsertCall
	listResult []*db.NodeEngineInventory
	listErr    error
}

type upsertCall struct {
	nodeID      string
	engineType  db.ProfileEngineType
	version     string
	status      db.InventoryStatus
	installPath string
	sizeBytes   int64
	placedVia   string
}

func (f *fakeEngineInventoryStore) Upsert(_ context.Context, nodeID string, engineType db.ProfileEngineType, version string, status db.InventoryStatus, installPath string, sizeBytes int64, placedVia string) (*db.NodeEngineInventory, error) {
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	f.calls = append(f.calls, upsertCall{nodeID, engineType, version, status, installPath, sizeBytes, placedVia})
	return &db.NodeEngineInventory{NodeID: nodeID, EngineType: engineType, Version: version, Status: status, InstallPath: installPath, SizeBytes: sizeBytes, PlacedVia: placedVia}, nil
}

func (f *fakeEngineInventoryStore) List(context.Context) ([]*db.NodeEngineInventory, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

// fakeDispatcher implements dispatcher for tests without a real
// coder/websocket connection - same pattern as internal/transfers'
// fakeDispatcher.
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

// fakeAuditRecorder implements auditRecorder for tests.
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

func validParams() ProvisionEngineParams {
	return ProvisionEngineParams{DestNodeID: "node-1", EngineType: db.ProfileEngineLlamaCPP, Version: "b4610"}
}

func TestService_ProvisionEngine_PermittedByAdmin(t *testing.T) {
	transferStore := &fakeEngineTransferStore{nextID: "engine-transfer-1"}
	inventory := &fakeEngineInventoryStore{}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, inventory, dispatch, audit, testLogger())
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	got, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if err != nil {
		t.Fatalf("ProvisionEngine() error: %v", err)
	}
	if got.ID != "engine-transfer-1" {
		t.Errorf("ID = %q, want %q", got.ID, "engine-transfer-1")
	}
	if len(transferStore.created) != 1 {
		t.Fatalf("transferStore.Create called %d times, want 1", len(transferStore.created))
	}
	created := transferStore.created[0]
	if created.EngineType != db.ProfileEngineLlamaCPP || created.Version != "b4610" {
		t.Errorf("created = %+v, want EngineType=llamacpp Version=b4610", created)
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
	if dispatch.sent[0].Type != agentproto.TypeStartEngineTransfer {
		t.Errorf("dispatch.Send envelope type = %q, want %q", dispatch.sent[0].Type, agentproto.TypeStartEngineTransfer)
	}
	var payload agentproto.StartEngineTransfer
	if err := dispatch.sent[0].DecodePayload(&payload); err != nil {
		t.Fatalf("decode start_engine_transfer payload: %v", err)
	}
	if payload.TransferID != "engine-transfer-1" || payload.EngineType != "llamacpp" || payload.Version != "b4610" {
		t.Errorf("start_engine_transfer payload = %+v, want TransferID=engine-transfer-1 EngineType=llamacpp Version=b4610", payload)
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	if audit.calls[0].action != "initiated_engine_transfer" || audit.calls[0].objectType != "engine_transfer" || audit.calls[0].objectID != "engine-transfer-1" {
		t.Errorf("audit call = %+v, want action=initiated_engine_transfer objectType=engine_transfer objectID=engine-transfer-1", audit.calls[0])
	}
}

func TestService_ProvisionEngine_PermittedBySuperAdmin_NilRequestedBy(t *testing.T) {
	transferStore := &fakeEngineTransferStore{nextID: "engine-transfer-1"}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if err != nil {
		t.Fatalf("ProvisionEngine() error: %v", err)
	}
	if transferStore.created[0].RequestedBy != nil {
		t.Errorf("RequestedBy = %v, want nil (SuperAdmin is not a Users row)", *transferStore.created[0].RequestedBy)
	}
}

func TestService_ProvisionEngine_NotPermitted(t *testing.T) {
	// Unlike Model transfers' manage_model_store, this is Admin/SuperAdmin
	// only with no PowerDev-override path - see PLANNING.md's 2026-08-15
	// Decisions Log entry.
	for _, tier := range []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev} {
		t.Run(string(tier), func(t *testing.T) {
			transferStore := &fakeEngineTransferStore{}
			dispatch := &fakeDispatcher{connected: true}
			audit := &fakeAuditRecorder{}
			svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, audit, testLogger())
			actor := rbac.Actor{Tier: tier, UserID: "user-1"}

			_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
			if !errors.Is(err, rbac.ErrNotPermitted) {
				t.Errorf("ProvisionEngine() error = %v, want rbac.ErrNotPermitted", err)
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

func TestService_ProvisionEngine_InvalidParamsNotPersistedOrDispatched(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	dispatch := &fakeDispatcher{connected: true}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	params := validParams()
	params.Version = ""

	_, err := svc.ProvisionEngine(context.Background(), actor, params)
	if !errors.Is(err, ErrInvalidProvisionRequest) {
		t.Errorf("ProvisionEngine() error = %v, want ErrInvalidProvisionRequest", err)
	}
	if len(transferStore.created) != 0 {
		t.Error("transferStore.Create was called despite invalid params")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite invalid params")
	}
}

func TestService_ProvisionEngine_DestNodeOffline(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	dispatch := &fakeDispatcher{connected: false}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if !errors.Is(err, ErrDestNodeOffline) {
		t.Errorf("ProvisionEngine() error = %v, want ErrDestNodeOffline", err)
	}
	if len(transferStore.created) != 0 {
		t.Error("transferStore.Create was called despite an offline destination node - a transfer that will never be picked up should never be queued")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite an offline destination node")
	}
}

func TestService_ProvisionEngine_CreateFails(t *testing.T) {
	transferStore := &fakeEngineTransferStore{createErr: errors.New("database unreachable")}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("ProvisionEngine() succeeded despite a Create failure")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite a Create failure")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Create failure")
	}
}

func TestService_ProvisionEngine_DispatchFails(t *testing.T) {
	transferStore := &fakeEngineTransferStore{nextID: "engine-transfer-1"}
	dispatch := &fakeDispatcher{connected: true, sendErr: errors.New("connection reset")}
	audit := &fakeAuditRecorder{}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("ProvisionEngine() succeeded despite a Send failure")
	}
	if len(transferStore.created) != 1 {
		t.Error("transferStore.Create was not called, want the transfer to have been persisted before dispatch was attempted")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Send failure")
	}
}

func TestService_ProvisionEngine_AuditFailurePropagates(t *testing.T) {
	transferStore := &fakeEngineTransferStore{nextID: "engine-transfer-1"}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, dispatch, audit, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.ProvisionEngine(context.Background(), actor, validParams())
	if err == nil {
		t.Fatal("ProvisionEngine() succeeded despite an audit Record failure")
	}
	if len(dispatch.sent) != 1 {
		t.Error("dispatch.Send was not called, want start_engine_transfer dispatched before the audit write was attempted")
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

func TestService_HandleEngineTransferProgress_UpdatesProgressAndStatus(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	inventory := &fakeEngineInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeEngineTransferProgress, agentproto.EngineTransferProgress{
		TransferID: "engine-transfer-1", BytesTransferred: 1024, BytesTotal: 4096, Status: string(db.EngineTransferStatusTransferring),
	})
	svc.HandleEngineTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 1 {
		t.Fatalf("UpdateProgress called %d times, want 1", len(transferStore.progressCalls))
	}
	got := transferStore.progressCalls[0]
	if got.id != "engine-transfer-1" || got.bytesTransferred != 1024 || got.bytesTotal != 4096 {
		t.Errorf("UpdateProgress call = %+v, want id=engine-transfer-1 bytesTransferred=1024 bytesTotal=4096", got)
	}

	if len(transferStore.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(transferStore.statusCalls))
	}
	if transferStore.statusCalls[0].status != db.EngineTransferStatusTransferring {
		t.Errorf("SetStatus status = %q, want %q", transferStore.statusCalls[0].status, db.EngineTransferStatusTransferring)
	}
	if transferStore.statusCalls[0].errMsg != nil {
		t.Errorf("SetStatus errMsg = %v, want nil for a non-error progress update", *transferStore.statusCalls[0].errMsg)
	}

	if len(inventory.calls) != 0 {
		t.Error("inventory.Upsert was called for a non-completed transfer")
	}
}

func TestService_HandleEngineTransferProgress_Completed_UpsertsInventory(t *testing.T) {
	transferStore := &fakeEngineTransferStore{
		findByIDResult: &db.EngineTransfer{ID: "engine-transfer-1", DestNodeID: "node-1", EngineType: db.ProfileEngineLlamaCPP, Version: "b4610"},
	}
	inventory := &fakeEngineInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeEngineTransferProgress, agentproto.EngineTransferProgress{
		TransferID: "engine-transfer-1", BytesTransferred: 4096, BytesTotal: 4096,
		Status: string(db.EngineTransferStatusCompleted), InstallPath: "/opt/sparky/serviceloop/engines/llamacpp/b4610",
		InstalledSizeBytes: 9000,
	})
	svc.HandleEngineTransferProgress("node-1", env)

	if len(inventory.calls) != 1 {
		t.Fatalf("inventory.Upsert called %d times, want 1", len(inventory.calls))
	}
	got := inventory.calls[0]
	if got.nodeID != "node-1" || got.engineType != db.ProfileEngineLlamaCPP || got.version != "b4610" ||
		got.status != db.InventoryStatusPresent || got.installPath != "/opt/sparky/serviceloop/engines/llamacpp/b4610" ||
		got.sizeBytes != 9000 || got.placedVia != "engine-transfer-1" {
		t.Errorf("inventory.Upsert call = %+v, want nodeID=node-1 engineType=llamacpp version=b4610 status=present installPath=.../b4610 sizeBytes=9000 (installed, not downloaded) placedVia=engine-transfer-1", got)
	}
}

func TestService_HandleEngineTransferProgress_Failed_SetsErrorMessage_NoInventoryUpsert(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	inventory := &fakeEngineInventoryStore{}
	svc := NewService(transferStore, inventory, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeEngineTransferProgress, agentproto.EngineTransferProgress{
		TransferID: "engine-transfer-1", Status: string(db.EngineTransferStatusFailed), ErrorMessage: "checksum mismatch",
	})
	svc.HandleEngineTransferProgress("node-1", env)

	if len(transferStore.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(transferStore.statusCalls))
	}
	got := transferStore.statusCalls[0]
	if got.errMsg == nil || *got.errMsg != "checksum mismatch" {
		t.Errorf("SetStatus errMsg = %v, want %q", got.errMsg, "checksum mismatch")
	}
	if len(inventory.calls) != 0 {
		t.Error("inventory.Upsert was called for a failed transfer")
	}
}

func TestService_HandleEngineTransferProgress_IgnoresOtherMessageTypes(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newEnvelope(t, agentproto.TypeHeartbeat, agentproto.Heartbeat{})
	svc.HandleEngineTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 0 || len(transferStore.statusCalls) != 0 {
		t.Error("HandleEngineTransferProgress acted on a non-engine_transfer_progress message type")
	}
}

func TestService_HandleEngineTransferProgress_MalformedPayload_Ignored(t *testing.T) {
	transferStore := &fakeEngineTransferStore{}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := agentproto.Envelope{Type: agentproto.TypeEngineTransferProgress, Payload: []byte(`{"transfer_id": 123}`)}
	svc.HandleEngineTransferProgress("node-1", env)

	if len(transferStore.progressCalls) != 0 || len(transferStore.statusCalls) != 0 {
		t.Error("HandleEngineTransferProgress acted on a malformed payload")
	}
}

func TestService_ListEngineTransfers(t *testing.T) {
	want := []*db.EngineTransfer{
		{ID: "engine-transfer-1", Version: "b4523"},
		{ID: "engine-transfer-2", Version: "b4610"},
	}
	transferStore := &fakeEngineTransferStore{listResult: want}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	got, err := svc.ListEngineTransfers(context.Background())
	if err != nil {
		t.Fatalf("ListEngineTransfers() error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "engine-transfer-1" || got[1].ID != "engine-transfer-2" {
		t.Errorf("ListEngineTransfers() = %+v, want %+v", got, want)
	}
}

func TestService_ListEngineTransfers_StoreError(t *testing.T) {
	transferStore := &fakeEngineTransferStore{listErr: errors.New("database unreachable")}
	svc := NewService(transferStore, &fakeEngineInventoryStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	if _, err := svc.ListEngineTransfers(context.Background()); err == nil {
		t.Fatal("ListEngineTransfers() succeeded despite a store error")
	}
}

func TestService_ListNodeEngineInventory(t *testing.T) {
	want := []*db.NodeEngineInventory{
		{NodeID: "node-1", EngineType: db.ProfileEngineLlamaCPP, Version: "b4523"},
		{NodeID: "node-2", EngineType: db.ProfileEngineLlamaCPP, Version: "b4610"},
	}
	inventory := &fakeEngineInventoryStore{listResult: want}
	svc := NewService(&fakeEngineTransferStore{}, inventory, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	got, err := svc.ListNodeEngineInventory(context.Background())
	if err != nil {
		t.Fatalf("ListNodeEngineInventory() error: %v", err)
	}
	if len(got) != 2 || got[0].Version != "b4523" || got[1].Version != "b4610" {
		t.Errorf("ListNodeEngineInventory() = %+v, want %+v", got, want)
	}
}

func TestService_ListNodeEngineInventory_StoreError(t *testing.T) {
	inventory := &fakeEngineInventoryStore{listErr: errors.New("database unreachable")}
	svc := NewService(&fakeEngineTransferStore{}, inventory, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	if _, err := svc.ListNodeEngineInventory(context.Background()); err == nil {
		t.Fatal("ListNodeEngineInventory() succeeded despite a store error")
	}
}
