// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// fakeProfileLookup implements profileLookup for tests without a real
// Postgres.
type fakeProfileLookup struct {
	profile *db.Profile
	err     error
}

func (f *fakeProfileLookup) FindByID(_ context.Context, _ string) (*db.Profile, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.profile, nil
}

// fakeInstanceStore implements instanceStore for tests, recording calls
// and letting a test control each result - same pattern as
// internal/nodes' fakeNodeStore.
type fakeInstanceStore struct {
	createErr error
	nextID    string
	created   []*db.RunningInstance

	findByIDResult *db.RunningInstance
	findByIDErr    error

	activeResult *db.RunningInstance
	activeErr    error

	statusCalls  []statusCall
	setStatusErr error
}

type statusCall struct {
	id         string
	status     db.RunningInstanceStatus
	actualPort *int
	errMsg     *string
}

func (f *fakeInstanceStore) Create(_ context.Context, profileID, primaryNodeID string, startedBy *string) (*db.RunningInstance, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "instance-1"
	}
	inst := &db.RunningInstance{
		ID: id, ProfileID: profileID, PrimaryNodeID: primaryNodeID, StartedBy: startedBy,
		Status: db.RunningInstanceStatusStarting, HealthStatus: db.InstanceHealthUnknown,
	}
	f.created = append(f.created, inst)
	return inst, nil
}

func (f *fakeInstanceStore) FindByID(_ context.Context, id string) (*db.RunningInstance, error) {
	if f.findByIDErr != nil {
		return nil, f.findByIDErr
	}
	if f.findByIDResult != nil {
		return f.findByIDResult, nil
	}
	return nil, db.ErrRunningInstanceNotFound
}

func (f *fakeInstanceStore) FindActiveByProfileID(_ context.Context, _ string) (*db.RunningInstance, error) {
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	if f.activeResult != nil {
		return f.activeResult, nil
	}
	return nil, db.ErrRunningInstanceNotFound
}

func (f *fakeInstanceStore) SetStatus(_ context.Context, id string, status db.RunningInstanceStatus, actualPort *int, errorMessage *string) error {
	if f.setStatusErr != nil {
		return f.setStatusErr
	}
	f.statusCalls = append(f.statusCalls, statusCall{id, status, actualPort, errorMessage})
	return nil
}

// fakeAdapter implements engines.Adapter for tests.
type fakeAdapter struct {
	spec    engines.LaunchSpec
	specErr error
}

func (fakeAdapter) RequiresFullGPUResidency() bool       { return true }
func (fakeAdapter) ValidateParams(json.RawMessage) error { return nil }
func (f fakeAdapter) BuildLaunchSpec(json.RawMessage) (engines.LaunchSpec, error) {
	if f.specErr != nil {
		return engines.LaunchSpec{}, f.specErr
	}
	return f.spec, nil
}

// fakeAdapterRegistry implements adapterRegistry for tests.
type fakeAdapterRegistry struct {
	adapter engines.Adapter
	err     error
}

