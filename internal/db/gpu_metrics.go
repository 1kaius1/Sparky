// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GPUMetric mirrors one row of the gpu_metrics table - see SCHEMA.md GPU
// metrics. A single point-in-time reading from one physical GPU on one
// node - see Metric's own doc comment for why GPU readings live in a
// separate table from node-level CPU/system-memory readings.
type GPUMetric struct {
	RecordedAt        time.Time
	NodeID            string
	GPUIndex          int
	RunningInstanceID *string
	UtilizationPct    float64
	MemoryUsedMB      float64
	MemoryTotalMB     float64
}

// recentGPUMetricsLimit caps how many of the most recent rows Recent
// returns, across all nodes and GPUs combined - a documented headroom
// assumption (4x metrics' own recentMetricsLimit), not a measured value:
// every node available to this project has exactly one GPU, so actual row
// volume today matches metrics' own window exactly; this only matters once
// a real multi-GPU node exists.
const recentGPUMetricsLimit = 800

// GPUMetricsRepository is the only component that queries the gpu_metrics
// table directly - see CLAUDE.md: the repository layer is the only place
// that accesses the database directly.
type GPUMetricsRepository struct {
	pool *pgxpool.Pool
}

// NewGPUMetricsRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewGPUMetricsRepository(pool *pgxpool.Pool) *GPUMetricsRepository {
	return &GPUMetricsRepository{pool: pool}
}

// Create inserts one GPU telemetry reading. recordedAt is the agent's own
// timestamp, shared across every GPU (and the sibling node-level Metric)
// from the same reading - see agentproto.Telemetry's own doc comment.
// runningInstanceID is nil when the node currently has nothing loaded; it
// deliberately duplicates the sibling metrics row's value rather than
// requiring a join back to it - see the 000023 migration's own doc
// comment.
func (r *GPUMetricsRepository) Create(ctx context.Context, recordedAt time.Time, nodeID string, gpuIndex int, runningInstanceID *string,
	utilizationPct, usedMB, totalMB float64) (*GPUMetric, error) {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO gpu_metrics (recorded_at, node_id, gpu_index, running_instance_id, gpu_utilization_pct, gpu_memory_used_mb, gpu_memory_total_mb)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		recordedAt, nodeID, gpuIndex, runningInstanceID, utilizationPct, usedMB, totalMB)
	if err != nil {
		return nil, fmt.Errorf("create gpu metric for node %s gpu %d: %w", nodeID, gpuIndex, err)
	}

	return &GPUMetric{
		RecordedAt: recordedAt, NodeID: nodeID, GPUIndex: gpuIndex, RunningInstanceID: runningInstanceID,
		UtilizationPct: utilizationPct, MemoryUsedMB: usedMB, MemoryTotalMB: totalMB,
	}, nil
}

const gpuMetricColumns = `recorded_at, node_id, gpu_index, running_instance_id, gpu_utilization_pct, gpu_memory_used_mb, gpu_memory_total_mb`

func scanGPUMetric(row pgx.Row) (*GPUMetric, error) {
	var m GPUMetric
	err := row.Scan(&m.RecordedAt, &m.NodeID, &m.GPUIndex, &m.RunningInstanceID, &m.UtilizationPct, &m.MemoryUsedMB, &m.MemoryTotalMB)
	if err != nil {
		return nil, fmt.Errorf("scan gpu metric: %w", err)
	}
	return &m, nil
}

// LatestByNodeAndGPU returns the single most recent reading for every
// (node, GPU index) pair that has ever reported one - the Metrics page's
// per-GPU summary table. DISTINCT ON (node_id, gpu_index) is served
// directly by the table's own primary key ordering, no extra index needed
// - see the 000023 migration's own doc comment.
func (r *GPUMetricsRepository) LatestByNodeAndGPU(ctx context.Context) ([]*GPUMetric, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT ON (node_id, gpu_index) `+gpuMetricColumns+`
		 FROM gpu_metrics ORDER BY node_id, gpu_index, recorded_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list latest gpu metrics by node and gpu: %w", err)
	}
	defer rows.Close()

	var metrics []*GPUMetric
	for rows.Next() {
		m, err := scanGPUMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("list latest gpu metrics by node and gpu: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list latest gpu metrics by node and gpu: %w", err)
	}
	return metrics, nil
}

// Recent returns the most recent readings across every node/GPU combined,
// up to recentGPUMetricsLimit, most recently recorded first - the GPU
// utilization/memory chart panels' data source. Deliberately a recent
// window, not full historical retention - same reasoning as metrics'
// Recent.
func (r *GPUMetricsRepository) Recent(ctx context.Context) ([]*GPUMetric, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+gpuMetricColumns+`
		 FROM gpu_metrics ORDER BY recorded_at DESC LIMIT $1`, recentGPUMetricsLimit)
	if err != nil {
		return nil, fmt.Errorf("list recent gpu metrics: %w", err)
	}
	defer rows.Close()

	var metrics []*GPUMetric
	for rows.Next() {
		m, err := scanGPUMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("list recent gpu metrics: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent gpu metrics: %w", err)
	}
	return metrics, nil
}
