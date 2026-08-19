// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestGPUMetricsRepository_Create_NoRunningInstance(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	recordedAt := time.Now().UTC().Truncate(time.Microsecond)

	m, err := gpuMetricsRepo.Create(ctx, recordedAt, node.ID, 0, nil, 45.5, 8192, 24576)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gpu_metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt)
	})

	if m.NodeID != node.ID {
		t.Errorf("NodeID = %q, want %q", m.NodeID, node.ID)
	}
	if m.GPUIndex != 0 {
		t.Errorf("GPUIndex = %v, want 0", m.GPUIndex)
	}
	if m.RunningInstanceID != nil {
		t.Errorf("RunningInstanceID = %v, want nil", *m.RunningInstanceID)
	}
	if m.UtilizationPct != 45.5 || m.MemoryUsedMB != 8192 || m.MemoryTotalMB != 24576 {
		t.Errorf("GPU fields = %v/%v/%v, want 45.5/8192/24576", m.UtilizationPct, m.MemoryUsedMB, m.MemoryTotalMB)
	}

	// Confirm it actually landed in the database, not just the returned
	// struct - same discipline as MetricsRepository's own tests.
	var gotUsed float64
	err = pool.QueryRow(ctx, `SELECT gpu_memory_used_mb FROM gpu_metrics WHERE node_id = $1 AND gpu_index = $2 AND recorded_at = $3`,
		node.ID, 0, recordedAt).Scan(&gotUsed)
	if err != nil {
		t.Fatalf("verify persisted row: %v", err)
	}
	if gotUsed != 8192 {
		t.Errorf("persisted gpu_memory_used_mb = %v, want 8192", gotUsed)
	}
}

func TestGPUMetricsRepository_Create_WithRunningInstance(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	profiles := NewProfileRepository(pool)
	instances := NewRunningInstanceRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	profile := createTestProfile(t, profiles, node.ID)
	inst := createTestRunningInstance(t, instances, profile.ID, node.ID, nil)
	recordedAt := time.Now().UTC().Truncate(time.Microsecond)

	m, err := gpuMetricsRepo.Create(ctx, recordedAt, node.ID, 0, &inst.ID, 10, 1024, 24576)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gpu_metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt)
	})

	if m.RunningInstanceID == nil || *m.RunningInstanceID != inst.ID {
		t.Errorf("RunningInstanceID = %v, want %q", m.RunningInstanceID, inst.ID)
	}
}

func TestGPUMetricsRepository_Create_UnknownRunningInstanceRejectedByDatabase(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	bogus := "00000000-0000-0000-0000-000000000000"

	if _, err := gpuMetricsRepo.Create(ctx, time.Now(), node.ID, 0, &bogus, 0, 0, 1); err == nil {
		t.Error("Create() succeeded with a running_instance_id that doesn't exist, want the FK to reject it")
	}
}

// TestGPUMetricsRepository_Create_MultipleGPUIndicesForSameNode proves the
// primary key's third column (gpu_index) actually disambiguates - two
// GPU rows for the same node at the same recorded_at both persist
// independently instead of colliding. This is a synthetic multi-GPU
// scenario: no real multi-GPU node exists anywhere this project develops
// against (the RTX 4090 laptop, the Dell Precision RTX 3080Ti both have
// exactly one GPU), so this confirms the schema/write path handles more
// than one GPU per tick correctly, not that a real nvidia-smi invocation
// actually emits CSV in this shape - see PLANNING.md Known Issues.
func TestGPUMetricsRepository_Create_MultipleGPUIndicesForSameNode(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	recordedAt := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := gpuMetricsRepo.Create(ctx, recordedAt, node.ID, 0, nil, 40, 4096, 24576); err != nil {
		t.Fatalf("Create() gpu 0 error: %v", err)
	}
	if _, err := gpuMetricsRepo.Create(ctx, recordedAt, node.ID, 1, nil, 60, 6144, 24576); err != nil {
		t.Fatalf("Create() gpu 1 error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gpu_metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt)
	})

	var count int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM gpu_metrics WHERE node_id = $1 AND recorded_at = $2`, node.ID, recordedAt).Scan(&count)
	if err != nil {
		t.Fatalf("count persisted rows: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (one row per gpu_index, no collision)", count)
	}
}

func TestGPUMetricsRepository_LatestByNodeAndGPU(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	older := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	newer := time.Now().UTC().Truncate(time.Microsecond)

	// gpu_index 0: an older and a newer reading - LatestByNodeAndGPU
	// should return only the newer one.
	if _, err := gpuMetricsRepo.Create(ctx, older, node.ID, 0, nil, 10, 1024, 24576); err != nil {
		t.Fatalf("Create() gpu 0 older error: %v", err)
	}
	if _, err := gpuMetricsRepo.Create(ctx, newer, node.ID, 0, nil, 90, 20480, 24576); err != nil {
		t.Fatalf("Create() gpu 0 newer error: %v", err)
	}
	// gpu_index 1: a single reading - proves DISTINCT ON keys on
	// (node_id, gpu_index), not just node_id, so both indices appear.
	if _, err := gpuMetricsRepo.Create(ctx, newer, node.ID, 1, nil, 50, 2048, 24576); err != nil {
		t.Fatalf("Create() gpu 1 error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gpu_metrics WHERE node_id = $1`, node.ID)
	})

	got, err := gpuMetricsRepo.LatestByNodeAndGPU(ctx)
	if err != nil {
		t.Fatalf("LatestByNodeAndGPU() error: %v", err)
	}

	byIndex := map[int]*GPUMetric{}
	for _, m := range got {
		if m.NodeID == node.ID {
			byIndex[m.GPUIndex] = m
		}
	}
	if len(byIndex) != 2 {
		t.Fatalf("got %d rows for node %s, want 2 (one per gpu_index)", len(byIndex), node.ID)
	}
	if !byIndex[0].RecordedAt.Equal(newer) || byIndex[0].UtilizationPct != 90 {
		t.Errorf("gpu 0 = %+v, want the newer reading (util 90)", byIndex[0])
	}
	if byIndex[1].UtilizationPct != 50 {
		t.Errorf("gpu 1 UtilizationPct = %v, want 50", byIndex[1].UtilizationPct)
	}
}

func TestGPUMetricsRepository_Recent_OrderedMostRecentFirst(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	gpuMetricsRepo := NewGPUMetricsRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	older := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	newer := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := gpuMetricsRepo.Create(ctx, older, node.ID, 0, nil, 10, 1024, 24576); err != nil {
		t.Fatalf("Create() older error: %v", err)
	}
	if _, err := gpuMetricsRepo.Create(ctx, newer, node.ID, 0, nil, 90, 20480, 24576); err != nil {
		t.Fatalf("Create() newer error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gpu_metrics WHERE node_id = $1`, node.ID)
	})

	got, err := gpuMetricsRepo.Recent(ctx)
	if err != nil {
		t.Fatalf("Recent() error: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("len(got) = %d, want at least 2", len(got))
	}
	var newerIdx, olderIdx = -1, -1
	for i, m := range got {
		if m.NodeID != node.ID {
			continue
		}
		if m.RecordedAt.Equal(newer) {
			newerIdx = i
		}
		if m.RecordedAt.Equal(older) {
			olderIdx = i
		}
	}
	if newerIdx == -1 || olderIdx == -1 {
		t.Fatalf("Recent() did not include both of this test's readings for node %s", node.ID)
	}
	if newerIdx > olderIdx {
		t.Errorf("newer reading at index %d, older at index %d - want most-recently-recorded first", newerIdx, olderIdx)
	}
}