func (f *fakeAdapterRegistry) Adapter(db.ProfileEngineType) (engines.Adapter, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.adapter, nil
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

func targetNodeID(id string) *string { return &id }

func testProfile() *db.Profile {
	return &db.Profile{
		ID: "profile-1", Name: "test-profile", ModelRef: "meta-llama/Llama-3-8B",
		EngineType: db.ProfileEngineVLLM, EngineParams: json.RawMessage(`{}`),
		RequiresFullGPUResidency: true, TargetNodeID: targetNodeID("node-1"), Port: 8000,
	}
}

func newTestService(profile *db.Profile, instances *fakeInstanceStore, adapters *fakeAdapterRegistry, dispatch *fakeDispatcher, audit *fakeAuditRecorder) *Service {
	return NewService(&fakeProfileLookup{profile: profile}, instances, adapters, dispatch, audit, testLogger())
}

func TestService_LoadInstance_PermittedByDeveloper(t *testing.T) {
	instances := &fakeInstanceStore{nextID: "instance-1"}
	adapters := &fakeAdapterRegistry{adapter: fakeAdapter{spec: engines.LaunchSpec{Image: "vllm/vllm-openai:latest", Args: []string{"--tensor-parallel-size", "1"}}}}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := newTestService(testProfile(), instances, adapters, dispatch, audit)
	actor := rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}

	got, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err != nil {
		t.Fatalf("LoadInstance() error: %v", err)
	}
	if got.ID != "instance-1" {
		t.Errorf("ID = %q, want %q", got.ID, "instance-1")
	}
	if len(instances.created) != 1 {
		t.Fatalf("instances.Create called %d times, want 1", len(instances.created))
	}
	created := instances.created[0]
	if created.ProfileID != "profile-1" || created.PrimaryNodeID != "node-1" {
		t.Errorf("Create call = %+v, want ProfileID=profile-1 PrimaryNodeID=node-1", created)
	}
	if created.StartedBy == nil || *created.StartedBy != "dev-1" {
		t.Errorf("StartedBy = %v, want %q", created.StartedBy, "dev-1")
	}

	if len(dispatch.sent) != 1 {
		t.Fatalf("dispatch.Send called %d times, want 1", len(dispatch.sent))
	}
	if dispatch.sentTo[0] != "node-1" {
		t.Errorf("dispatch.Send nodeID = %q, want %q", dispatch.sentTo[0], "node-1")
	}
	if dispatch.sent[0].Type != agentproto.TypeLoadInstance {
		t.Errorf("envelope type = %q, want %q", dispatch.sent[0].Type, agentproto.TypeLoadInstance)
	}
	var payload agentproto.LoadInstance
	if err := dispatch.sent[0].DecodePayload(&payload); err != nil {
		t.Fatalf("decode load_instance payload: %v", err)
	}
	want := agentproto.LoadInstance{
		InstanceID: "instance-1", ModelRef: "meta-llama/Llama-3-8B", Image: "vllm/vllm-openai:latest",
		Args: []string{"--tensor-parallel-size", "1"}, Port: 8000, RequiresFullGPUResidency: true,
	}
	if payload.InstanceID != want.InstanceID || payload.ModelRef != want.ModelRef || payload.Image != want.Image ||
		payload.Port != want.Port || payload.RequiresFullGPUResidency != want.RequiresFullGPUResidency {
		t.Errorf("load_instance payload = %+v, want %+v", payload, want)
	}

	if len(audit.calls) != 1 {
		t.Fatalf("audit.Record called %d times, want 1", len(audit.calls))
	}
	if audit.calls[0].action != "loaded_model" || audit.calls[0].objectType != "running_instance" || audit.calls[0].objectID != "instance-1" {
		t.Errorf("audit call = %+v, want action=loaded_model objectType=running_instance objectID=instance-1", audit.calls[0])
	}
}

func TestService_LoadInstance_PermittedBySuperAdmin_NilStartedBy(t *testing.T) {
	instances := &fakeInstanceStore{nextID: "instance-1"}
	adapters := &fakeAdapterRegistry{adapter: fakeAdapter{}}
	svc := newTestService(testProfile(), instances, adapters, &fakeDispatcher{connected: true}, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err != nil {
		t.Fatalf("LoadInstance() error: %v", err)
	}
	if instances.created[0].StartedBy != nil {
		t.Errorf("StartedBy = %v, want nil (SuperAdmin is not a Users row)", *instances.created[0].StartedBy)
	}
}

func TestService_LoadInstance_NotPermitted(t *testing.T) {
	instances := &fakeInstanceStore{}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, audit)
	actor := rbac.Actor{Tier: db.TierReadOnly, UserID: "user-1"}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("LoadInstance() error = %v, want rbac.ErrNotPermitted", err)
	}
	if len(instances.created) != 0 {
		t.Error("instances.Create was called despite the actor not being permitted")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite the actor not being permitted")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a refused load")
	}
}

func TestService_LoadInstance_InvalidParams(t *testing.T) {
	svc := newTestService(testProfile(), &fakeInstanceStore{}, &fakeAdapterRegistry{adapter: fakeAdapter{}}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{})
	if !errors.Is(err, ErrInvalidLoad) {
		t.Errorf("LoadInstance() error = %v, want ErrInvalidLoad", err)
	}
}

func TestService_LoadInstance_ProfileNotFound(t *testing.T) {
	profiles := &fakeProfileLookup{err: db.ErrProfileNotFound}
	svc := NewService(profiles, &fakeInstanceStore{}, &fakeAdapterRegistry{adapter: fakeAdapter{}}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "missing"})
	if !errors.Is(err, db.ErrProfileNotFound) {
		t.Errorf("LoadInstance() error = %v, want db.ErrProfileNotFound", err)
	}
}

func TestService_LoadInstance_AlreadyRunning(t *testing.T) {
	instances := &fakeInstanceStore{activeResult: &db.RunningInstance{ID: "instance-existing", Status: db.RunningInstanceStatusRunning}}
	dispatch := &fakeDispatcher{connected: true}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("LoadInstance() error = %v, want ErrAlreadyRunning", err)
	}
	if len(instances.created) != 0 {
		t.Error("instances.Create was called despite an already-active instance")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite an already-active instance")
	}
}

func TestService_LoadInstance_TargetNodeOffline(t *testing.T) {
	instances := &fakeInstanceStore{}
	dispatch := &fakeDispatcher{connected: false}
	audit := &fakeAuditRecorder{}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if !errors.Is(err, ErrTargetNodeOffline) {
		t.Errorf("LoadInstance() error = %v, want ErrTargetNodeOffline", err)
	}
	if len(instances.created) != 0 {
		t.Error("instances.Create was called despite an offline target node")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite an offline target node")
	}
}

