// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// fakeNodeStore implements nodeStore for tests without a real Postgres -
// same pattern as internal/rbac's fakeUserStore.
type fakeNodeStore struct {
	createErr error
	nextID    string
	created   []*db.Node
	// bearerTokenHashes parallels created - the hash Create received for
	// the node at the same index. Kept separate from db.Node since
	// NodeRepository.Create deliberately never returns the hash - see
	// internal/db/nodes.go.
	bearerTokenHashes []string

	listResult []*db.Node
	listErr    error

	findResult *db.Node
	findErr    error

	defaultIfaceCalls []*string
}

func (f *fakeNodeStore) FindByID(_ context.Context, id string) (*db.Node, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.findResult != nil {
		return f.findResult, nil
	}
	return nil, db.ErrNodeNotFound
}

func (f *fakeNodeStore) SetDefaultTransferInterface(_ context.Context, _ string, name *string) error {
	f.defaultIfaceCalls = append(f.defaultIfaceCalls, name)
	return nil
}

type fakeInterfaceStore struct {
	replaced [][]db.NodeNetworkInterface
	listed   []*db.NodeNetworkInterface
}

func (f *fakeInterfaceStore) ReplaceForNode(_ context.Context, _ string, ifaces []db.NodeNetworkInterface) error {
	f.replaced = append(f.replaced, ifaces)
	return nil
}

func (f *fakeInterfaceStore) ListByNode(context.Context, string) ([]*db.NodeNetworkInterface, error) {
	return f.listed, nil
}

type fakeDispatcher struct {
	connected bool
	sent      []agentproto.Envelope
}

func (f *fakeDispatcher) Connected(string) bool { return f.connected }
func (f *fakeDispatcher) Send(_ context.Context, _ string, env agentproto.Envelope) error {
	f.sent = append(f.sent, env)
	return nil
}

func (f *fakeNodeStore) List(_ context.Context) ([]*db.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeNodeStore) Create(_ context.Context, name, hostname, ipAddress string, runtimeBackend db.RuntimeBackend, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string, bearerTokenHash string) (*db.Node, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "node-1"
	}
	n := &db.Node{
		ID:             id,
		Name:           name,
		Hostname:       hostname,
		IPAddress:      ipAddress,
		RuntimeBackend: runtimeBackend,
		GPUMemoryGB:    gpuMemoryGB,
		CPUMemoryGB:    cpuMemoryGB,
		AgentStatus:    db.AgentStatusOffline,
		RegisteredBy:   registeredBy,
	}
	f.created = append(f.created, n)
	f.bearerTokenHashes = append(f.bearerTokenHashes, bearerTokenHash)
	return n, nil
}

// fakeAuditRecorder implements auditRecorder for tests without a real
// Postgres - same pattern as internal/rbac's fakeAuditRecorder.
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

