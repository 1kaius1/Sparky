// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunningInstanceStatus mirrors the running_instance_status Postgres enum
// - see migrations/000010_create_running_instances.up.sql and SCHEMA.md
// Running instances.
type RunningInstanceStatus string

const (
	RunningInstanceStatusStarting RunningInstanceStatus = "starting"
	RunningInstanceStatusRunning  RunningInstanceStatus = "running"
	RunningInstanceStatusStopping RunningInstanceStatus = "stopping"
	RunningInstanceStatusStopped  RunningInstanceStatus = "stopped"
	RunningInstanceStatusFailed   RunningInstanceStatus = "failed"
)

// isTerminalRunningInstanceStatus reports whether status ends a running
// instance's lifecycle - SetStatus uses this to decide whether to also
// stamp stopped_at, same pattern as ModelTransferRepository.SetStatus.
func isTerminalRunningInstanceStatus(status RunningInstanceStatus) bool {
	return status == RunningInstanceStatusStopped || status == RunningInstanceStatusFailed
}

// InstanceHealthStatus mirrors the instance_health_status Postgres enum.
type InstanceHealthStatus string

const (
	InstanceHealthHealthy   InstanceHealthStatus = "healthy"
	InstanceHealthUnhealthy InstanceHealthStatus = "unhealthy"
	InstanceHealthUnknown   InstanceHealthStatus = "unknown"
)

// RunningInstance mirrors the running_instances table - see SCHEMA.md
// Running instances. Live lifecycle state of what is actually loaded
// right now, distinct from Profile (intent).
type RunningInstance struct {
	ID                string
	ProfileID         string
	Status            RunningInstanceStatus
	PrimaryNodeID     string
	ActualPort        *int
	StartedBy         *string
	StartedAt         time.Time
	StoppedAt         *time.Time
	HealthStatus      InstanceHealthStatus
	LastHealthCheckAt *time.Time
	ErrorMessage      *string
}

// ErrRunningInstanceNotFound is returned when a lookup finds no matching
// row.
var ErrRunningInstanceNotFound = errors.New("running instance not found")

// RunningInstanceRepository is the only component that queries the
// running_instances table directly - see CLAUDE.md: the repository layer
// is the only place that accesses the database directly.
type RunningInstanceRepository struct {
	pool *pgxpool.Pool
}

// NewRunningInstanceRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewRunningInstanceRepository(pool *pgxpool.Pool) *RunningInstanceRepository {
	return &RunningInstanceRepository{pool: pool}
}

const runningInstanceColumns = `id, profile_id, status, primary_node_id, actual_port, started_by,
	started_at, stopped_at, health_status, last_health_check_at, error_message`

func scanRunningInstance(row pgx.Row) (*RunningInstance, error) {
	var inst RunningInstance
	err := row.Scan(&inst.ID, &inst.ProfileID, &inst.Status, &inst.PrimaryNodeID, &inst.ActualPort, &inst.StartedBy,
		&inst.StartedAt, &inst.StoppedAt, &inst.HealthStatus, &inst.LastHealthCheckAt, &inst.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunningInstanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan running instance: %w", err)
	}
	return &inst, nil
}

