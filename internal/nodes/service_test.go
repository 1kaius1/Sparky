// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"errors"
	"testing"

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
}

func (f *fakeNodeStore) List(_ context.Context) ([]*db.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeNodeStore) Create(_ context.Context, name, hostname, ipAddress string, nodeType db.NodeType, containerRuntime *db.ContainerRuntime, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string, bearerTokenHash string) (*db.Node, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "node-1"
	}
	n := &db.Node{
		ID:               id,
		Name:             name,
		Hostname:         hostname,
		IPAddress:        ipAddress,
		NodeType:         nodeType,
		ContainerRuntime: containerRuntime,
		GPUMemoryGB:      gpuMemoryGB,
		CPUMemoryGB:      cpuMemoryGB,
		AgentStatus:      db.AgentStatusOffline,
		RegisteredBy:     registeredBy,
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
	svc := NewService(store, audit)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	n, token, err := svc.RegisterNode(context.Background(), actor, validSparkParams())
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
	svc := NewService(store, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validSparkParams())
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
			svc := NewService(store, audit)
			actor := rbac.Actor{Tier: tier, UserID: "user-1"}

			_, _, err := svc.RegisterNode(context.Background(), actor, validSparkParams())
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
	svc := NewService(store, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	params := validSparkParams()
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
	svc := NewService(store, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validSparkParams())
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
	svc := NewService(store, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, _, err := svc.RegisterNode(context.Background(), actor, validSparkParams())
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
	svc := NewService(store, &fakeAuditRecorder{})

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
	svc := NewService(store, &fakeAuditRecorder{})

	if _, err := svc.ListNodes(context.Background()); err == nil {
		t.Fatal("ListNodes() succeeded despite a store failure")
	}
}