func TestService_RegisterNode_PermittedByAdmin(t *testing.T) {
	store := &fakeNodeStore{nextID: "node-1"}
	audit := &fakeAuditRecorder{}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	n, token, err := svc.RegisterNode(context.Background(), actor, validBareMetalParams())
	if err != nil {
		t.Fatalf("RegisterNode() error: %v", err)
	}
	if n.ID != "node-1" {
		t.Errorf("ID = %q, want %q", n.ID, "node-1")
	}
	if len(store.created) != 1 {
		t.Fatalf("nodeStore.Create called %d times, want 1", len(store.created))
	}
	if store.created[0].RegisteredBy == nil || *store.created[0].RegisteredBy != "admin-1" {
		t.Errorf("RegisteredBy = %v, want %q", store.created[0].RegisteredBy, "admin-1")
	}

	if token == "" {
		t.Error("bearer token is empty, want a generated plaintext token")
	}
	gotHash := store.bearerTokenHashes[0]
	if gotHash == "" || gotHash == token {
		t.Errorf("stored bearer_token_hash = %q, want a non-empty hash distinct from the plaintext token %q", gotHash, token)
	}
	if !auth.VerifyNodeToken(token, gotHash) {
		t.Error("auth.VerifyNodeToken(token, storedHash) = false, want true - the returned plaintext must verify against the persisted hash")
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	got := audit.calls[0]
	if got.action != "registered_node" || got.objectType != "node" || got.objectID != "node-1" {
		t.Errorf("audit call = %+v, want action=registered_node objectType=node objectID=node-1", got)
	}
	if got.actorID == nil || *got.actorID != "admin-1" {
		t.Errorf("audit actorID = %v, want %q", got.actorID, "admin-1")
	}
	if got.isSuperAdminAction {
		t.Error("audit isSuperAdminAction = true, want false")
	}
}

func TestService_RegisterNode_PermittedBySuperAdmin_NilRegisteredBy(t *testing.T) {
	store := &fakeNodeStore{nextID: "node-1"}
	audit := &fakeAuditRecorder{}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validBareMetalParams())
	if err != nil {
		t.Fatalf("RegisterNode() error: %v", err)
	}
	if store.created[0].RegisteredBy != nil {
		t.Errorf("RegisteredBy = %v, want nil (SuperAdmin is not a Users row)", *store.created[0].RegisteredBy)
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	got := audit.calls[0]
	if got.actorID != nil {
		t.Errorf("audit actorID = %v, want nil", *got.actorID)
	}
	if !got.isSuperAdminAction {
		t.Error("audit isSuperAdminAction = false, want true")
	}
}

func TestService_RegisterNode_NotPermitted(t *testing.T) {
	for _, tier := range []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev} {
		t.Run(string(tier), func(t *testing.T) {
			store := &fakeNodeStore{}
			audit := &fakeAuditRecorder{}
			svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
			actor := rbac.Actor{Tier: tier, UserID: "user-1"}

			_, _, err := svc.RegisterNode(context.Background(), actor, validBareMetalParams())
			if !errors.Is(err, rbac.ErrNotPermitted) {
				t.Errorf("RegisterNode() error = %v, want rbac.ErrNotPermitted", err)
			}
			if len(store.created) != 0 {
				t.Error("nodeStore.Create was called despite the actor not being permitted")
			}
			if len(audit.calls) != 0 {
				t.Error("audit.Record was called despite a refused registration")
			}
		})
	}
}

func TestService_RegisterNode_InvalidParamsNotPersistedOrAudited(t *testing.T) {
	store := &fakeNodeStore{}
	audit := &fakeAuditRecorder{}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
	actor := rbac.Actor{IsSuperAdmin: true}

	params := validBareMetalParams()
	params.Name = ""

	_, _, err := svc.RegisterNode(context.Background(), actor, params)
	if !errors.Is(err, ErrInvalidNode) {
		t.Errorf("RegisterNode() error = %v, want ErrInvalidNode", err)
	}
	if len(store.created) != 0 {
		t.Error("nodeStore.Create was called despite invalid params")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite invalid params")
	}
}

func TestService_RegisterNode_CreateFails(t *testing.T) {
	store := &fakeNodeStore{createErr: errors.New("database unreachable")}
	audit := &fakeAuditRecorder{}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validBareMetalParams())
	if err == nil {
		t.Fatal("RegisterNode() succeeded despite a Create failure")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Create failure")
	}
}

func TestService_RegisterNode_AuditFailurePropagates(t *testing.T) {
	store := &fakeNodeStore{nextID: "node-1"}
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, audit, log.New(io.Discard, "", 0))
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validBareMetalParams())
	if err == nil {
		t.Fatal("RegisterNode() succeeded despite an audit Record failure")
	}
	// The node was already created - not rolled back, same known
	// limitation as rbac.Service.ElevateTier - see PLANNING.md Known
	// Issues and Technical Debt.
	if len(store.created) != 1 {
		t.Error("nodeStore.Create was not called, want the node to have been persisted before the audit write was attempted")
	}
}

