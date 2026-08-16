// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// createTestProfile creates a throwaway single-node model profile to
// satisfy running_instances.profile_id's foreign key, cleaned up after
// the test.
func createTestProfile(t *testing.T, profiles *ProfileRepository, nodeID string) *Profile {
	t.Helper()
	p, err := profiles.Create(context.Background(), fmt.Sprintf("profile-%s", t.Name()), "Qwen/Qwen2.5-0.5B-Instruct",
		ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, nil, nodeID, 8000, nil)
	if err != nil {
		t.Fatalf("create test profile: %v", err)
	}
	t.Cleanup(func() {
		_, _ = profiles.pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, p.ID)
	})
	return p
}

// createTestRunningInstance creates a throwaway running instance in
// RunningInstanceStatusStarting, cleaned up after the test.
func createTestRunningInstance(t *testing.T, instances *RunningInstanceRepository, profileID, nodeID string, startedBy *string) *RunningInstance {
	t.Helper()
	inst, err := instances.Create(context.Background(), profileID, nodeID, startedBy)
	if err != nil {
		t.Fatalf("create test running instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = instances.pool.Exec(context.Background(), `DELETE FROM running_instances WHERE id = $1`, inst.ID)
	})
	return inst
}

func TestRunningInstanceRepository_Create(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	starter := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)

	inst, err := instances.Create(ctx, profile.ID, node.ID, &starter.ID)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM running_instances WHERE id = $1`, inst.ID)
	})

	if inst.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if inst.Status != RunningInstanceStatusStarting {
		t.Errorf("Status = %q, want default %q", inst.Status, RunningInstanceStatusStarting)
	}
	if inst.HealthStatus != InstanceHealthUnknown {
		t.Errorf("HealthStatus = %q, want default %q", inst.HealthStatus, InstanceHealthUnknown)
	}
	if inst.PrimaryNodeID != node.ID {
		t.Errorf("PrimaryNodeID = %q, want %q", inst.PrimaryNodeID, node.ID)
	}
	if inst.StartedBy == nil || *inst.StartedBy != starter.ID {
		t.Errorf("StartedBy = %v, want %q", inst.StartedBy, starter.ID)
	}
	if inst.ActualPort != nil {
		t.Errorf("ActualPort = %v, want nil for a newly-created instance", *inst.ActualPort)
	}
	if inst.StoppedAt != nil {
		t.Errorf("StoppedAt = %v, want nil for a newly-created instance", inst.StoppedAt)
	}
}

func TestRunningInstanceRepository_Create_NilStartedBy(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)

	// nil for the same reason nodes.registered_by can be: the break-glass
	// SuperAdmin is not a Users row.
	inst, err := instances.Create(ctx, profile.ID, node.ID, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM running_instances WHERE id = $1`, inst.ID)
	})

	if inst.StartedBy != nil {
		t.Errorf("StartedBy = %v, want nil", *inst.StartedBy)
	}
}

func TestRunningInstanceRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	got, err := instances.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ID != created.ID || got.ProfileID != created.ProfileID {
		t.Errorf("FindByID() = %+v, want ID=%q ProfileID=%q", got, created.ID, created.ProfileID)
	}
}

func TestRunningInstanceRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	instances := NewRunningInstanceRepository(pool)

	_, err := instances.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrRunningInstanceNotFound {
		t.Errorf("FindByID() error = %v, want ErrRunningInstanceNotFound", err)
	}
}

func TestRunningInstanceRepository_FindActiveByProfileID_Active(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	got, err := instances.FindActiveByProfileID(ctx, profile.ID)
	if err != nil {
		t.Fatalf("FindActiveByProfileID() error: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("FindActiveByProfileID() ID = %q, want %q", got.ID, created.ID)
	}
}

func TestRunningInstanceRepository_FindActiveByProfileID_NoneActive(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusStopped, nil, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	_, err := instances.FindActiveByProfileID(ctx, profile.ID)
	if err != ErrRunningInstanceNotFound {
		t.Errorf("FindActiveByProfileID() error = %v, want ErrRunningInstanceNotFound once the only instance is stopped", err)
	}
}

func TestRunningInstanceRepository_FindActiveByNode_Running(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	// FindActiveByNode requires status = running specifically - starting
	// (created's default) doesn't count, unlike FindActiveByProfileID's
	// broader starting/running/stopping definition of "active".
	port := 8000
	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusRunning, &port, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := instances.FindActiveByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("FindActiveByNode() error: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("FindActiveByNode() ID = %q, want %q", got.ID, created.ID)
	}
}

func TestRunningInstanceRepository_FindActiveByNode_StartingNotYetRunning(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	createTestRunningInstance(t, instances, profile.ID, node.ID, nil) // still "starting"

	_, err := instances.FindActiveByNode(ctx, node.ID)
	if err != ErrRunningInstanceNotFound {
		t.Errorf("FindActiveByNode() error = %v, want ErrRunningInstanceNotFound for a starting (not yet running) instance", err)
	}
}

func TestRunningInstanceRepository_FindActiveByNode_NoneActive(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	instances := NewRunningInstanceRepository(pool)

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	_, err := instances.FindActiveByNode(context.Background(), node.ID)
	if err != ErrRunningInstanceNotFound {
		t.Errorf("FindActiveByNode() error = %v, want ErrRunningInstanceNotFound", err)
	}
}

func TestRunningInstanceRepository_SetStatus_NonTerminal_ActualPortSet(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	port := 8000
	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusRunning, &port, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := instances.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != RunningInstanceStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, RunningInstanceStatusRunning)
	}
	if got.ActualPort == nil || *got.ActualPort != port {
		t.Errorf("ActualPort = %v, want %d", got.ActualPort, port)
	}
	if got.StoppedAt != nil {
		t.Errorf("StoppedAt = %v, want nil for a non-terminal status", got.StoppedAt)
	}
}

func TestRunningInstanceRepository_SetStatus_NilActualPortLeavesExistingValue(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	port := 8000
	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusRunning, &port, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}
	// A later transition (e.g. running -> stopping) that has nothing new
	// to report must not clobber the already-recorded port back to null.
	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusStopping, nil, nil); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := instances.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ActualPort == nil || *got.ActualPort != port {
		t.Errorf("ActualPort = %v, want unchanged %d", got.ActualPort, port)
	}
}

func TestRunningInstanceRepository_SetStatus_Terminal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	created := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	errMsg := "image pull failed"
	if err := instances.SetStatus(ctx, created.ID, RunningInstanceStatusFailed, nil, &errMsg); err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}

	got, err := instances.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.Status != RunningInstanceStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, RunningInstanceStatusFailed)
	}
	if got.StoppedAt == nil {
		t.Error("StoppedAt is nil, want it set for a terminal status")
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != errMsg {
		t.Errorf("ErrorMessage = %v, want %q", got.ErrorMessage, errMsg)
	}
}

func TestRunningInstanceRepository_List(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	a := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)
	b := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)

	got, err := instances.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, inst := range got {
		if inst.ID == a.ID {
			foundA = true
		}
		if inst.ID == b.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %+v, want it to include both %q and %q", got, a.ID, b.ID)
	}
}
