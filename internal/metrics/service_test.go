// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// fakeMetricsStore implements metricsStore for tests without a real
// Postgres, recording every call.
type fakeMetricsStore struct {
	createErr error
	calls     []createCall

	latestByNode    []*db.Metric
	latestByNodeErr error
	recent          []*db.Metric
	recentErr       error
}

type createCall struct {
	recordedAt                                                 time.Time
	nodeID                                                     string
	runningInstanceID                                          *string
	cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64
}

func (f *fakeMetricsStore) Create(_ context.Context, recordedAt time.Time, nodeID string, runningInstanceID *string,
	cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64) (*db.Metric, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.calls = append(f.calls, createCall{
		recordedAt, nodeID, runningInstanceID,
		cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB,
	})
	return &db.Metric{RecordedAt: recordedAt, NodeID: nodeID, RunningInstanceID: runningInstanceID}, nil
}

func (f *fakeMetricsStore) LatestByNode(context.Context) ([]*db.Metric, error) {
	if f.latestByNodeErr != nil {
		return nil, f.latestByNodeErr
	}
	return f.latestByNode, nil
}

func (f *fakeMetricsStore) Recent(context.Context) ([]*db.Metric, error) {
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.recent, nil
}

// fakeGPUMetricsStore implements gpuMetricsStore for tests without a real
// Postgres, recording every call - mirrors fakeMetricsStore's exact shape.
type fakeGPUMetricsStore struct {
	createErr error
	calls     []gpuCreateCall

	latestByNodeAndGPU    []*db.GPUMetric
	latestByNodeAndGPUErr error
	recent                []*db.GPUMetric
	recentErr             error
}

type gpuCreateCall struct {
	recordedAt                      time.Time
	nodeID                          string
	gpuIndex                        int
	runningInstanceID               *string
	utilizationPct, usedMB, totalMB float64
}

func (f *fakeGPUMetricsStore) Create(_ context.Context, recordedAt time.Time, nodeID string, gpuIndex int, runningInstanceID *string,
	utilizationPct, usedMB, totalMB float64) (*db.GPUMetric, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.calls = append(f.calls, gpuCreateCall{
		recordedAt, nodeID, gpuIndex, runningInstanceID,
		utilizationPct, usedMB, totalMB,
	})
	return &db.GPUMetric{RecordedAt: recordedAt, NodeID: nodeID, GPUIndex: gpuIndex, RunningInstanceID: runningInstanceID}, nil
}

func (f *fakeGPUMetricsStore) LatestByNodeAndGPU(context.Context) ([]*db.GPUMetric, error) {
	if f.latestByNodeAndGPUErr != nil {
		return nil, f.latestByNodeAndGPUErr
	}
	return f.latestByNodeAndGPU, nil
}

func (f *fakeGPUMetricsStore) Recent(context.Context) ([]*db.GPUMetric, error) {
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.recent, nil
}

// fakeInstanceLookup implements instanceLookup for tests.
type fakeInstanceLookup struct {
	active *db.RunningInstance
	err    error
}

func (f *fakeInstanceLookup) FindActiveByNode(_ context.Context, _ string) (*db.RunningInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.active != nil {
		return f.active, nil
	}
	return nil, db.ErrRunningInstanceNotFound
}

func testLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

func newEnvelope(t *testing.T, msgType agentproto.MessageType, payload any) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(msgType, "", payload)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

func testTelemetry() agentproto.Telemetry {
	return agentproto.Telemetry{
		RecordedAt:          time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		GPUs:                []agentproto.GPUTelemetry{{Index: 0, UtilizationPct: 45, MemoryUsedMB: 8192, MemoryTotalMB: 24576}},
		CPUUtilizationPct:   12.5,
		SystemMemoryUsedMB:  4096,
		SystemMemoryTotalMB: 16384,
	}
}