func TestService_ListNodes(t *testing.T) {
	want := []*db.Node{{ID: "node-1", Name: "spark-1"}, {ID: "node-2", Name: "spark-2"}}
	store := &fakeNodeStore{listResult: want}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, log.New(io.Discard, "", 0))

	got, err := svc.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes() error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "node-1" || got[1].ID != "node-2" {
		t.Errorf("ListNodes() = %+v, want %+v", got, want)
	}
}

func TestService_ListNodes_StoreError(t *testing.T) {
	store := &fakeNodeStore{listErr: errors.New("database unreachable")}
	svc := NewService(store, &fakeInterfaceStore{}, &fakeDispatcher{}, &fakeAuditRecorder{}, log.New(io.Discard, "", 0))

	if _, err := svc.ListNodes(context.Background()); err == nil {
		t.Fatal("ListNodes() succeeded despite a store failure")
	}
}

func reportEnv(t *testing.T, ifaces ...agentproto.NetworkInterface) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeReportInterfaces, "", agentproto.ReportInterfaces{Interfaces: ifaces})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func newIfaceService(ifaces *fakeInterfaceStore, disp *fakeDispatcher, store *fakeNodeStore, audit *fakeAuditRecorder) *Service {
	return NewService(store, ifaces, disp, audit, log.New(io.Discard, "", 0))
}

func TestService_HandleReportInterfaces_StoresValidAndSkipsInvalid(t *testing.T) {
	ifaces := &fakeInterfaceStore{}
	svc := newIfaceService(ifaces, &fakeDispatcher{}, &fakeNodeStore{}, &fakeAuditRecorder{})
	speed, neg := 10000, -1

	svc.HandleReportInterfaces("node-1", reportEnv(t,
		agentproto.NetworkInterface{Name: "eth0", IPAddress: "10.0.0.5", LinkSpeedMbps: &speed},
		agentproto.NetworkInterface{Name: "eth1", IPAddress: "10.0.1.5", LinkSpeedMbps: &neg},
		agentproto.NetworkInterface{Name: "bad name;rm", IPAddress: "10.0.2.5"},
		agentproto.NetworkInterface{Name: "eth3", IPAddress: "not-an-ip"},
		agentproto.NetworkInterface{Name: "eth0", IPAddress: "10.9.9.9"},
	))

	if len(ifaces.replaced) != 1 {
		t.Fatalf("ReplaceForNode called %d times, want 1", len(ifaces.replaced))
	}
	got := ifaces.replaced[0]
	if len(got) != 2 || got[0].InterfaceName != "eth0" || got[0].IPAddress != "10.0.0.5" || got[0].NodeID != "node-1" {
		t.Fatalf("stored = %+v, want eth0 (first duplicate kept) and eth1 only", got)
	}
	if got[1].InterfaceName != "eth1" || got[1].LinkSpeedMbps != nil {
		t.Errorf("eth1 = %+v, want a non-positive speed normalized to nil", got[1])
	}
}

func TestService_HandleReportInterfaces_EmptyReportClearsInterfaces(t *testing.T) {
	ifaces := &fakeInterfaceStore{}
	svc := newIfaceService(ifaces, &fakeDispatcher{}, &fakeNodeStore{}, &fakeAuditRecorder{})
	svc.HandleReportInterfaces("node-1", reportEnv(t))
	if len(ifaces.replaced) != 1 || len(ifaces.replaced[0]) != 0 {
		t.Errorf("replaced = %+v, want one empty replace", ifaces.replaced)
	}
}

func TestService_HandleReportInterfaces_CapsCount(t *testing.T) {
	ifaces := &fakeInterfaceStore{}
	svc := newIfaceService(ifaces, &fakeDispatcher{}, &fakeNodeStore{}, &fakeAuditRecorder{})
	var many []agentproto.NetworkInterface
	for i := 0; i < 100; i++ {
		many = append(many, agentproto.NetworkInterface{Name: "if" + string(rune('a'+i%26)) + string(rune('a'+i/26)), IPAddress: "10.0.0.1"})
	}
	svc.HandleReportInterfaces("node-1", reportEnv(t, many...))
	if got := len(ifaces.replaced[0]); got != maxReportedInterfaces {
		t.Errorf("stored %d, want cap %d", got, maxReportedInterfaces)
	}
}

