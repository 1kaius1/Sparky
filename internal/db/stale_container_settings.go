// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StaleContainerMode is what stale container monitoring does on its schedule.
type StaleContainerMode string

const (
	// StaleContainerModeNone does nothing. The default.
	StaleContainerModeNone StaleContainerMode = "none"

	// StaleContainerModeReport scans on the schedule and raises alerts.
	StaleContainerModeReport StaleContainerMode = "report"

	// StaleContainerModePrune scans on the schedule and removes the stale
	// containers it finds, after archiving each one's log.
	StaleContainerModePrune StaleContainerMode = "prune"
)

// Valid reports whether m is one of the three modes.
func (m StaleContainerMode) Valid() bool {
	switch m {
	case StaleContainerModeNone, StaleContainerModeReport, StaleContainerModePrune:
		return true
	}
	return false
}

// StaleContainerSettings mirrors the stale_container_settings table - see
// SCHEMA.md Stale container settings. A singleton row, always present as of
// migration 000045.
type StaleContainerSettings struct {
	Mode StaleContainerMode

	// Schedule is an internal/schedule Spec as JSON; nil only while the mode
	// is none and no schedule has ever been saved.
	Schedule json.RawMessage

	// ScheduleSetAt is when the schedule was last saved or the monitoring was
	// last switched on, which the schedule counts from.
	ScheduleSetAt time.Time

	// LastRunAt is when a scheduled run last began; nil if none has.
	LastRunAt *time.Time

	// LastRunSummary is what that run found and did; opaque JSON for display.
	LastRunSummary json.RawMessage

	MinAgeHours  int
	ArchiveLines int
	UpdatedBy    *string
	UpdatedAt    time.Time
}

// StaleContainerSettingsRepository is the only component that queries the
// stale_container_settings table directly.
type StaleContainerSettingsRepository struct {
	pool *pgxpool.Pool
}

// NewStaleContainerSettingsRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewStaleContainerSettingsRepository(pool *pgxpool.Pool) *StaleContainerSettingsRepository {
	return &StaleContainerSettingsRepository{pool: pool}
}

const staleContainerSettingsColumns = `mode, schedule, schedule_set_at, last_run_at, last_run_summary,
	min_age_hours, archive_lines, updated_by, updated_at`

// Get returns the current settings.
func (r *StaleContainerSettingsRepository) Get(ctx context.Context) (*StaleContainerSettings, error) {
	var s StaleContainerSettings
	err := r.pool.QueryRow(ctx,
		`SELECT `+staleContainerSettingsColumns+` FROM stale_container_settings WHERE id = true`).
		Scan(&s.Mode, &s.Schedule, &s.ScheduleSetAt, &s.LastRunAt, &s.LastRunSummary,
			&s.MinAgeHours, &s.ArchiveLines, &s.UpdatedBy, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get stale container settings: %w", err)
	}
	return &s, nil
}

// StaleContainerSettingsUpdate is what an Admin can change. UpdatedBy is nil
// only for the break-glass SuperAdmin, which is not a Users row.
type StaleContainerSettingsUpdate struct {
	Mode         StaleContainerMode
	Schedule     json.RawMessage
	MinAgeHours  int
	ArchiveLines int
	UpdatedBy    *string
}

// Update saves an Admin's change. A new schedule, or switching the monitoring
// on from none, restarts the schedule: ScheduleSetAt becomes now and LastRunAt
// is cleared, so a weekly schedule does not count occurrences from before it
// was saved and an every-N-days schedule's first scan is the moment it is
// saved. Saving anything else about an unchanged running schedule leaves both
// alone, so editing the minimum age never triggers a scan. The database's CHECKs
// are a second line behind the service's validation.
func (r *StaleContainerSettingsRepository) Update(ctx context.Context, u StaleContainerSettingsUpdate) error {
	// Every SET expression reads the row as it was before this statement, so
	// "mode = 'none'" below means "was none until now".
	if _, err := r.pool.Exec(ctx,
		`UPDATE stale_container_settings SET
		   schedule_set_at = CASE WHEN schedule IS DISTINCT FROM $2::jsonb OR mode = 'none' THEN now() ELSE schedule_set_at END,
		   last_run_at = CASE WHEN schedule IS DISTINCT FROM $2::jsonb OR mode = 'none' THEN NULL ELSE last_run_at END,
		   mode = $1, schedule = $2::jsonb, min_age_hours = $3, archive_lines = $4,
		   updated_by = $5, updated_at = now()
		 WHERE id = true`,
		u.Mode, nullableJSON(u.Schedule), u.MinAgeHours, u.ArchiveLines, u.UpdatedBy); err != nil {
		return fmt.Errorf("update stale container settings: %w", err)
	}
	return nil
}

// Restore puts every column an Admin's change touched back exactly as prev
// had it, including the schedule's save time and last run - used to undo a
// change whose audit record could not be written. Update cannot do this: it
// would treat the restored schedule as a new one.
func (r *StaleContainerSettingsRepository) Restore(ctx context.Context, prev *StaleContainerSettings) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE stale_container_settings SET
		   mode = $1, schedule = $2::jsonb, schedule_set_at = $3, last_run_at = $4,
		   min_age_hours = $5, archive_lines = $6, updated_by = $7, updated_at = $8
		 WHERE id = true`,
		prev.Mode, nullableJSON(prev.Schedule), prev.ScheduleSetAt, prev.LastRunAt,
		prev.MinAgeHours, prev.ArchiveLines, prev.UpdatedBy, prev.UpdatedAt); err != nil {
		return fmt.Errorf("restore stale container settings: %w", err)
	}
	return nil
}

// MarkRun records that a scheduled run is beginning at startedAt, before it
// does anything, so a crash part-way cannot make it run again.
func (r *StaleContainerSettingsRepository) MarkRun(ctx context.Context, startedAt time.Time) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE stale_container_settings SET last_run_at = $1 WHERE id = true`, startedAt); err != nil {
		return fmt.Errorf("mark stale container run: %w", err)
	}
	return nil
}

// SetLastRunSummary stores what the latest scheduled run found and did.
func (r *StaleContainerSettingsRepository) SetLastRunSummary(ctx context.Context, summary json.RawMessage) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE stale_container_settings SET last_run_summary = $1::jsonb WHERE id = true`, nullableJSON(summary)); err != nil {
		return fmt.Errorf("set stale container run summary: %w", err)
	}
	return nil
}

// nullableJSON passes an empty value to the database as NULL rather than as
// an invalid empty JSON document.
func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