func TestService_HandleTelemetry_NoActiveInstance_NilRunningInstanceID(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &fakeGPUMetricsStore{}
	svc := NewService(store, gpuStore, &fakeInstanceLookup{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 1 {
		t.Fatalf("metrics.Create called %d times, want 1", len(store.calls))
	}
	got := store.calls[0]
	if got.nodeID != "node-1" {
		t.Errorf("nodeID = %q, want %q", got.nodeID, "node-1")
	}
	if got.runningInstanceID != nil {
		t.Errorf("runningInstanceID = %v, want nil when nothing is active", *got.runningInstanceID)
	}
	want := testTelemetry()
	if !got.recordedAt.Equal(want.RecordedAt) {
		t.Errorf("recordedAt = %v, want %v", got.recordedAt, want.RecordedAt)
	}
	if got.cpuUtilizationPct != want.CPUUtilizationPct ||
		got.systemMemoryUsedMB != want.SystemMemoryUsedMB || got.systemMemoryTotalMB != want.SystemMemoryTotalMB {
		t.Errorf("Create call = %+v, want values matching %+v", got, want)
	}

	if len(gpuStore.calls) != 1 {
		t.Fatalf("gpuMetrics.Create called %d times, want 1 (one per GPU in the telemetry payload)", len(gpuStore.calls))
	}
	gotGPU := gpuStore.calls[0]
	wantGPU := want.GPUs[0]
	if gotGPU.nodeID != "node-1" || gotGPU.gpuIndex != wantGPU.Index ||
		gotGPU.utilizationPct != wantGPU.UtilizationPct || gotGPU.usedMB != wantGPU.MemoryUsedMB || gotGPU.totalMB != wantGPU.MemoryTotalMB {
		t.Errorf("gpuMetrics Create call = %+v, want values matching %+v", gotGPU, wantGPU)
	}
	if gotGPU.runningInstanceID != nil {
		t.Errorf("gpu runningInstanceID = %v, want nil when nothing is active", *gotGPU.runningInstanceID)
	}
}

func TestService_HandleTelemetry_ActiveInstance_SetsRunningInstanceID(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &fakeGPUMetricsStore{}
	instances := &fakeInstanceLookup{active: &db.RunningInstance{ID: "instance-1"}}
	svc := NewService(store, gpuStore, instances, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 1 {
		t.Fatalf("metrics.Create called %d times, want 1", len(store.calls))
	}
	got := store.calls[0].runningInstanceID
	if got == nil || *got != "instance-1" {
		t.Errorf("runningInstanceID = %v, want %q", got, "instance-1")
	}

	if len(gpuStore.calls) != 1 {
		t.Fatalf("gpuMetrics.Create called %d times, want 1", len(gpuStore.calls))
	}
	gotGPU := gpuStore.calls[0].runningInstanceID
	if gotGPU == nil || *gotGPU != "instance-1" {
		t.Errorf("gpu runningInstanceID = %v, want %q", gotGPU, "instance-1")
	}
}

func TestService_HandleTelemetry_InstanceLookupInfraFailure_StillPersistsWithNilID(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &fakeGPUMetricsStore{}
	instances := &fakeInstanceLookup{err: errors.New("database unreachable")}
	svc := NewService(store, gpuStore, instances, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 1 {
		t.Fatalf("metrics.Create called %d times, want 1 (a correlation lookup failure must not drop the reading)", len(store.calls))
	}
	if store.calls[0].runningInstanceID != nil {
		t.Errorf("runningInstanceID = %v, want nil when the lookup itself failed", *store.calls[0].runningInstanceID)
	}
}

func TestService_HandleTelemetry_CreateFails_NoPanic(t *testing.T) {
	store := &fakeMetricsStore{createErr: errors.New("database unreachable")}
	gpuStore := &fakeGPUMetricsStore{}
	svc := NewService(store, gpuStore, &fakeInstanceLookup{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env) // must not panic

	if len(gpuStore.calls) != 1 {
		t.Errorf("gpuMetrics.Create called %d times, want 1 - a node-level write failure must not block the GPU writes", len(gpuStore.calls))
	}
}

// TestService_HandleTelemetry_OneGPUWriteFails_OthersStillPersist proves
// each GPU's write is independently best-effort: one failing does not
// drop the node-level write or any other GPU's write.
func TestService_HandleTelemetry_OneGPUWriteFails_OthersStillPersist(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &failNthGPUCreateStore{failOn: 1}
	svc := NewService(store, gpuStore, &fakeInstanceLookup{}, testLogger())

	telemetry := testTelemetry()
	telemetry.GPUs = []agentproto.GPUTelemetry{
		{Index: 0, UtilizationPct: 10, MemoryUsedMB: 1024, MemoryTotalMB: 24576},
		{Index: 1, UtilizationPct: 20, MemoryUsedMB: 2048, MemoryTotalMB: 24576},
	}
	env := newEnvelope(t, agentproto.TypeTelemetry, telemetry)
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 1 {
		t.Errorf("metrics.Create called %d times, want 1", len(store.calls))
	}
	if len(gpuStore.succeeded) != 1 || gpuStore.succeeded[0] != 0 {
		t.Errorf("succeeded GPU indices = %v, want [0] (index 1's write fails, index 0's still persists)", gpuStore.succeeded)
	}
}

// failNthGPUCreateStore fails the (0-indexed) failOn'th Create call only,
// recording every gpu_index that actually succeeded.
type failNthGPUCreateStore struct {
	failOn    int
	calls     int
	succeeded []int
}

func (f *failNthGPUCreateStore) Create(_ context.Context, _ time.Time, _ string, gpuIndex int, _ *string,
	_, _, _ float64) (*db.GPUMetric, error) {
	call := f.calls
	f.calls++
	if call == f.failOn {
		return nil, errors.New("database unreachable")
	}
	f.succeeded = append(f.succeeded, gpuIndex)
	return &db.GPUMetric{GPUIndex: gpuIndex}, nil
}

func (f *failNthGPUCreateStore) LatestByNodeAndGPU(context.Context) ([]*db.GPUMetric, error) {
	return nil, nil
}

func (f *failNthGPUCreateStore) Recent(context.Context) ([]*db.GPUMetric, error) {
	return nil, nil
}

func TestService_HandleTelemetry_IgnoresOtherMessageTypes(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &fakeGPUMetricsStore{}
	svc := NewService(store, gpuStore, &fakeInstanceLookup{}, testLogger())

	env := newEnvelope(t, agentproto.TypeHeartbeat, agentproto.Heartbeat{})
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 0 {
		t.Error("HandleTelemetry acted on a non-telemetry message type")
	}
}

func TestService_HandleTelemetry_MalformedPayload_Ignored(t *testing.T) {
	store := &fakeMetricsStore{}
	gpuStore := &fakeGPUMetricsStore{}
	svc := NewService(store, gpuStore, &fakeInstanceLookup{}, testLogger())

	env := agentproto.Envelope{Type: agentproto.TypeTelemetry, Payload: []byte(`{"cpu_utilization_pct": "not a number"}`)}
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 0 {
		t.Error("HandleTelemetry acted on a malformed payload")
	}
}

func TestService_ListLatestByNode(t *testing.T) {
	store := &fakeMetricsStore{latestByNode: []*db.Metric{{NodeID: "node-1", CPUUtilizationPct: 42}}}
	svc := NewService(store, &fakeGPUMetricsStore{}, &fakeInstanceLookup{}, testLogger())

	got, err := svc.ListLatestByNode(context.Background())
	if err != nil {
		t.Fatalf("ListLatestByNode() error: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "node-1" {
		t.Errorf("ListLatestByNode() = %+v, want one metric for node-1", got)
	}
}

func TestService_ListLatestByNode_StoreFailurePropagates(t *testing.T) {
	store := &fakeMetricsStore{latestByNodeErr: errors.New("database unreachable")}
	svc := NewService(store, &fakeGPUMetricsStore{}, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListLatestByNode(context.Background()); err == nil {
		t.Fatal("ListLatestByNode() succeeded despite a store failure")
	}
}

func TestService_ListRecent(t *testing.T) {
	store := &fakeMetricsStore{recent: []*db.Metric{{NodeID: "node-1"}, {NodeID: "node-2"}}}
	svc := NewService(store, &fakeGPUMetricsStore{}, &fakeInstanceLookup{}, testLogger())

	got, err := svc.ListRecent(context.Background())
	if err != nil {
		t.Fatalf("ListRecent() error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

func TestService_ListRecent_StoreFailurePropagates(t *testing.T) {
	store := &fakeMetricsStore{recentErr: errors.New("database unreachable")}
	svc := NewService(store, &fakeGPUMetricsStore{}, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListRecent(context.Background()); err == nil {
		t.Fatal("ListRecent() succeeded despite a store failure")
	}
}

func TestService_ListLatestGPUByNode(t *testing.T) {
	gpuStore := &fakeGPUMetricsStore{latestByNodeAndGPU: []*db.GPUMetric{{NodeID: "node-1", GPUIndex: 0, UtilizationPct: 42}}}
	svc := NewService(&fakeMetricsStore{}, gpuStore, &fakeInstanceLookup{}, testLogger())

	got, err := svc.ListLatestGPUByNode(context.Background())
	if err != nil {
		t.Fatalf("ListLatestGPUByNode() error: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "node-1" || got[0].GPUIndex != 0 {
		t.Errorf("ListLatestGPUByNode() = %+v, want one metric for node-1 gpu 0", got)
	}
}

func TestService_ListLatestGPUByNode_StoreFailurePropagates(t *testing.T) {
	gpuStore := &fakeGPUMetricsStore{latestByNodeAndGPUErr: errors.New("database unreachable")}
	svc := NewService(&fakeMetricsStore{}, gpuStore, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListLatestGPUByNode(context.Background()); err == nil {
		t.Fatal("ListLatestGPUByNode() succeeded despite a store failure")
	}
}

func TestService_ListRecentGPU(t *testing.T) {
	gpuStore := &fakeGPUMetricsStore{recent: []*db.GPUMetric{{NodeID: "node-1", GPUIndex: 0}, {NodeID: "node-1", GPUIndex: 1}}}
	svc := NewService(&fakeMetricsStore{}, gpuStore, &fakeInstanceLookup{}, testLogger())

	got, err := svc.ListRecentGPU(context.Background())
	if err != nil {
		t.Fatalf("ListRecentGPU() error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

func TestService_ListRecentGPU_StoreFailurePropagates(t *testing.T) {
	gpuStore := &fakeGPUMetricsStore{recentErr: errors.New("database unreachable")}
	svc := NewService(&fakeMetricsStore{}, gpuStore, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListRecentGPU(context.Background()); err == nil {
		t.Fatal("ListRecentGPU() succeeded despite a store failure")
	}
}
