// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
)

const (
	// listTimeout is how long a node is waited for to describe its
	// containers. A node whose agent predates this feature ignores the request
	// and so times out; the scan reports it as not scanned.
	listTimeout = 30 * time.Second

	// removeTimeout is how long a node is waited for to remove one container.
	// It covers the agent archiving the log and waiting up to 30 seconds for
	// this server to confirm it stored it.
	removeTimeout = 90 * time.Second

	// maxContainersPerNode and maxListParts bound what one node can make a scan
	// hold, whatever its agent claims: the agent's own cap is 500 containers in
	// parts of 30.
	maxContainersPerNode = 600
	maxListParts         = 40

	// MaxMinAgeHours and MaxArchiveLines bound the two numbers an Admin sets,
	// matching the database's CHECK constraints.
	MaxMinAgeHours  = 720
	MaxArchiveLines = 100000

	// settingsAuditObjectID stands in for the settings row in an audit_log
	// entry - object_id is a real uuid, so the conventional all-zero uuid is
	// used, as for the other singleton settings rows.
	settingsAuditObjectID = "00000000-0000-0000-0000-000000000000"
)

var (
	// ErrInvalidSettings is wrapped by a refused settings change; its message
	// is safe to show an Admin.
	ErrInvalidSettings = errors.New("invalid stale container settings")

	// ErrPruneNotConfirmed is returned when a change would turn on Prune without
	// the Admin having confirmed it.
	ErrPruneNotConfirmed = errors.New("confirm that Prune removes containers automatically before turning it on")

	// ErrNotStale is returned by RemoveOne for a container that is not stale
	// now. The message says why; it is safe to show.
	ErrNotStale = errors.New("this container is not stale")

	// ErrNodeUnavailable is returned when a node cannot be reached to scan or
	// act: unknown, not a container node, or its agent is not connected.
	ErrNodeUnavailable = errors.New("node is not available")

	// ErrInvalidRequest is returned for a malformed id or line count.
	ErrInvalidRequest = errors.New("invalid request")

	// ErrAuditAfterRemoval is returned, with the result, when a container was
	// removed but the audit entry for it could not be written. The removal
	// cannot be undone, so the failure is reported loudly instead.
	ErrAuditAfterRemoval = errors.New("the container was removed but recording it in the audit log failed")
)

// settingsStore is the subset of *db.StaleContainerSettingsRepository this
// package needs.
type settingsStore interface {
	Get(ctx context.Context) (*db.StaleContainerSettings, error)
	Update(ctx context.Context, u db.StaleContainerSettingsUpdate) error
	Restore(ctx context.Context, prev *db.StaleContainerSettings) error
	MarkRun(ctx context.Context, startedAt time.Time) error
	SetLastRunSummary(ctx context.Context, summary json.RawMessage) error
}

// nodeStore is the subset of *db.NodeRepository this package needs.
type nodeStore interface {
	List(ctx context.Context) ([]*db.Node, error)
	FindByID(ctx context.Context, id string) (*db.Node, error)
}

// instanceStore is the subset of *db.RunningInstanceRepository this package
// needs: matching a node's containers to the instances they belong to.
type instanceStore interface {
	FindByIDs(ctx context.Context, ids []string) (map[string]*db.RunningInstance, error)
}

// profileStore is the subset of *db.ProfileRepository used to name a
// container's profile.
type profileStore interface {
	FindByID(ctx context.Context, id string) (*db.Profile, error)
}