func TestService_LoadInstance_BuildLaunchSpecFails(t *testing.T) {
	instances := &fakeInstanceStore{}
	adapters := &fakeAdapterRegistry{adapter: fakeAdapter{specErr: engines.ErrInvalidParams}}
	svc := newTestService(testProfile(), instances, adapters, &fakeDispatcher{connected: true}, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err == nil {
		t.Fatal("LoadInstance() succeeded despite a BuildLaunchSpec failure")
	}
	if len(instances.created) != 0 {
		t.Error("instances.Create was called despite a BuildLaunchSpec failure")
	}
}

func TestService_LoadInstance_CreateFails(t *testing.T) {
	instances := &fakeInstanceStore{createErr: errors.New("database unreachable")}
	dispatch := &fakeDispatcher{connected: true}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, &fakeAuditRecorder{})
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err == nil {
		t.Fatal("LoadInstance() succeeded despite a Create failure")
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite a Create failure")
	}
}

func TestService_LoadInstance_DispatchFails(t *testing.T) {
	instances := &fakeInstanceStore{nextID: "instance-1"}
	dispatch := &fakeDispatcher{connected: true, sendErr: errors.New("connection reset")}
	audit := &fakeAuditRecorder{}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err == nil {
		t.Fatal("LoadInstance() succeeded despite a Send failure")
	}
	if len(instances.created) != 1 {
		t.Error("instances.Create was not called, want the instance to have been persisted before dispatch was attempted")
	}
	if len(audit.calls) != 0 {
		t.Error("audit.Record was called despite a Send failure")
	}
}

func TestService_LoadInstance_AuditFailurePropagates(t *testing.T) {
	instances := &fakeInstanceStore{nextID: "instance-1"}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{recordErr: errors.New("database unreachable")}
	svc := newTestService(testProfile(), instances, &fakeAdapterRegistry{adapter: fakeAdapter{}}, dispatch, audit)
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.LoadInstance(context.Background(), actor, LoadParams{ProfileID: "profile-1"})
	if err == nil {
		t.Fatal("LoadInstance() succeeded despite an audit Record failure")
	}
	if len(dispatch.sent) != 1 {
		t.Error("dispatch.Send was not called, want load_instance dispatched before the audit write was attempted")
	}
}

func runningInstance() *db.RunningInstance {
	return &db.RunningInstance{ID: "instance-1", ProfileID: "profile-1", PrimaryNodeID: "node-1", Status: db.RunningInstanceStatusRunning}
}

func TestService_UnloadInstance_Success(t *testing.T) {
	instances := &fakeInstanceStore{findByIDResult: runningInstance()}
	dispatch := &fakeDispatcher{connected: true}
	audit := &fakeAuditRecorder{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, dispatch, audit, testLogger())
	actor := rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}

	got, err := svc.UnloadInstance(context.Background(), actor, "instance-1")
	if err != nil {
		t.Fatalf("UnloadInstance() error: %v", err)
	}
	if got.Status != db.RunningInstanceStatusStopping {
		t.Errorf("returned Status = %q, want %q", got.Status, db.RunningInstanceStatusStopping)
	}

	if len(instances.statusCalls) != 1 {
		t.Fatalf("instances.SetStatus called %d times, want 1", len(instances.statusCalls))
	}
	if instances.statusCalls[0].status != db.RunningInstanceStatusStopping {
		t.Errorf("SetStatus status = %q, want %q", instances.statusCalls[0].status, db.RunningInstanceStatusStopping)
	}

	if len(dispatch.sent) != 1 {
		t.Fatalf("dispatch.Send called %d times, want 1", len(dispatch.sent))
	}
	if dispatch.sentTo[0] != "node-1" {
		t.Errorf("dispatch.Send nodeID = %q, want %q", dispatch.sentTo[0], "node-1")
	}
	if dispatch.sent[0].Type != agentproto.TypeUnloadInstance {
		t.Errorf("envelope type = %q, want %q", dispatch.sent[0].Type, agentproto.TypeUnloadInstance)
	}
	var payload agentproto.UnloadInstance
	if err := dispatch.sent[0].DecodePayload(&payload); err != nil {
		t.Fatalf("decode unload_instance payload: %v", err)
	}
	if payload.InstanceID != "instance-1" {
		t.Errorf("InstanceID = %q, want %q", payload.InstanceID, "instance-1")
	}

	if len(audit.calls) != 1 || audit.calls[0].action != "unloaded_model" {
		t.Errorf("audit calls = %+v, want a single unloaded_model call", audit.calls)
	}
}

