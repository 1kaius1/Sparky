// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Metric mirrors one row of the metrics table - see SCHEMA.md Metrics. A
// single point-in-time hardware reading from one node.
type Metric struct {
	RecordedAt          time.Time
	NodeID              string
	RunningInstanceID   *string
	GPUUtilizationPct   float64
	GPUMemoryUsedMB     float64
	GPUMemoryTotalMB    float64
	CPUUtilizationPct   float64
	SystemMemoryUsedMB  float64
	SystemMemoryTotalMB float64
}

// recentMetricsLimit caps how many of the most recent rows Recent
// returns, across all nodes combined - a recent-window view for the
// Metrics page's chart, not full historical retention (that remains the
// separate v0.4.0 Historical metrics milestone - see this package's own
// doc comment).
const recentMetricsLimit = 200

// MetricsRepository is the only component that queries the metrics table
// directly - see CLAUDE.md: the repository layer is the only place that
// accesses the database directly.
type MetricsRepository struct {
	pool *pgxpool.Pool
}

// NewMetricsRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewMetricsRepository(pool *pgxpool.Pool) *MetricsRepository {
	return &MetricsRepository{pool: pool}
}

// Create inserts one telemetry reading. recordedAt is the agent's own
// timestamp (agentproto.Telemetry.RecordedAt), not a database-assigned
// one - see that field's doc comment for why. runningInstanceID is nil
// when the node currently has nothing loaded.
func (r *MetricsRepository) Create(ctx context.Context, recordedAt time.Time, nodeID string, runningInstanceID *string,
	gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB, cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64) (*Metric, error) {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO metrics (recorded_at, node_id, running_instance_id, gpu_utilization_pct, gpu_memory_used_mb,
			gpu_memory_total_mb, cpu_utilization_pct, system_memory_used_mb, system_memory_total_mb)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		recordedAt, nodeID, runningInstanceID, gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB,
		cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB)
	if err != nil {
		return nil, fmt.Errorf("create metric for node %s: %w", nodeID, err)
	}

	return &Metric{
		RecordedAt: recordedAt, NodeID: nodeID, RunningInstanceID: runningInstanceID,
		GPUUtilizationPct: gpuUtilizationPct, GPUMemoryUsedMB: gpuMemoryUsedMB, GPUMemoryTotalMB: gpuMemoryTotalMB,
		CPUUtilizationPct: cpuUtilizationPct, SystemMemoryUsedMB: systemMemoryUsedMB, SystemMemoryTotalMB: systemMemoryTotalMB,
	}, nil
}

const metricColumns = `recorded_at, node_id, running_instance_id, gpu_utilization_pct, gpu_memory_used_mb, gpu_memory_total_mb, cpu_utilization_pct, system_memory_used_mb, system_memory_total_mb`

func scanMetric(row pgx.Row) (*Metric, error) {
	var m Metric
	err := row.Scan(&m.RecordedAt, &m.NodeID, &m.RunningInstanceID, &m.GPUUtilizationPct, &m.GPUMemoryUsedMB,
		&m.GPUMemoryTotalMB, &m.CPUUtilizationPct, &m.SystemMemoryUsedMB, &m.SystemMemoryTotalMB)
	if err != nil {
		return nil, fmt.Errorf("scan metric: %w", err)
	}
	return &m, nil
}

// LatestByNode returns the single most recent reading for every node that
// has ever reported one - the Metrics page's per-node summary table.
// Nodes that have never reported telemetry (no agent connected yet, or
// telemetry hasn't been wired to persist in this environment) simply
// don't appear - the caller resolves node names by ID and only needs to
// handle "no row for this node" as an empty state, not a special case.
func (r *MetricsRepository) LatestByNode(ctx context.Context) ([]*Metric, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT ON (node_id) `+metricColumns+`
		 FROM metrics ORDER BY node_id, recorded_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list latest metrics by node: %w", err)
	}
	defer rows.Close()

	var metrics []*Metric
	for rows.Next() {
		m, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("list latest metrics by node: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list latest metrics by node: %w", err)
	}
	return metrics, nil
}

// Recent returns the most recent readings across every node combined, up
// to recentMetricsLimit, most recently recorded first - the Metrics
// page's chart data source. Deliberately a recent window, not full
// historical retention - see that constant's own doc comment.
func (r *MetricsRepository) Recent(ctx context.Context) ([]*Metric, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+metricColumns+`
		 FROM metrics ORDER BY recorded_at DESC LIMIT $1`, recentMetricsLimit)
	if err != nil {
		return nil, fmt.Errorf("list recent metrics: %w", err)
	}
	defer rows.Close()

	var metrics []*Metric
	for rows.Next() {
		m, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("list recent metrics: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent metrics: %w", err)
	}
	return metrics, nil
}
