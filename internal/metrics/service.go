// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics is Telemetry ingestion - see ARCHITECTURE.md's Metrics
// Ingestion & Retention component and CLAUDE.md's repository layout
// ("metrics/ - Telemetry ingestion, retention/downsample, NFS/S3
// export"). This package implements ingestion only - ARCHITECTURE.md's
// downsampling and NFS/S3 export jobs belong to the separate v0.4.0
// Historical metrics milestone, and PLANNING.md's v0.1.0 "Metrics" item
// is explicitly "no historical retention yet".
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// metricsStore is the subset of *db.MetricsRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance.
type metricsStore interface {
	Create(ctx context.Context, recordedAt time.Time, nodeID string, runningInstanceID *string,
		cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64) (*db.Metric, error)
	LatestByNode(ctx context.Context) ([]*db.Metric, error)
	Recent(ctx context.Context) ([]*db.Metric, error)
}

// gpuMetricsStore is the subset of *db.GPUMetricsRepository this package
// needs - a separate interface from metricsStore, one per repository/table,
// matching this codebase's existing convention (e.g. internal/transfers.
// Service composes multiple single-table repo interfaces via separate
// constructor params rather than one merged interface).
type gpuMetricsStore interface {
	Create(ctx context.Context, recordedAt time.Time, nodeID string, gpuIndex int, runningInstanceID *string,
		utilizationPct, usedMB, totalMB float64) (*db.GPUMetric, error)
	LatestByNodeAndGPU(ctx context.Context) ([]*db.GPUMetric, error)
	Recent(ctx context.Context) ([]*db.GPUMetric, error)
}

// instanceLookup is the subset of *db.RunningInstanceRepository this
// package needs, to correlate an incoming reading with whatever Running
// instance (if any) is currently loaded on the reporting node.
type instanceLookup interface {
	FindActiveByNode(ctx context.Context, nodeID string) (*db.RunningInstance, error)
}

// Service is Telemetry ingestion's orchestration layer - unlike
// internal/transfers and internal/lifecycle, there is no RBAC check or
// audit record on the write side: a telemetry push is agent-initiated
// observational data, not a human-actor state-changing action (SCHEMA.md
// Audit log's "action" examples - loaded_model, elevated_user,
// deleted_model_copy - are all administrative actions on domain objects;
// a telemetry reading is neither administrative nor actor-attributable).
// HandleTelemetry is wired in as internal/agentconn's OnMessageFunc.
// ListLatestByNode/ListRecent are the Metrics page's read path, also
// unguarded by RBAC - same reasoning as nodes.Service.ListNodes: viewing
// telemetry is available at the lowest tier (CLAUDE.md Frontend
// Conventions, Metrics' sidebar tier "Read-only").
type Service struct {
	metrics    metricsStore
	gpuMetrics gpuMetricsStore
	instances  instanceLookup
	logger     *log.Logger
}

// NewService constructs a Service. logger is used because
// HandleTelemetry, as an agentconn.OnMessageFunc, has no return value to
// propagate an error through - same reasoning as internal/transfers.Service
// and internal/lifecycle.Service's logger dependency.
func NewService(metrics metricsStore, gpuMetrics gpuMetricsStore, instances instanceLookup, logger *log.Logger) *Service {
	return &Service{metrics: metrics, gpuMetrics: gpuMetrics, instances: instances, logger: logger}
}

