// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// fakeProfileStore implements profileStore for tests without a real
// Postgres - same pattern as internal/nodes' fakeNodeStore.
type fakeProfileStore struct {
	createErr error
	updateErr error
	deleteErr error
	nextID    string

	created    []*db.Profile
	updated    []*db.Profile
	deletedIDs []string

	listResult []*db.Profile
	listErr    error

	findResult *db.Profile
	findErr    error
}

func (f *fakeProfileStore) List(_ context.Context) ([]*db.Profile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeProfileStore) Create(_ context.Context, name, modelRef string, engineType db.ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion, quantization *string, targetNodeID string, port int, createdBy *string) (*db.Profile, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "profile-1"
	}
	p := &db.Profile{
		ID: id, Name: name, ModelRef: modelRef, EngineType: engineType, EngineParams: engineParams,
		RequiresFullGPUResidency: requiresFullGPUResidency, RequiredMemoryGB: requiredMemoryGB, EngineVersion: engineVersion, Quantization: quantization,
		Topology: db.ProfileTopologySingleNode, TargetNodeID: &targetNodeID, Port: port, CreatedBy: createdBy,
	}
	f.created = append(f.created, p)
	return p, nil
}

func (f *fakeProfileStore) Update(_ context.Context, id, name, modelRef string, engineType db.ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, engineVersion, quantization *string, targetNodeID string, port int, updatedBy *string) (*db.Profile, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	p := &db.Profile{
		ID: id, Name: name, ModelRef: modelRef, EngineType: engineType, EngineParams: engineParams,
		RequiresFullGPUResidency: requiresFullGPUResidency, RequiredMemoryGB: requiredMemoryGB, EngineVersion: engineVersion, Quantization: quantization,
		Topology: db.ProfileTopologySingleNode, TargetNodeID: &targetNodeID, Port: port, UpdatedBy: updatedBy,
	}
	f.updated = append(f.updated, p)
	return p, nil
}

func (f *fakeProfileStore) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

func (f *fakeProfileStore) FindByID(_ context.Context, id string) (*db.Profile, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findResult, nil
}

// fakeNodeLookup implements nodeLookup.
type fakeNodeLookup struct {
	node    *db.Node
	findErr error
}

func (f *fakeNodeLookup) FindByID(_ context.Context, id string) (*db.Node, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.node == nil {
		return nil, db.ErrNodeNotFound
	}
	return f.node, nil
}

// fakeAdapter implements engines.Adapter.
type fakeAdapter struct {
	requiresFullGPU bool
	validateErr     error
}

func (f fakeAdapter) RequiresFullGPUResidency() bool       { return f.requiresFullGPU }
func (f fakeAdapter) ValidateParams(json.RawMessage) error { return f.validateErr }
func (f fakeAdapter) BuildLaunchSpec(json.RawMessage) (engines.LaunchSpec, error) {
	return engines.LaunchSpec{}, nil
}

// fakeAdapterRegistry implements adapterRegistry.
type fakeAdapterRegistry struct {
	adapter engines.Adapter
	err     error
}

func (f *fakeAdapterRegistry) Adapter(_ db.ProfileEngineType) (engines.Adapter, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.adapter, nil
}

type auditCall struct {
	actorID            *string
	isSuperAdminAction bool
	action             string
	objectType         string
	objectID           string
	detail             map[string]any
}

// fakeAuditRecorder implements auditRecorder.
type fakeAuditRecorder struct {
	recordErr error
	calls     []auditCall
}

func (f *fakeAuditRecorder) Record(_ context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.calls = append(f.calls, auditCall{actorID, isSuperAdminAction, action, objectType, objectID, detail})
	return nil
}

func testDeps() (*fakeProfileStore, *fakeNodeLookup, *fakeAdapterRegistry, *fakeAuditRecorder) {
	return &fakeProfileStore{nextID: "profile-1"},
		&fakeNodeLookup{node: &db.Node{ID: "node-1", Name: "spark-1"}},
		&fakeAdapterRegistry{adapter: fakeAdapter{requiresFullGPU: true}},
		&fakeAuditRecorder{}
}

