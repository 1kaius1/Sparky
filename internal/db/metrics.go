// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

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

// MetricsRepository is the only component that queries the metrics table
// directly - see CLAUDE.md: the repository layer is the only place that
// accesses the database directly. Write-only for now, same as
// AuditRepository - nothing yet reads metrics back (no dashboard, no
// retention/downsample job - both later work, see PLANNING.md's Metrics
// milestone item and the separate v0.4.0 Historical metrics milestone).
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
