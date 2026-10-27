// SPDX-License-Identifier: AGPL-3.0-or-later

package schedule

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// ErrInvalidTimezone is returned for a time zone name that is not an IANA zone
// this server knows.
var ErrInvalidTimezone = errors.New("invalid time zone")

// settingsAuditObjectID stands in for schedule_settings' singleton row in an
// audit_log entry - object_id is a real uuid, so the conventional all-zero uuid
// is used, as for container_log_settings and theme_settings.
const settingsAuditObjectID = "00000000-0000-0000-0000-000000000000"

// zoneNamePattern is the characters an IANA zone name uses. It keeps a name
// that is a path ("../etc/passwd") or a Go-specific alias out of
// time.LoadLocation altogether.
var zoneNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+\-]+(/[A-Za-z0-9_+\-]+){0,2}$`)

// settingsStore is the subset of *db.ScheduleSettingsRepository this package
// needs.
type settingsStore interface {
	Get(ctx context.Context) (*db.ScheduleSettings, error)
	Update(ctx context.Context, timezone string, updatedBy *string) error
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service holds the one global time zone every schedule is read in: set by an
// Admin on the Settings page, read by whatever runs a schedule. One zone for
// the whole deployment suits an organization that standardizes on one for its
// syslog and other messages (PLANNING.md, 2026-10-26).
type Service struct {
	store settingsStore
	audit auditRecorder
}

// NewService constructs a Service.
func NewService(store settingsStore, audit auditRecorder) *Service {
	return &Service{store: store, audit: audit}
}

// ParseTimezone resolves an IANA zone name ("America/New_York", "UTC"). It
// refuses an empty name and "Local" - both of which time.LoadLocation accepts,
// meaning UTC and the server process's own zone respectively - because an
// Admin typing either has not named a zone, and the second would make every
// schedule depend on the host's configuration.
func ParseTimezone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > 64 || !zoneNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: %q is not an IANA time zone name such as America/New_York or UTC", ErrInvalidTimezone, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a time zone this server knows", ErrInvalidTimezone, name)
	}
	return loc, nil
}

// Settings returns the schedule settings. Requires rbac.CanViewSettings
// (Admin): the Settings page is one all-or-nothing Admin gate.
func (s *Service) Settings(ctx context.Context, actor rbac.Actor) (*db.ScheduleSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.store.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get schedule settings: %w", err)
	}
	return cfg, nil
}

// Location returns the global time zone as a *time.Location, for the code that
// runs schedules. It does no authorization: it is not reachable from a
// request. A stored name this server cannot load (a zone database that lost
// it) is an error rather than a quiet fall back to UTC, so a schedule never
// runs at a time nobody chose.
func (s *Service) Location(ctx context.Context) (*time.Location, error) {
	cfg, err := s.store.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get schedule settings: %w", err)
	}
	loc, err := ParseTimezone(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("stored time zone: %w", err)
	}
	return loc, nil
}

// UpdateTimezone sets the global time zone. Requires rbac.CanViewSettings.
// Audited as updated_schedule_timezone; as with the other Settings writes, a
// failed audit write reverts the change rather than leaving an unaudited one
// in place.
func (s *Service) UpdateTimezone(ctx context.Context, actor rbac.Actor, name string) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if _, err := ParseTimezone(name); err != nil {
		return err
	}
	current, err := s.store.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current schedule settings: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.store.Update(ctx, name, actorID); err != nil {
		return fmt.Errorf("update schedule settings: %w", err)
	}
	detail := map[string]any{"timezone": name, "previous_timezone": current.Timezone}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_schedule_timezone", "schedule_settings", settingsAuditObjectID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.store.Update(ctx, current.Timezone, current.UpdatedBy); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}