func TestService_CreateProfile_PermittedByPowerDev(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	p, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err != nil {
		t.Fatalf("CreateProfile() error: %v", err)
	}
	if p.ID != "profile-1" {
		t.Errorf("ID = %q, want %q", p.ID, "profile-1")
	}
	if !p.RequiresFullGPUResidency {
		t.Error("RequiresFullGPUResidency = false, want true (from the fake adapter)")
	}
	if len(store.created) != 1 {
		t.Fatalf("profileStore.Create called %d times, want 1", len(store.created))
	}
	if store.created[0].CreatedBy == nil || *store.created[0].CreatedBy != "pd-1" {
		t.Errorf("CreatedBy = %v, want %q", store.created[0].CreatedBy, "pd-1")
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	got := audit.calls[0]
	if got.action != "created_profile" || got.objectType != "model_profile" || got.objectID != "profile-1" {
		t.Errorf("audit call = %+v, want action=created_profile objectType=model_profile objectID=profile-1", got)
	}
}

func TestService_CreateProfile_EngineVersion_RoundTrips(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	version := "b4610"
	fields := validFields()
	fields.EngineVersion = &version

	p, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: fields})
	if err != nil {
		t.Fatalf("CreateProfile() error: %v", err)
	}
	if p.EngineVersion == nil || *p.EngineVersion != version {
		t.Errorf("EngineVersion = %v, want %q", p.EngineVersion, version)
	}

	// A pinned version is passed straight through to the store, not
	// validated against any node/inventory lookup - confirmed deliberately
	// (a bad pin fails clearly at launch time instead, see
	// PLANNING.md's per-profile engine version pinning entry) by the fact
	// that nodes/adapters here never see EngineVersion at all.
	if len(store.created) != 1 || store.created[0].EngineVersion == nil || *store.created[0].EngineVersion != version {
		t.Fatalf("store.Create was not called with EngineVersion=%q", version)
	}
}

func TestService_CreateProfile_EngineVersion_NilWhenUnset(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	p, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err != nil {
		t.Fatalf("CreateProfile() error: %v", err)
	}
	if p.EngineVersion != nil {
		t.Errorf("EngineVersion = %v, want nil (unpinned) when not set", *p.EngineVersion)
	}
}

func TestService_CreateProfile_PermittedBySuperAdmin_NilCreatedBy(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err != nil {
		t.Fatalf("CreateProfile() error: %v", err)
	}
	if store.created[0].CreatedBy != nil {
		t.Errorf("CreatedBy = %v, want nil (SuperAdmin is not a Users row)", *store.created[0].CreatedBy)
	}
	if !audit.calls[0].isSuperAdminAction {
		t.Error("audit isSuperAdminAction = false, want true")
	}
}

func TestService_CreateProfile_NotPermitted(t *testing.T) {
	for _, tier := range []db.Tier{db.TierReadOnly, db.TierDeveloper} {
		t.Run(string(tier), func(t *testing.T) {
			store, nodes, adapters, audit := testDeps()
			svc := NewService(store, nodes, adapters, audit)
			actor := rbac.Actor{Tier: tier, UserID: "user-1"}

			_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
			if !errors.Is(err, rbac.ErrNotPermitted) {
				t.Errorf("CreateProfile() error = %v, want rbac.ErrNotPermitted", err)
			}
			if len(store.created) != 0 {
				t.Error("profileStore.Create was called despite the actor not being permitted")
			}
			if len(audit.calls) != 0 {
				t.Error("audit.Record was called despite a refused creation")
			}
		})
	}
}

func TestService_CreateProfile_InvalidFieldsNotPersistedOrAudited(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	fields := validFields()
	fields.Name = ""

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: fields})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("CreateProfile() error = %v, want ErrInvalidProfile", err)
	}
	if len(store.created) != 0 {
		t.Error("profileStore.Create was called despite invalid fields")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite invalid fields")
	}
}

func TestService_CreateProfile_AdapterValidationFailure(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	adapters.adapter = fakeAdapter{validateErr: errors.New("bad engine_params")}
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("CreateProfile() error = %v, want ErrInvalidProfile", err)
	}
	if len(store.created) != 0 {
		t.Error("profileStore.Create was called despite a failed adapter validation")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a failed adapter validation")
	}
}

func TestService_CreateProfile_UnknownEngineType(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	adapters.err = engines.ErrUnknownEngineType
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("CreateProfile() error = %v, want ErrInvalidProfile", err)
	}
	if len(store.created) != 0 {
		t.Error("profileStore.Create was called despite an unknown engine type")
	}
}

func TestService_CreateProfile_TargetNodeNotFound(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	nodes.node = nil
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("CreateProfile() error = %v, want ErrInvalidProfile", err)
	}
	if len(store.created) != 0 {
		t.Error("profileStore.Create was called despite a missing target node")
	}
}

