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
	gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB       float64
	cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64
}

func (f *fakeMetricsStore) Create(_ context.Context, recordedAt time.Time, nodeID string, runningInstanceID *string,
	gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB, cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64) (*db.Metric, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.calls = append(f.calls, createCall{
		recordedAt, nodeID, runningInstanceID,
		gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB,
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
		RecordedAt:        time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		GPUUtilizationPct: 45, GPUMemoryUsedMB: 8192, GPUMemoryTotalMB: 24576,
		CPUUtilizationPct: 12.5, SystemMemoryUsedMB: 4096, SystemMemoryTotalMB: 16384,
	}
}

func TestService_HandleTelemetry_NoActiveInstance_NilRunningInstanceID(t *testing.T) {
	store := &fakeMetricsStore{}
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

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
	if got.gpuUtilizationPct != want.GPUUtilizationPct || got.gpuMemoryUsedMB != want.GPUMemoryUsedMB ||
		got.gpuMemoryTotalMB != want.GPUMemoryTotalMB || got.cpuUtilizationPct != want.CPUUtilizationPct ||
		got.systemMemoryUsedMB != want.SystemMemoryUsedMB || got.systemMemoryTotalMB != want.SystemMemoryTotalMB {
		t.Errorf("Create call = %+v, want values matching %+v", got, want)
	}
}

func TestService_HandleTelemetry_ActiveInstance_SetsRunningInstanceID(t *testing.T) {
	store := &fakeMetricsStore{}
	instances := &fakeInstanceLookup{active: &db.RunningInstance{ID: "instance-1"}}
	svc := NewService(store, instances, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 1 {
		t.Fatalf("metrics.Create called %d times, want 1", len(store.calls))
	}
	got := store.calls[0].runningInstanceID
	if got == nil || *got != "instance-1" {
		t.Errorf("runningInstanceID = %v, want %q", got, "instance-1")
	}
}

func TestService_HandleTelemetry_InstanceLookupInfraFailure_StillPersistsWithNilID(t *testing.T) {
	store := &fakeMetricsStore{}
	instances := &fakeInstanceLookup{err: errors.New("database unreachable")}
	svc := NewService(store, instances, testLogger())

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
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

	env := newEnvelope(t, agentproto.TypeTelemetry, testTelemetry())
	svc.HandleTelemetry("node-1", env) // must not panic
}

func TestService_HandleTelemetry_IgnoresOtherMessageTypes(t *testing.T) {
	store := &fakeMetricsStore{}
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

	env := newEnvelope(t, agentproto.TypeHeartbeat, agentproto.Heartbeat{})
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 0 {
		t.Error("HandleTelemetry acted on a non-telemetry message type")
	}
}

func TestService_HandleTelemetry_MalformedPayload_Ignored(t *testing.T) {
	store := &fakeMetricsStore{}
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

	env := agentproto.Envelope{Type: agentproto.TypeTelemetry, Payload: []byte(`{"gpu_utilization_pct": "not a number"}`)}
	svc.HandleTelemetry("node-1", env)

	if len(store.calls) != 0 {
		t.Error("HandleTelemetry acted on a malformed payload")
	}
}

func TestService_ListLatestByNode(t *testing.T) {
	store := &fakeMetricsStore{latestByNode: []*db.Metric{{NodeID: "node-1", GPUUtilizationPct: 42}}}
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

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
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListLatestByNode(context.Background()); err == nil {
		t.Fatal("ListLatestByNode() succeeded despite a store failure")
	}
}

func TestService_ListRecent(t *testing.T) {
	store := &fakeMetricsStore{recent: []*db.Metric{{NodeID: "node-1"}, {NodeID: "node-2"}}}
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

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
	svc := NewService(store, &fakeInstanceLookup{}, testLogger())

	if _, err := svc.ListRecent(context.Background()); err == nil {
		t.Fatal("ListRecent() succeeded despite a store failure")
	}
}
