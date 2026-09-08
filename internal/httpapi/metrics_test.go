// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// TestBuildMetricsChartData_XIsRealUnixMillis is the regression test for the
// real two-node bug (PLANNING.md's 2026-09-08 Decisions Log entries): each
// chartPoint's X must be the reading's own real Unix-milliseconds timestamp,
// not a pre-formatted string bucketed into a shared category label. Two
// nodes with distinct, non-coincident RecordedAt values must produce two
// distinct, non-coincident X values - the actual server-side contract
// web/static/js/metrics.js's linear time axis depends on to interleave them
// correctly, rather than laying them out as two abutting blocks.
func TestBuildMetricsChartData_XIsRealUnixMillis(t *testing.T) {
	node1Time := time.Date(2026, 9, 8, 11, 36, 25, 0, time.UTC)
	node2Time := time.Date(2026, 9, 8, 11, 36, 39, 0, time.UTC) // deliberately not aligned with node1Time

	recentGPU := []*db.GPUMetric{
		{RecordedAt: node1Time, NodeID: "node-1", GPUIndex: 0, UtilizationPct: 50, MemoryUsedMB: 1000, MemoryTotalMB: 4000},
		{RecordedAt: node2Time, NodeID: "node-2", GPUIndex: 0, UtilizationPct: 75, MemoryUsedMB: 2000, MemoryTotalMB: 4000},
	}
	recentNode := []*db.Metric{
		{RecordedAt: node1Time, NodeID: "node-1", CPUUtilizationPct: 10, SystemMemoryUsedMB: 500, SystemMemoryTotalMB: 8000},
		{RecordedAt: node2Time, NodeID: "node-2", CPUUtilizationPct: 20, SystemMemoryUsedMB: 600, SystemMemoryTotalMB: 8000},
	}
	nodeNames := map[string]string{"node-1": "Spark-1", "node-2": "Spark-2"}

	got := buildMetricsChartData(recentNode, recentGPU, nodeNames)

	wantByLabel := map[string]int64{
		"Spark-1 GPU 0": node1Time.UnixMilli(),
		"Spark-2 GPU 0": node2Time.UnixMilli(),
	}
	if len(got.GPUUtilization) != 2 {
		t.Fatalf("GPUUtilization: got %d series, want 2", len(got.GPUUtilization))
	}
	for _, s := range got.GPUUtilization {
		want, ok := wantByLabel[s.Label]
		if !ok {
			t.Fatalf("GPUUtilization: unexpected series label %q", s.Label)
		}
		if len(s.Points) != 1 {
			t.Fatalf("GPUUtilization series %q: got %d points, want 1", s.Label, len(s.Points))
		}
		if s.Points[0].X != want {
			t.Errorf("GPUUtilization series %q: X = %d, want %d (real RecordedAt.UnixMilli(), not a formatted/bucketed string)", s.Label, s.Points[0].X, want)
		}
	}

	// Same real-timestamp contract for the node-level (not GPU-indexed) CPU
	// series - a separate code path in buildMetricsChartData from the GPU
	// one above, so it needs its own assertion rather than being assumed
	// to share the fix.
	wantCPUByLabel := map[string]int64{
		"Spark-1": node1Time.UnixMilli(),
		"Spark-2": node2Time.UnixMilli(),
	}
	if len(got.CPU) != 2 {
		t.Fatalf("CPU: got %d series, want 2", len(got.CPU))
	}
	for _, s := range got.CPU {
		want, ok := wantCPUByLabel[s.Label]
		if !ok {
			t.Fatalf("CPU: unexpected series label %q", s.Label)
		}
		if len(s.Points) != 1 {
			t.Fatalf("CPU series %q: got %d points, want 1", s.Label, len(s.Points))
		}
		if s.Points[0].X != want {
			t.Errorf("CPU series %q: X = %d, want %d", s.Label, s.Points[0].X, want)
		}
	}

	// The two nodes' real timestamps are, by construction, not equal - if
	// they ever collapsed to the same X (e.g. a regression back to a
	// coarser format like whole-second or formatted-string truncation
	// bucketing), that would silently reintroduce a coarser version of the
	// same category-collision class of bug this fix closed.
	if node1Time.UnixMilli() == node2Time.UnixMilli() {
		t.Fatal("test fixture bug: node1Time and node2Time must be distinct")
	}
}

// TestBuildMetricsChartData_OrderedChronologically confirms points within a
// series still come back oldest-first (unchanged by the X-field type
// change) - metrics.js's decimation/right-edge assumptions depend on this.
func TestBuildMetricsChartData_OrderedChronologically(t *testing.T) {
	t1 := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	t2 := t1.Add(5 * time.Second)
	t3 := t1.Add(10 * time.Second)

	recentGPU := []*db.GPUMetric{
		{RecordedAt: t1, NodeID: "node-1", GPUIndex: 0, UtilizationPct: 1},
		{RecordedAt: t2, NodeID: "node-1", GPUIndex: 0, UtilizationPct: 2},
		{RecordedAt: t3, NodeID: "node-1", GPUIndex: 0, UtilizationPct: 3},
	}
	got := buildMetricsChartData(nil, recentGPU, map[string]string{"node-1": "Spark-1"})

	if len(got.GPUUtilization) != 1 || len(got.GPUUtilization[0].Points) != 3 {
		t.Fatalf("unexpected shape: %+v", got.GPUUtilization)
	}
	points := got.GPUUtilization[0].Points
	if points[0].X != t1.UnixMilli() || points[1].X != t2.UnixMilli() || points[2].X != t3.UnixMilli() {
		t.Errorf("points not in chronological order: got X values %d, %d, %d", points[0].X, points[1].X, points[2].X)
	}
}
