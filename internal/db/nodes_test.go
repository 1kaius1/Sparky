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

	n, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "gpu-host.local", "10.0.0.5",
		RuntimeBackendPodman, 24, 64, &admin.ID, "test-bearer-token-hash")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, n.ID)
	})

	if n.ID == "" {
		t.Error("ID is empty, want a generated uuid")
	}
	if n.RuntimeBackend != RuntimeBackendPodman {
		t.Errorf("RuntimeBackend = %q, want %q", n.RuntimeBackend, RuntimeBackendPodman)
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

	// bearer_token_hash is deliberately excluded from nodeColumns/Node -
	// confirm the value Create actually persisted it by reading the raw
	// column directly, not just that Create() didn't error.
	var storedHash string
	if err := pool.QueryRow(ctx, `SELECT bearer_token_hash FROM nodes WHERE id = $1`, n.ID).Scan(&storedHash); err != nil {
		t.Fatalf("query bearer_token_hash: %v", err)
	}
	if storedHash != "test-bearer-token-hash" {
		t.Errorf("stored bearer_token_hash = %q, want %q", storedHash, "test-bearer-token-hash")
	}
}

func TestNodeRepository_Create_BareMetal(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	// A Spark's unified memory - gpu_memory_gb and cpu_memory_gb equal -
	// is the degenerate case of the two-pool model, not a special case,
	// per SCHEMA.md Nodes. Bare-metal itself is also not Spark-specific -
	// it's for any host without viable GPU passthrough (e.g. a
	// single-GPU workstation already using that GPU for its own host
	// session) - see PLANNING.md's Decisions Log.
	n, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-1.local", "10.0.0.6",
		RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, n.ID)
	})

	if n.RuntimeBackend != RuntimeBackendBareMetal {
		t.Errorf("RuntimeBackend = %q, want %q", n.RuntimeBackend, RuntimeBackendBareMetal)
	}
	if n.RegisteredBy != nil {
		t.Errorf("RegisteredBy = %v, want nil (SuperAdmin is not a Users row)", *n.RegisteredBy)
	}
}

func TestNodeRepository_FindByID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	created, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-3.local", "10.0.0.9",
		RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
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

	a, err := nodes.Create(ctx, nameA, "host-a.local", "10.0.1.1", RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, a.ID)
	})

	b, err := nodes.Create(ctx, nameB, "host-b.local", "10.0.1.2", RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
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

func TestNodeRepository_FindCredentialByName(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	name := fmt.Sprintf("node-%s", t.Name())
	created, err := nodes.Create(ctx, name, "spark-4.local", "10.0.0.10", RuntimeBackendBareMetal, 128, 128, nil, "cred-test-hash")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, created.ID)
	})

	cred, err := nodes.FindCredentialByName(ctx, name)
	if err != nil {
		t.Fatalf("FindCredentialByName() error: %v", err)
	}
	if cred.Node.ID != created.ID {
		t.Errorf("Node.ID = %q, want %q", cred.Node.ID, created.ID)
	}
	if cred.BearerTokenHash != "cred-test-hash" {
		t.Errorf("BearerTokenHash = %q, want %q", cred.BearerTokenHash, "cred-test-hash")
	}
}

func TestNodeRepository_FindCredentialByName_NotFound(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)

	_, err := nodes.FindCredentialByName(context.Background(), "no-such-node")
	if err != ErrNodeNotFound {
		t.Errorf("FindCredentialByName() error = %v, want ErrNodeNotFound", err)
	}
}

func TestNodeRepository_SetAgentStatus(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	ctx := context.Background()

	created, err := nodes.Create(ctx, fmt.Sprintf("node-%s", t.Name()), "spark-5.local", "10.0.0.11",
		RuntimeBackendBareMetal, 128, 128, nil, "test-bearer-token-hash")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM nodes WHERE id = $1`, created.ID)
	})
	if created.LastHeartbeatAt != nil {
		t.Fatalf("LastHeartbeatAt = %v, want nil before any SetAgentStatus call", created.LastHeartbeatAt)
	}

	if err := nodes.SetAgentStatus(ctx, created.ID, AgentStatusOnline, true); err != nil {
		t.Fatalf("SetAgentStatus(online, bumpHeartbeat=true) error: %v", err)
	}
	online, err := nodes.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if online.AgentStatus != AgentStatusOnline {
		t.Errorf("AgentStatus = %q, want %q", online.AgentStatus, AgentStatusOnline)
	}
	if online.LastHeartbeatAt == nil {
		t.Error("LastHeartbeatAt is nil, want it set when bumpHeartbeat=true")
	}
	firstHeartbeat := online.LastHeartbeatAt

	if err := nodes.SetAgentStatus(ctx, created.ID, AgentStatusOffline, false); err != nil {
		t.Fatalf("SetAgentStatus(offline, bumpHeartbeat=false) error: %v", err)
	}
	offline, err := nodes.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if offline.AgentStatus != AgentStatusOffline {
		t.Errorf("AgentStatus = %q, want %q", offline.AgentStatus, AgentStatusOffline)
	}
	if offline.LastHeartbeatAt == nil || !offline.LastHeartbeatAt.Equal(*firstHeartbeat) {
		t.Errorf("LastHeartbeatAt changed to %v on a bumpHeartbeat=false call, want it unchanged from %v", offline.LastHeartbeatAt, firstHeartbeat)
	}
}