// dispatcher reaches the agents.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// alertRaiser is the subset of *alerts.Service this package needs. None of its
// calls is audited (PLANNING.md, 2026-10-26): the alert row is the record.
type alertRaiser interface {
	Raise(ctx context.Context, p alerts.RaiseParams) (*db.RaiseResult, error)
	ResolveCondition(ctx context.Context, dedupeKey string) (int, error)
	ResolveConditionsExcept(ctx context.Context, prefix string, keep []string) (int, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service finds, reports and removes stale containers.
type Service struct {
	settings  settingsStore
	nodes     nodeStore
	instances instanceStore
	profiles  profileStore
	dispatch  dispatcher
	alerts    alertRaiser
	audit     auditRecorder
	zone      schedule.ZoneSource
	logger    *log.Logger
	now       func() time.Time

	// listTimeout and removeTimeout are the package constants, as fields only
	// so a test can shorten them.
	listTimeout   time.Duration
	removeTimeout time.Duration

	// removeMu serializes removals, scheduled and manual alike. Each archives
	// a log through the same upload path, which holds only a few unfinished
	// uploads per node, so removing one container at a time keeps that path
	// well inside its limits.
	removeMu sync.Mutex

	mu          sync.Mutex
	listWaits   map[string]*listWait
	removeWaits map[string]*removeWait
}

// NewService constructs a Service.
func NewService(settings settingsStore, nodes nodeStore, instances instanceStore, profiles profileStore, dispatch dispatcher, alertsSvc alertRaiser, audit auditRecorder, zone schedule.ZoneSource, logger *log.Logger) *Service {
	return &Service{
		settings: settings, nodes: nodes, instances: instances, profiles: profiles, dispatch: dispatch,
		alerts: alertsSvc, audit: audit, zone: zone, logger: logger, now: time.Now,
		listTimeout: listTimeout, removeTimeout: removeTimeout,
		listWaits: make(map[string]*listWait), removeWaits: make(map[string]*removeWait),
	}
}

// SettingsView is the settings as the Settings page shows them.
type SettingsView struct {
	Settings *db.StaleContainerSettings

	// Spec is the saved schedule, nil if none has been saved.
	Spec *schedule.Spec

	// Timezone is the global zone the schedule is read in.
	Timezone string

	// NextRun is when the schedule will next run; nil when the monitoring is
	// off or the schedule cannot be read. A run already due shows as now.
	NextRun *time.Time

	// LastRun is what the latest scheduled run found and did, nil if none has
	// run (or its summary could not be read).
	LastRun *RunSummary
}

// Settings returns the settings and when the schedule runs next. Requires
// rbac.CanManageStaleContainers.
func (s *Service) Settings(ctx context.Context, actor rbac.Actor) (*SettingsView, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get stale container settings: %w", err)
	}
	view := &SettingsView{Settings: cfg}

	if len(cfg.Schedule) > 0 {
		var spec schedule.Spec
		if err := json.Unmarshal(cfg.Schedule, &spec); err != nil {
			s.logger.Printf("stalecontainers: the saved schedule cannot be read: %v", err)
		} else if spec.Validate() == nil {
			view.Spec = &spec
		}
	}
	if len(cfg.LastRunSummary) > 0 {
		var sum RunSummary
		if err := json.Unmarshal(cfg.LastRunSummary, &sum); err == nil {
			view.LastRun = &sum
		}
	}
	loc, err := s.zone.Location(ctx)
	if err != nil {
		// The page still renders; it just cannot say when the next run is.
		s.logger.Printf("stalecontainers: time zone for the next run: %v", err)
		return view, nil
	}
	view.Timezone = loc.String()
	if cfg.Mode != db.StaleContainerModeNone && view.Spec != nil {
		now := s.now()
		var lastRun time.Time
		if cfg.LastRunAt != nil {
			lastRun = *cfg.LastRunAt
		}
		if _, due := view.Spec.Latest(cfg.ScheduleSetAt, lastRun, now, loc); due {
			view.NextRun = &now
		} else if next, ok := view.Spec.Next(cfg.ScheduleSetAt, now, loc); ok {
			view.NextRun = &next
		}
	}
	return view, nil
}

// SettingsInput is what the Settings form submits.
type SettingsInput struct {
	Mode db.StaleContainerMode

	// Schedule is the schedule to save. With Mode none it may be left zero, in
	// which case the saved schedule (if any) is kept.
	Schedule    schedule.Spec
	HasSchedule bool

	MinAgeHours  int
	ArchiveLines int

	// ConfirmPrune is the Admin's explicit confirmation that Prune removes
	// containers on its own. It is required whenever the change turns Prune on
	// from some other mode.
	ConfirmPrune bool
}

// UpdateSettings saves the monitoring mode, its schedule, the minimum age and
// the number of log lines a scheduled prune archives. Requires
// rbac.CanManageStaleContainers. Audited as updated_stale_container_settings;
// as with the other Settings writes, a failed audit write reverts the change
// rather than leaving an unaudited one in place.
//
// Turning Prune on needs ConfirmPrune each time it is switched on from another
// mode (not only the first time ever): it is the one mode that deletes
// things unattended, and switching to it is rare enough that asking again costs
// nothing. Editing the schedule of a monitoring that is already Prune does not
// ask again.
func (s *Service) UpdateSettings(ctx context.Context, actor rbac.Actor, in SettingsInput) error {
	if !rbac.CanManageStaleContainers(actor) {
		return rbac.ErrNotPermitted
	}
	if !in.Mode.Valid() {
		return fmt.Errorf("%w: choose None, Report or Prune", ErrInvalidSettings)
	}
	if in.MinAgeHours < 0 || in.MinAgeHours > MaxMinAgeHours {
		return fmt.Errorf("%w: the minimum age must be between 0 and %d hours", ErrInvalidSettings, MaxMinAgeHours)
	}
	if in.ArchiveLines < 0 || in.ArchiveLines > MaxArchiveLines {
		return fmt.Errorf("%w: log lines to keep must be between 0 (all) and %d", ErrInvalidSettings, MaxArchiveLines)
	}

	prev, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current stale container settings: %w", err)
	}

	var scheduleJSON json.RawMessage
	switch {
	case in.Mode != db.StaleContainerModeNone || in.HasSchedule:
		if err := in.Schedule.Validate(); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidSettings, trimInvalidSpec(err))
		}
		scheduleJSON, err = json.Marshal(in.Schedule.Normalized())
		if err != nil {
			return fmt.Errorf("encode schedule: %w", err)
		}
	default:
		// Mode none with no schedule submitted: keep whatever was saved.
		scheduleJSON = prev.Schedule
	}
	if in.Mode == db.StaleContainerModePrune && prev.Mode != db.StaleContainerModePrune && !in.ConfirmPrune {
		return ErrPruneNotConfirmed
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.settings.Update(ctx, db.StaleContainerSettingsUpdate{
		Mode: in.Mode, Schedule: scheduleJSON, MinAgeHours: in.MinAgeHours, ArchiveLines: in.ArchiveLines, UpdatedBy: actorID,
	}); err != nil {
		return fmt.Errorf("update stale container settings: %w", err)
	}

	detail := map[string]any{
		"mode": string(in.Mode), "previous_mode": string(prev.Mode),
		"min_age_hours": in.MinAgeHours, "archive_lines": in.ArchiveLines,
		"confirmed_prune": in.ConfirmPrune && in.Mode == db.StaleContainerModePrune,
	}
	if len(scheduleJSON) > 0 {
		detail["schedule"] = json.RawMessage(scheduleJSON)
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_stale_container_settings", "stale_container_settings", settingsAuditObjectID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.settings.Restore(ctx, prev); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// trimInvalidSpec drops the "invalid schedule: " prefix schedule.Spec.Validate
// puts on its message, since ErrInvalidSettings already says what was invalid.
func trimInvalidSpec(err error) string {
	return strings.TrimPrefix(err.Error(), "invalid schedule: ")
}