// HandleTelemetry implements agentconn.OnMessageFunc for
// agentproto.TypeTelemetry, the only message type this package expects -
// wire it in as the onMessage callback passed to agentconn.NewHandler.
// Every other message type is ignored, matching OnMessageFunc's contract.
//
// nodeID is the sending connection's authenticated identity (from
// agentconn's own handshake), trusted the same way
// internal/transfers.Service.HandleTransferProgress and
// internal/lifecycle.Service.HandleInstanceResult already trust it, not a
// value read out of the payload - agentproto.Telemetry deliberately
// carries no node identity of its own.
//
// Writes the node-level row (metrics) and one row per reported GPU
// (gpu_metrics) as independent best-effort inserts, not inside a
// transaction - matching this path's existing philosophy that one missed
// reading isn't worth failing the rest over (the correlation lookup below
// already treats its own failure this way). A partial tick - the
// node-level row lands but one GPU's insert fails, or vice versa - is a
// self-healing gap the next ~5s tick corrects, not a consistency
// violation.
func (s *Service) HandleTelemetry(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeTelemetry {
		return
	}

	var t agentproto.Telemetry
	if err := env.DecodePayload(&t); err != nil {
		s.logger.Printf("metrics: node %s sent a malformed telemetry payload: %v", nodeID, err)
		return
	}

	// context.Background(), not a request context - this fires from
	// agentconn's readLoop, off the tail of the WebSocket read, not any
	// HTTP request - same reasoning as internal/transfers.Service's
	// HandleTransferProgress.
	ctx := context.Background()

	var runningInstanceID *string
	switch active, err := s.instances.FindActiveByNode(ctx, nodeID); {
	case err == nil:
		runningInstanceID = &active.ID
	case errors.Is(err, db.ErrRunningInstanceNotFound):
		// Nothing currently loaded on this node - runningInstanceID stays
		// nil, a legitimate value (SCHEMA.md Metrics), not an error.
	default:
		// The correlation lookup failing is not a reason to drop the
		// telemetry point itself - log and persist with a nil
		// running_instance_id rather than losing the reading entirely.
		s.logger.Printf("metrics: look up active running instance for node %s: %v", nodeID, err)
	}

	if _, err := s.metrics.Create(ctx, t.RecordedAt, nodeID, runningInstanceID,
		t.CPUUtilizationPct, t.SystemMemoryUsedMB, t.SystemMemoryTotalMB); err != nil {
		s.logger.Printf("metrics: create metric for node %s: %v", nodeID, err)
	}

	for _, g := range t.GPUs {
		if _, err := s.gpuMetrics.Create(ctx, t.RecordedAt, nodeID, g.Index, runningInstanceID,
			g.UtilizationPct, g.MemoryUsedMB, g.MemoryTotalMB); err != nil {
			s.logger.Printf("metrics: create gpu metric for node %s gpu %d: %v", nodeID, g.Index, err)
			continue
		}
	}
}

// ListLatestByNode returns the single most recent reading for every node
// that has ever reported one - see this method's doc comment on the
// Service type for why this is unguarded by RBAC.
func (s *Service) ListLatestByNode(ctx context.Context) ([]*db.Metric, error) {
	metrics, err := s.metrics.LatestByNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("list latest metrics by node: %w", err)
	}
	return metrics, nil
}

// ListRecent returns the most recent readings across every node, up to
// db.MetricsRepository's own recent-window cap - the Metrics page's chart
// data source.
func (s *Service) ListRecent(ctx context.Context) ([]*db.Metric, error) {
	metrics, err := s.metrics.Recent(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent metrics: %w", err)
	}
	return metrics, nil
}

// ListLatestGPUByNode returns the single most recent reading for every
// (node, GPU index) pair that has ever reported one - same unguarded-by-RBAC
// reasoning as ListLatestByNode, see this type's own doc comment.
func (s *Service) ListLatestGPUByNode(ctx context.Context) ([]*db.GPUMetric, error) {
	metrics, err := s.gpuMetrics.LatestByNodeAndGPU(ctx)
	if err != nil {
		return nil, fmt.Errorf("list latest gpu metrics by node and gpu: %w", err)
	}
	return metrics, nil
}

// ListRecentGPU returns the most recent GPU readings across every node/GPU,
// up to db.GPUMetricsRepository's own recent-window cap - the Metrics
// page's GPU utilization/memory chart panels' data source.
func (s *Service) ListRecentGPU(ctx context.Context) ([]*db.GPUMetric, error) {
	metrics, err := s.gpuMetrics.Recent(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent gpu metrics: %w", err)
	}
	return metrics, nil
}