// Create inserts a new running instance in RunningInstanceStatusStarting,
// InstanceHealthUnknown. startedBy is nil only for the break-glass
// SuperAdmin, which is not a Users row.
func (r *RunningInstanceRepository) Create(ctx context.Context, profileID, primaryNodeID string, startedBy *string) (*RunningInstance, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO running_instances (profile_id, status, primary_node_id, started_by, health_status)
		 VALUES ($1, 'starting', $2, $3, 'unknown')
		 RETURNING `+runningInstanceColumns,
		profileID, primaryNodeID, startedBy)

	inst, err := scanRunningInstance(row)
	if err != nil {
		return nil, fmt.Errorf("create running instance: %w", err)
	}
	return inst, nil
}

// FindByID looks up a running instance by its internal ID. Returns
// ErrRunningInstanceNotFound if no row matches.
func (r *RunningInstanceRepository) FindByID(ctx context.Context, id string) (*RunningInstance, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+runningInstanceColumns+` FROM running_instances WHERE id = $1`, id)
	return scanRunningInstance(row)
}

// FindActiveByProfileID returns a profile's current non-terminal running
// instance (status starting, running, or stopping), if any - used to
// refuse a second concurrent load for a profile that is already running
// or mid-transition. Returns ErrRunningInstanceNotFound if none is
// active; a profile can accumulate any number of past stopped/failed
// rows (this is an operation-log-style table, not one row per profile),
// so this deliberately filters on status rather than just taking the
// most recent row by start time.
func (r *RunningInstanceRepository) FindActiveByProfileID(ctx context.Context, profileID string) (*RunningInstance, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+runningInstanceColumns+` FROM running_instances
		 WHERE profile_id = $1 AND status IN ('starting', 'running', 'stopping')
		 ORDER BY started_at DESC LIMIT 1`,
		profileID)
	return scanRunningInstance(row)
}

// FindActiveByNode returns the most recently started
// RunningInstanceStatusRunning instance on nodeID, if any - used by
// internal/metrics to correlate an incoming telemetry reading with
// whatever model is currently loaded on that node. Deliberately narrower
// than FindActiveByProfileID's "starting, running, or stopping" - only a
// genuinely running instance represents "which model was loaded at the
// time" (SCHEMA.md Metrics). Returns ErrRunningInstanceNotFound if none.
func (r *RunningInstanceRepository) FindActiveByNode(ctx context.Context, nodeID string) (*RunningInstance, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+runningInstanceColumns+` FROM running_instances
		 WHERE primary_node_id = $1 AND status = 'running'
		 ORDER BY started_at DESC LIMIT 1`,
		nodeID)
	return scanRunningInstance(row)
}

// ListRunningByNode returns every currently-RunningInstanceStatusRunning
// instance on nodeID - the input to the running_instances staleness
// reconciliation sweep (PLANNING.md's Decisions Log), triggered when that
// node's agent reconnects. Deliberately plural and scoped to exactly
// 'running', unlike the narrower, singular FindActiveByNode above (which
// internal/metrics depends on for a different purpose, most-recent-only):
// a node can legitimately have more than one simultaneously-running
// instance (multiple profiles, different ports), and every one of them
// needs to be re-verified, not just the latest. Also deliberately narrower
// than FindActiveByProfileID's "starting, running, or stopping" - a
// transitional row isn't this fix's concern (a separate, already-tracked
// Known Issues gap covers stuck starting/stopping rows from a different
// cause).
func (r *RunningInstanceRepository) ListRunningByNode(ctx context.Context, nodeID string) ([]*RunningInstance, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+runningInstanceColumns+` FROM running_instances
		 WHERE primary_node_id = $1 AND status = 'running'
		 ORDER BY started_at`,
		nodeID)
	if err != nil {
		return nil, fmt.Errorf("list running instances for node %s: %w", nodeID, err)
	}
	defer rows.Close()

	var instances []*RunningInstance
	for rows.Next() {
		inst, err := scanRunningInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list running instances for node %s: %w", nodeID, err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running instances for node %s: %w", nodeID, err)
	}
	return instances, nil
}

// SetStatus transitions a running instance's status. actualPort, when
// non-nil, is recorded (typically once status becomes
// RunningInstanceStatusRunning, per the agent's instance_result report);
// a nil actualPort leaves the existing value untouched via COALESCE,
// rather than clobbering it back to null on a later transition (e.g.
// running -> stopping) that has nothing new to report.
// errorMessage is accepted for any status, same reasoning as
// ModelTransferRepository.SetStatus - a caller may want to clear a
// previously-set message. stopped_at is stamped only for the two
// terminal statuses (stopped, failed).
func (r *RunningInstanceRepository) SetStatus(ctx context.Context, id string, status RunningInstanceStatus, actualPort *int, errorMessage *string) error {
	var err error
	if isTerminalRunningInstanceStatus(status) {
		_, err = r.pool.Exec(ctx,
			`UPDATE running_instances SET status = $1, actual_port = COALESCE($2, actual_port), error_message = $3, stopped_at = now() WHERE id = $4`,
			status, actualPort, errorMessage, id)
	} else {
		_, err = r.pool.Exec(ctx,
			`UPDATE running_instances SET status = $1, actual_port = COALESCE($2, actual_port), error_message = $3 WHERE id = $4`,
			status, actualPort, errorMessage, id)
	}
	if err != nil {
		return fmt.Errorf("set status for running instance %s: %w", id, err)
	}
	return nil
}

// List returns every running instance, most recently started first - the
// Dashboard overview page's fleet summary.
func (r *RunningInstanceRepository) List(ctx context.Context) ([]*RunningInstance, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+runningInstanceColumns+` FROM running_instances ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}
	defer rows.Close()

	var instances []*RunningInstance
	for rows.Next() {
		inst, err := scanRunningInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list running instances: %w", err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}
	return instances, nil
}
