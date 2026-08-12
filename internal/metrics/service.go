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
	"log"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// metricsStore is the subset of *db.MetricsRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance.
type metricsStore interface {
	Create(ctx context.Context, recordedAt time.Time, nodeID string, runningInstanceID *string,
		gpuUtilizationPct, gpuMemoryUsedMB, gpuMemoryTotalMB, cpuUtilizationPct, systemMemoryUsedMB, systemMemoryTotalMB float64) (*db.Metric, error)
}

// instanceLookup is the subset of *db.RunningInstanceRepository this
// package needs, to correlate an incoming reading with whatever Running
// instance (if any) is currently loaded on the reporting node.
type instanceLookup interface {
	FindActiveByNode(ctx context.Context, nodeID string) (*db.RunningInstance, error)
}

// Service is Telemetry ingestion's orchestration layer - unlike
// internal/transfers and internal/lifecycle, there is no RBAC check or
// audit record here: a telemetry push is agent-initiated observational
// data, not a human-actor state-changing action (SCHEMA.md Audit log's
// "action" examples - loaded_model, elevated_user, deleted_model_copy -
// are all administrative actions on domain objects; a telemetry reading
// is neither administrative nor actor-attributable). Its only job is
// HandleTelemetry, wired in as internal/agentconn's OnMessageFunc.
type Service struct {
	metrics   metricsStore
	instances instanceLookup
	logger    *log.Logger
}

// NewService constructs a Service. logger is used because
// HandleTelemetry, as an agentconn.OnMessageFunc, has no return value to
// propagate an error through - same reasoning as internal/transfers.Service
// and internal/lifecycle.Service's logger dependency.
func NewService(metrics metricsStore, instances instanceLookup, logger *log.Logger) *Service {
	return &Service{metrics: metrics, instances: instances, logger: logger}
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
		t.GPUUtilizationPct, t.GPUMemoryUsedMB, t.GPUMemoryTotalMB,
		t.CPUUtilizationPct, t.SystemMemoryUsedMB, t.SystemMemoryTotalMB); err != nil {
		s.logger.Printf("metrics: create metric for node %s: %v", nodeID, err)
	}
}