func TestService_CreateProfile_NodeLookupInfraFailure(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	nodes.findErr = errors.New("database unreachable")
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err == nil {
		t.Fatal("CreateProfile() succeeded despite a node lookup failure")
	}
	if errors.Is(err, ErrInvalidProfile) {
		t.Error("CreateProfile() returned ErrInvalidProfile for an infrastructure failure, want a distinct error")
	}
}

func TestService_CreateProfile_CreateFails(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.createErr = errors.New("database unreachable")
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err == nil {
		t.Fatal("CreateProfile() succeeded despite a Create failure")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Create failure")
	}
}

func TestService_CreateProfile_AuditFailurePropagates(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	audit.recordErr = errors.New("database unreachable")
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: validFields()})
	if err == nil {
		t.Fatal("CreateProfile() succeeded despite an audit Record failure")
	}
	if len(store.created) != 1 {
		t.Error("profileStore.Create was not called, want the profile to have been persisted before the audit write was attempted")
	}
}

// TestService_CreateProfile_RealAdapterRegistry_BothEngineTypes uses the
// actual engines.Registry (not fakeAdapterRegistry), so this exercises
// the real vLLM and llama.cpp adapters' ValidateParams and
// RequiresFullGPUResidency through the service, not just a stub -
// matching this phase's own "happy path for both engine types"
// completion bar.
func TestService_CreateProfile_RealAdapterRegistry_BothEngineTypes(t *testing.T) {
	tests := []struct {
		engineType           db.ProfileEngineType
		params               string
		wantFullGPUResidency bool
	}{
		{db.ProfileEngineVLLM, `{"tensor_parallel_size":1,"dtype":"bfloat16"}`, true},
		{db.ProfileEngineLlamaCPP, `{"n_gpu_layers":0,"ctx_size":4096}`, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.engineType), func(t *testing.T) {
			store := &fakeProfileStore{nextID: "profile-1"}
			nodes := &fakeNodeLookup{node: &db.Node{ID: "node-1", Name: "spark-1"}}
			audit := &fakeAuditRecorder{}
			svc := NewService(store, nodes, engines.NewRegistry(), audit)
			actor := rbac.Actor{IsSuperAdmin: true}

			fields := validFields()
			fields.EngineType = tt.engineType
			fields.EngineParams = json.RawMessage(tt.params)

			p, err := svc.CreateProfile(context.Background(), actor, CreateParams{Fields: fields})
			if err != nil {
				t.Fatalf("CreateProfile() error: %v", err)
			}
			if p.RequiresFullGPUResidency != tt.wantFullGPUResidency {
				t.Errorf("RequiresFullGPUResidency = %v, want %v", p.RequiresFullGPUResidency, tt.wantFullGPUResidency)
			}
		})
	}
}

func TestService_UpdateProfile_Success(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	p, err := svc.UpdateProfile(context.Background(), actor, UpdateParams{ID: "profile-1", Fields: validFields()})
	if err != nil {
		t.Fatalf("UpdateProfile() error: %v", err)
	}
	if p.ID != "profile-1" {
		t.Errorf("ID = %q, want %q", p.ID, "profile-1")
	}
	if len(store.updated) != 1 {
		t.Fatalf("profileStore.Update called %d times, want 1", len(store.updated))
	}
	if store.updated[0].UpdatedBy == nil || *store.updated[0].UpdatedBy != "admin-1" {
		t.Errorf("UpdatedBy = %v, want %q", store.updated[0].UpdatedBy, "admin-1")
	}
	if len(audit.calls) != 1 || audit.calls[0].action != "updated_profile" {
		t.Errorf("audit calls = %+v, want one updated_profile call", audit.calls)
	}
}

func TestService_UpdateProfile_EngineVersion_RoundTrips(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

	version := "b4523"
	fields := validFields()
	fields.EngineVersion = &version

	p, err := svc.UpdateProfile(context.Background(), actor, UpdateParams{ID: "profile-1", Fields: fields})
	if err != nil {
		t.Fatalf("UpdateProfile() error: %v", err)
	}
	if p.EngineVersion == nil || *p.EngineVersion != version {
		t.Errorf("EngineVersion = %v, want %q", p.EngineVersion, version)
	}
}