func TestService_UnloadInstance_NotPermitted(t *testing.T) {
	instances := &fakeInstanceStore{findByIDResult: runningInstance()}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{Tier: db.TierReadOnly, UserID: "user-1"}

	_, err := svc.UnloadInstance(context.Background(), actor, "instance-1")
	if !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("UnloadInstance() error = %v, want rbac.ErrNotPermitted", err)
	}
	if len(instances.statusCalls) != 0 {
		t.Error("instances.SetStatus was called despite the actor not being permitted")
	}
}

func TestService_UnloadInstance_NotFound(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.UnloadInstance(context.Background(), actor, "missing")
	if !errors.Is(err, db.ErrRunningInstanceNotFound) {
		t.Errorf("UnloadInstance() error = %v, want db.ErrRunningInstanceNotFound", err)
	}
}

func TestService_UnloadInstance_NotRunning(t *testing.T) {
	inst := runningInstance()
	inst.Status = db.RunningInstanceStatusStopped
	instances := &fakeInstanceStore{findByIDResult: inst}
	dispatch := &fakeDispatcher{connected: true}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, dispatch, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.UnloadInstance(context.Background(), actor, "instance-1")
	if !errors.Is(err, ErrInstanceNotRunning) {
		t.Errorf("UnloadInstance() error = %v, want ErrInstanceNotRunning", err)
	}
	if len(dispatch.sent) != 0 {
		t.Error("dispatch.Send was called despite a non-running instance")
	}
}

func TestService_UnloadInstance_TargetNodeOffline(t *testing.T) {
	instances := &fakeInstanceStore{findByIDResult: runningInstance()}
	dispatch := &fakeDispatcher{connected: false}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, dispatch, &fakeAuditRecorder{}, testLogger())
	actor := rbac.Actor{IsSuperAdmin: true}

	_, err := svc.UnloadInstance(context.Background(), actor, "instance-1")
	if !errors.Is(err, ErrTargetNodeOffline) {
		t.Errorf("UnloadInstance() error = %v, want ErrTargetNodeOffline", err)
	}
	if len(instances.statusCalls) != 0 {
		t.Error("instances.SetStatus was called despite an offline target node")
	}
}

func newInstanceResultEnvelope(t *testing.T, result agentproto.InstanceResult) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeInstanceResult, "", result)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

func TestService_HandleInstanceResult_Running_SetsActualPort(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: agentproto.InstanceStatusRunning, ActualPort: 8000})
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(instances.statusCalls))
	}
	got := instances.statusCalls[0]
	if got.status != db.RunningInstanceStatusRunning {
		t.Errorf("status = %q, want %q", got.status, db.RunningInstanceStatusRunning)
	}
	if got.actualPort == nil || *got.actualPort != 8000 {
		t.Errorf("actualPort = %v, want 8000", got.actualPort)
	}
}

func TestService_HandleInstanceResult_Failed_SetsErrorMessage(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: agentproto.InstanceStatusFailed, ErrorMessage: "image pull failed"})
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 1 {
		t.Fatalf("SetStatus called %d times, want 1", len(instances.statusCalls))
	}
	got := instances.statusCalls[0]
	if got.status != db.RunningInstanceStatusFailed {
		t.Errorf("status = %q, want %q", got.status, db.RunningInstanceStatusFailed)
	}
	if got.errMsg == nil || *got.errMsg != "image pull failed" {
		t.Errorf("errMsg = %v, want %q", got.errMsg, "image pull failed")
	}
	if got.actualPort != nil {
		t.Errorf("actualPort = %v, want nil for a failed load", *got.actualPort)
	}
}

func TestService_HandleInstanceResult_Stopped(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: agentproto.InstanceStatusStopped})
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 1 || instances.statusCalls[0].status != db.RunningInstanceStatusStopped {
		t.Errorf("statusCalls = %+v, want a single stopped call", instances.statusCalls)
	}
}

func TestService_HandleInstanceResult_IgnoresOtherMessageTypes(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 0 {
		t.Error("HandleInstanceResult acted on a non-instance_result message type")
	}
}

func TestService_HandleInstanceResult_MalformedPayload_Ignored(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := agentproto.Envelope{Type: agentproto.TypeInstanceResult, Payload: []byte(`{"instance_id": 123}`)}
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 0 {
		t.Error("HandleInstanceResult acted on a malformed payload")
	}
}

func TestService_HandleInstanceResult_UnknownStatus_Ignored(t *testing.T) {
	instances := &fakeInstanceStore{}
	svc := NewService(&fakeProfileLookup{}, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, testLogger())

	env := newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: "unknown_status"})
	svc.HandleInstanceResult("node-1", env)

	if len(instances.statusCalls) != 0 {
		t.Error("HandleInstanceResult acted on an unrecognized status")
	}
}
