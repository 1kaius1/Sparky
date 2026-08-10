// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

func TestNodeRepository_Create_DockerGPU(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	admin := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s", t.Name()))
	runtime := ContainerRuntimePodman

	n, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "gpu-host.local", "10.0.0.5",
		NodeTypeDockerGPU, &runtime, 24, 64, &admin.ID)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, n.ID)
	})

	if n.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if n.NodeType != NodeTypeDockerGPU {
		t.Errorf("NodeType = %q, want %q", n.NodeType, NodeTypeDockerGPU)
	}
	if n.ContainerRuntime == nil || *n.ContainerRuntime != ContainerRuntimePodman {
		t.Errorf("ContainerRuntime = %v, want %q", n.ContainerRuntime, ContainerRuntimePodman)
	}
	if n.GPUMemoryGB != 24 || n.CPUMemoryGB != 64 {
		t.Errorf("GPUMemoryGB/CPUMemoryGB = %v/%v, want 24/64", n.GPUMemoryGB, n.CPUMemoryGB)
	}
	if n.AgentStatus != AgentStatusOffline {
		t.Errorf("AgentStatus = %q, want default %q", n.AgentStatus, AgentStatusOffline)
	}
	if n.RegisteredBy == nil || *n.RegisteredBy != admin.ID {
		t.Errorf("RegisteredBy = %v, want %q", n.RegisteredBy, admin.ID)
	}
}

func TestNodeRepository_Create_Spark_NilContainerRuntime(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	// A Spark's unified memory - gpu_memory_gb and cpu_memory_gb equal -
	// is the degenerate case of the two-pool model, not a special case,
	// per SCHEMA.md Nodes.
	n, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-1.local", "10.0.0.6",
		NodeTypeSpark, nil, 128, 128, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, n.ID)
	})

	if n.ContainerRuntime != nil {
		t.Errorf("ContainerRuntime = %v, want nil for a Spark node", *n.ContainerRuntime)
	}
	if n.RegisteredBy != nil {
		t.Errorf("RegisteredBy = %v, want nil (SuperAdmin is not a Users row)", *n.RegisteredBy)
	}
}

func TestNodeRepository_Create_ContainerRuntimeMismatchRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	runtime := ContainerRuntimeDocker
	if _, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-2.local", "10.0.0.7",
		NodeTypeSpark, &runtime, 128, 128, nil); err == nil {
		t.Error("Create() succeeded for a Spark node with a container_runtime set, want the CHECK constraint to reject it")
	}

	if _, err := nodes.Create(ctx, fmt.Sprintf("node-%s-2", t.Name()), "gpu-host-2.local", "10.0.0.8",
		NodeTypeDockerGPU, nil, 24, 64, nil); err == nil {
		t.Error("Create() succeeded for a docker-gpu node with no container_runtime, want the CHECK constraint to reject it")
	}
}

func TestNodeRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	created, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-3.local", "10.0.0.9",
		NodeTypeSpark, nil, 128, 128, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, created.ID)
	})

	got, err := nodes.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("FindByID() = %+v, want ID=%q Name=%q", got, created.ID, created.Name)
	}
}

func TestNodeRepository_FindByID_NotFound(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)

	_, err := nodes.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrNodeNotFound {
		t.Errorf("FindByID() error = %v, want ErrNodeNotFound", err)
	}
}

func TestNodeRepository_List(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	nameA := fmt.Sprintf("node-a-%s", t.Name())
	nameB := fmt.Sprintf("node-b-%s", t.Name())

	a, err := nodes.Create(ctx, nameA, "host-a.local", "10.0.1.1", NodeTypeSpark, nil, 128, 128, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, a.ID)
	})

	b, err := nodes.Create(ctx, nameB, "host-b.local", "10.0.1.2", NodeTypeSpark, nil, 128, 128, nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, b.ID)
	})

	got, err := nodes.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var foundA, foundB bool
	for _, n := range got {
		if n.ID == a.ID {
			foundA = true
		}
		if n.ID == b.ID {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("List() = %d nodes, missing one or both of the two just created", len(got))
	}
}
