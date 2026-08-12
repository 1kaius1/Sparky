// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMetricsRepository_Create_NoRunningInstance(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	metricsRepo := NewMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	recordedAt := time.Now().UTC().Truncate(time.Microsecond)

	m, err := metricsRepo.Create(ctx, recordedAt, node.ID, nil, 45.5, 8192, 24576, 12.3, 4096, 16384)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt)
	})

	if m.NodeID != node.ID {
		t.Errorf("NodeID = %q, want %q", m.NodeID, node.ID)
	}
	if m.RunningInstanceID != nil {
		t.Errorf("RunningInstanceID = %v, want nil", *m.RunningInstanceID)
	}
	if m.GPUUtilizationPct != 45.5 || m.GPUMemoryUsedMB != 8192 || m.GPUMemoryTotalMB != 24576 {
		t.Errorf("GPU fields = %v/%v/%v, want 45.5/8192/24576", m.GPUUtilizationPct, m.GPUMemoryUsedMB, m.GPUMemoryTotalMB)
	}
	if m.CPUUtilizationPct != 12.3 || m.SystemMemoryUsedMB != 4096 || m.SystemMemoryTotalMB != 16384 {
		t.Errorf("CPU/memory fields = %v/%v/%v, want 12.3/4096/16384", m.CPUUtilizationPct, m.SystemMemoryUsedMB, m.SystemMemoryTotalMB)
	}

	// Confirm it actually landed in the database, not just the returned
	// struct - a direct query, bypassing the repository, same discipline
	// as ModelTransferRepository's CHECK-constraint tests.
	var gotUsed float64
	err = pool.QueryRow(ctx, `SELECT gpu_memory_used_mb FROM metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt).Scan(&gotUsed)
	if err != nil {
		t.Fatalf("verify persisted row: %v", err)
	}
	if gotUsed != 8192 {
		t.Errorf("persisted gpu_memory_used_mb = %v, want 8192", gotUsed)
	}
}

func TestMetricsRepository_Create_WithRunningInstance(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	metricsRepo := NewMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	inst := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)
	recordedAt := time.Now().UTC().Truncate(time.Microsecond)

	m, err := metricsRepo.Create(ctx, recordedAt, node.ID, &inst.ID, 10, 1024, 24576, 5, 1024, 16384)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt)
	})

	if m.RunningInstanceID == nil || *m.RunningInstanceID != inst.ID {
		t.Errorf("RunningInstanceID = %v, want %q", m.RunningInstanceID, inst.ID)
	}
}

func TestMetricsRepository_Create_UnknownRunningInstanceRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	metricsRepo := NewMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	bogus := "00000000-0000-0000-0000-000000000000"

	if _, err := metricsRepo.Create(ctx, time.Now(), node.ID, &bogus, 0, 0, 1, 0, 0, 1); err == nil {
		t.Error("Create() succeeded with a running_instance_id that doesn't exist, want the FK to reject it")
	}
}