func TestService_RescanInterfaces(t *testing.T) {
	admin := rbac.Actor{Tier: db.TierAdmin, UserID: "a"}
	disp := &fakeDispatcher{connected: true}
	svc := newIfaceService(&fakeInterfaceStore{}, disp, &fakeNodeStore{}, &fakeAuditRecorder{})

	if err := svc.RescanInterfaces(context.Background(), rbac.Actor{Tier: db.TierDeveloper, UserID: "d"}, "node-1"); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer error = %v, want ErrNotPermitted", err)
	}
	if err := svc.RescanInterfaces(context.Background(), admin, "node-1"); err != nil {
		t.Fatal(err)
	}
	if len(disp.sent) != 1 || disp.sent[0].Type != agentproto.TypeRescanInterfaces {
		t.Errorf("sent = %+v", disp.sent)
	}
	disp.connected = false
	if err := svc.RescanInterfaces(context.Background(), admin, "node-1"); !errors.Is(err, ErrNodeNotConnected) {
		t.Errorf("offline error = %v, want ErrNodeNotConnected", err)
	}
}

func TestService_SetDefaultTransferInterface(t *testing.T) {
	admin := rbac.Actor{Tier: db.TierAdmin, UserID: "a"}
	store := &fakeNodeStore{findResult: &db.Node{ID: "node-1"}}
	ifaces := &fakeInterfaceStore{listed: []*db.NodeNetworkInterface{{InterfaceName: "eth0"}}}
	audit := &fakeAuditRecorder{}
	svc := newIfaceService(ifaces, &fakeDispatcher{}, store, audit)
	ctx := context.Background()

	if err := svc.SetDefaultTransferInterface(ctx, rbac.Actor{Tier: db.TierDeveloper, UserID: "d"}, "node-1", "eth0"); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer error = %v, want ErrNotPermitted", err)
	}
	if err := svc.SetDefaultTransferInterface(ctx, admin, "node-1", "eth9"); !errors.Is(err, ErrUnknownInterface) {
		t.Errorf("unknown interface error = %v, want ErrUnknownInterface", err)
	}
	if len(store.defaultIfaceCalls) != 0 || len(audit.calls) != 0 {
		t.Fatal("rejected calls must not write or audit")
	}

	if err := svc.SetDefaultTransferInterface(ctx, admin, "node-1", "eth0"); err != nil {
		t.Fatal(err)
	}
	if len(store.defaultIfaceCalls) != 1 || store.defaultIfaceCalls[0] == nil || *store.defaultIfaceCalls[0] != "eth0" {
		t.Errorf("stored = %v, want eth0", store.defaultIfaceCalls)
	}
	if len(audit.calls) != 1 || audit.calls[0].action != "set_default_transfer_interface" || audit.calls[0].objectID != "node-1" {
		t.Errorf("audit = %+v", audit.calls)
	}

	if err := svc.SetDefaultTransferInterface(ctx, admin, "node-1", ""); err != nil {
		t.Fatal(err)
	}
	if last := store.defaultIfaceCalls[len(store.defaultIfaceCalls)-1]; last != nil {
		t.Errorf("empty name stored %q, want nil (Fastest)", *last)
	}
}

func TestService_SetDefaultTransferInterface_UnknownNode(t *testing.T) {
	svc := newIfaceService(&fakeInterfaceStore{}, &fakeDispatcher{}, &fakeNodeStore{}, &fakeAuditRecorder{})
	err := svc.SetDefaultTransferInterface(context.Background(), rbac.Actor{Tier: db.TierAdmin, UserID: "a"}, "nope", "")
	if !errors.Is(err, db.ErrNodeNotFound) {
		t.Errorf("error = %v, want ErrNodeNotFound", err)
	}
}
