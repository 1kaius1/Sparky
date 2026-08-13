// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// assertJSONEqual compares got and want by decoded value, not raw bytes -
// Postgres's jsonb column reformats on storage (e.g. adds a space after
// each ":"), so a byte-for-byte comparison of what was written vs. what
// comes back would fail even when the two are the same JSON value.
func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("EngineParams = %s, want %s", got, want)
	}
}

// createTestNode creates a throwaway single-node-eligible node to
// satisfy model_profiles.target_node_id's foreign key, cleaned up after
// the test. Registered after any test user the caller also created, so
// t.Cleanup's LIFO ordering deletes this node before that user - see
// createTestUser.
func createTestNode(t *testing.T, nodes *NodeRepository, name string) *Node {
	t.Helper()
	n, err := nodes.Create(context.Background(), name, name+".local", "10.0.9.1",
		RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
	if err != nil {
		t.Fatalf("create test node: %v", err)
	}
	t.Cleanup(func() {
		_, _ = nodes.pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, n.ID)
	})
	return n
}

func TestProfileRepository_Create(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	creator := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	params := json.RawMessage(`{"tensor_parallel_size":1}`)
	requiredMemory := 8.0
	p, err := profiles.Create(ctx, fmt.Sprintf("profile-%s", t.Name()), "Qwen/Qwen2.5-0.5B-Instruct",
		ProfileEngineVLLM, params, true, &requiredMemory, node.ID, 8000, &creator.ID)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, p.ID)
	})

	if p.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if p.EngineType != ProfileEngineVLLM {
		t.Errorf("EngineType = %q, want %q", p.EngineType, ProfileEngineVLLM)
	}
	assertJSONEqual(t, p.EngineParams, params)
	if !p.RequiresFullGPUResidency {
		t.Error("RequiresFullGPUResidency = false, want true")
	}
	if p.RequiredMemoryGB == nil || *p.RequiredMemoryGB != 8.0 {
		t.Errorf("RequiredMemoryGB = %v, want 8.0", p.RequiredMemoryGB)
	}
	if p.Topology != ProfileTopologySingleNode {
		t.Errorf("Topology = %q, want %q (the column default)", p.Topology, ProfileTopologySingleNode)
	}
	if p.TargetNodeID == nil || *p.TargetNodeID != node.ID {
		t.Errorf("TargetNodeID = %v, want %q", p.TargetNodeID, node.ID)
	}
	if p.Port != 8000 {
		t.Errorf("Port = %d, want 8000", p.Port)
	}
	if p.CreatedBy == nil || *p.CreatedBy != creator.ID {
		t.Errorf("CreatedBy = %v, want %q", p.CreatedBy, creator.ID)
	}
	if p.UpdatedBy != nil {
		t.Errorf("UpdatedBy = %v, want nil for a freshly created profile", p.UpdatedBy)
	}
}

func TestProfileRepository_Create_SuperAdmin_NilCreatedBy(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	p, err := profiles.Create(ctx, fmt.Sprintf("profile-%s", t.Name()), "TinyLlama/TinyLlama-1.1B-Chat-v1.0-GGUF",
		ProfileEngineLlamaCPP, json.RawMessage(`{}`), false, nil, node.ID, 8001, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, p.ID)
	})

	if p.CreatedBy != nil {
		t.Errorf("CreatedBy = %v, want nil (SuperAdmin is not a Users row)", *p.CreatedBy)
	}
	if p.RequiredMemoryGB != nil {
		t.Errorf("RequiredMemoryGB = %v, want nil when unset", *p.RequiredMemoryGB)
	}
}

func TestProfileRepository_Create_DuplicateName_RejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	name := fmt.Sprintf("profile-%s", t.Name())

	p, err := profiles.Create(ctx, name, "model-a", ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, p.ID)
	})

	if _, err := profiles.Create(ctx, name, "model-b", ProfileEngineLlamaCPP, json.RawMessage(`{}`), false, nil, node.ID, 8001, nil); err == nil {
		t.Error("Create() succeeded for a duplicate name, want the UNIQUE constraint to reject it")
	}
}

func TestProfileRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created, err := profiles.Create(ctx, fmt.Sprintf("profile-%s", t.Name()), "model-a", ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, created.ID)
	})

	got, err := profiles.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("FindByID() = %+v, want ID=%q Name=%q", got, created.ID, created.Name)
	}
}

func TestProfileRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	profiles := NewProfileRepository(pool)

	_, err := profiles.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrProfileNotFound {
		t.Errorf("FindByID() error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileRepository_List(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	nameA := fmt.Sprintf("profile-a-%s", t.Name())
	nameB := fmt.Sprintf("profile-b-%s", t.Name())

	a, err := profiles.Create(ctx, nameA, "model-a", ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, a.ID)
	})

	b, err := profiles.Create(ctx, nameB, "model-b", ProfileEngineLlamaCPP, json.RawMessage(`{}`), false, nil, node.ID, 8001, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, b.ID)
	})

	got, err := profiles.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, p := range got {
		if p.ID == a.ID {
			foundA = true
		}
		if p.ID == b.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d profiles, missing one or both of the two just created", len(got))
	}
}

func TestProfileRepository_Update(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	updater := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	otherNode := createTestNode(t, nodes, fmt.Sprintf("node-b-%s", t.Name()))

	created, err := profiles.Create(ctx, fmt.Sprintf("profile-%s", t.Name()), "model-a", ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE id = $1`, created.ID)
	})

	newParams := json.RawMessage(`{"n_gpu_layers":0}`)
	newMemory := 4.0
	updated, err := profiles.Update(ctx, created.ID, "renamed-profile", "model-b", ProfileEngineLlamaCPP,
		newParams, false, &newMemory, otherNode.ID, 8080, &updater.ID)
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}

	if updated.Name != "renamed-profile" {
		t.Errorf("Name = %q, want %q", updated.Name, "renamed-profile")
	}
	if updated.ModelRef != "model-b" {
		t.Errorf("ModelRef = %q, want %q", updated.ModelRef, "model-b")
	}
	if updated.EngineType != ProfileEngineLlamaCPP {
		t.Errorf("EngineType = %q, want %q", updated.EngineType, ProfileEngineLlamaCPP)
	}
	assertJSONEqual(t, updated.EngineParams, newParams)
	if updated.RequiresFullGPUResidency {
		t.Error("RequiresFullGPUResidency = true, want false")
	}
	if updated.RequiredMemoryGB == nil || *updated.RequiredMemoryGB != 4.0 {
		t.Errorf("RequiredMemoryGB = %v, want 4.0", updated.RequiredMemoryGB)
	}
	if updated.TargetNodeID == nil || *updated.TargetNodeID != otherNode.ID {
		t.Errorf("TargetNodeID = %v, want %q", updated.TargetNodeID, otherNode.ID)
	}
	if updated.Port != 8080 {
		t.Errorf("Port = %d, want 8080", updated.Port)
	}
	if updated.UpdatedBy == nil || *updated.UpdatedBy != updater.ID {
		t.Errorf("UpdatedBy = %v, want %q", updated.UpdatedBy, updater.ID)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Errorf("UpdatedAt = %v, want after CreatedAt = %v", updated.UpdatedAt, updated.CreatedAt)
	}
}

func TestProfileRepository_Update_NotFound(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	_, err := profiles.Update(ctx, "00000000-0000-0000-0000-000000000000", "x", "x", ProfileEngineVLLM,
		json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != ErrProfileNotFound {
		t.Errorf("Update() error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileRepository_Delete(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	created, err := profiles.Create(ctx, fmt.Sprintf("profile-%s", t.Name()), "model-a", ProfileEngineVLLM, json.RawMessage(`{}`), true, nil, node.ID, 8000, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := profiles.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	if _, err := profiles.FindByID(ctx, created.ID); err != ErrProfileNotFound {
		t.Errorf("FindByID() after Delete() error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileRepository_Delete_NotFound(t *testing.T) {
	pool := newTestPool(t)
	profiles := NewProfileRepository(pool)

	err := profiles.Delete(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrProfileNotFound {
		t.Errorf("Delete() error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileRepository_Create_ClusteredTopologyRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	// Bypasses ProfileRepository.Create (which never sets topology
	// explicitly) to confirm the model_profiles_single_node_only CHECK
	// constraint itself rejects a clustered row, not just that the Go
	// API happens to never construct one.
	_, err := pool.Exec(ctx,
		`INSERT INTO model_profiles (name, model_ref, engine_type, requires_full_gpu_residency, topology, target_node_id, port)
		 VALUES ($1, 'model-a', 'vllm', true, 'clustered', $2, 8000)`,
		fmt.Sprintf("profile-%s", t.Name()), node.ID)
	if err == nil {
		t.Error("INSERT succeeded with topology='clustered', want the CHECK constraint to reject it")
	}
}

func TestProfileRepository_Create_NullTargetNodeRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO model_profiles (name, model_ref, engine_type, requires_full_gpu_residency, port)
		 VALUES ($1, 'model-a', 'vllm', true, 8000)`,
		fmt.Sprintf("profile-%s", t.Name()))
	if err == nil {
		t.Error("INSERT succeeded with a null target_node_id, want the CHECK constraint to reject it")
	}
}