func TestService_UpdateProfile_NotPermitted(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierDeveloper, UserID: "user-1"}

	_, err := svc.UpdateProfile(context.Background(), actor, UpdateParams{ID: "profile-1", Fields: validFields()})
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("UpdateProfile() error = %v, want rbac.ErrNotPermitted", err)
	}
	if len(store.updated) != 0 {
		t.Error("profileStore.Update was called despite the actor not being permitted")
	}
}

func TestService_UpdateProfile_NotFound(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.updateErr = db.ErrProfileNotFound
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.UpdateProfile(context.Background(), actor, UpdateParams{ID: "no-such-profile", Fields: validFields()})
	if !errors.Is(err, db.ErrProfileNotFound) {
		t.Errorf("UpdateProfile() error = %v, want db.ErrProfileNotFound", err)
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a not-found update")
	}
}

func TestService_DeleteProfile_Success(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierPowerDev, UserID: "pd-1"}

	if err := svc.DeleteProfile(context.Background(), actor, "profile-1"); err != nil {
		t.Fatalf("DeleteProfile() error: %v", err)
	}
	if len(store.deletedIDs) != 1 || store.deletedIDs[0] != "profile-1" {
		t.Errorf("deletedIDs = %v, want [profile-1]", store.deletedIDs)
	}
	if len(audit.calls) != 1 || audit.calls[0].action != "deleted_profile" {
		t.Errorf("audit calls = %+v, want one deleted_profile call", audit.calls)
	}
}

func TestService_DeleteProfile_NotPermitted(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{Tier: db.TierReadOnly, UserID: "user-1"}

	err := svc.DeleteProfile(context.Background(), actor, "profile-1")
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("DeleteProfile() error = %v, want rbac.ErrNotPermitted", err)
	}
	if len(store.deletedIDs) != 0 {
		t.Error("profileStore.Delete was called despite the actor not being permitted")
	}
}

func TestService_DeleteProfile_NotFound(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.deleteErr = db.ErrProfileNotFound
	svc := NewService(store, nodes, adapters, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	err := svc.DeleteProfile(context.Background(), actor, "no-such-profile")
	if !errors.Is(err, db.ErrProfileNotFound) {
		t.Errorf("DeleteProfile() error = %v, want db.ErrProfileNotFound", err)
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a not-found delete")
	}
}

func TestService_ListProfiles(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	want := []*db.Profile{{ID: "profile-1", Name: "a"}, {ID: "profile-2", Name: "b"}}
	store.listResult = want
	svc := NewService(store, nodes, adapters, audit)

	got, err := svc.ListProfiles(context.Background())
	if err != nil {
		t.Fatalf("ListProfiles() error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "profile-1" || got[1].ID != "profile-2" {
		t.Errorf("ListProfiles() = %+v, want %+v", got, want)
	}
}

func TestService_ListProfiles_StoreError(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.listErr = errors.New("database unreachable")
	svc := NewService(store, nodes, adapters, audit)

	if _, err := svc.ListProfiles(context.Background()); err == nil {
		t.Fatal("ListProfiles() succeeded despite a store failure")
	}
}

func TestService_GetProfile(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.findResult = &db.Profile{ID: "profile-1", Name: "llama-70b"}
	svc := NewService(store, nodes, adapters, audit)

	got, err := svc.GetProfile(context.Background(), "profile-1")
	if err != nil {
		t.Fatalf("GetProfile() error: %v", err)
	}
	if got.Name != "llama-70b" {
		t.Errorf("GetProfile().Name = %q, want %q", got.Name, "llama-70b")
	}
}

func TestService_GetProfile_NotFound(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.findErr = db.ErrProfileNotFound
	svc := NewService(store, nodes, adapters, audit)

	_, err := svc.GetProfile(context.Background(), "does-not-exist")
	if !errors.Is(err, db.ErrProfileNotFound) {
		t.Errorf("GetProfile() error = %v, want db.ErrProfileNotFound", err)
	}
}

func TestService_GetProfile_StoreError(t *testing.T) {
	store, nodes, adapters, audit := testDeps()
	store.findErr = errors.New("database unreachable")
	svc := NewService(store, nodes, adapters, audit)

	_, err := svc.GetProfile(context.Background(), "profile-1")
	if err == nil {
		t.Fatal("GetProfile() succeeded despite a store failure")
	}
	if errors.Is(err, db.ErrProfileNotFound) {
		t.Error("GetProfile() returned ErrProfileNotFound for an infrastructure failure")
	}
}
