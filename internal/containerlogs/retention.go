// SPDX-License-Identifier: AGPL-3.0-or-later

package containerlogs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// MinRetentionMonths and MaxRetentionMonths bound the retention period,
	// matching the audit log's own range (SCHEMA.md Audit settings). The
	// database enforces the same range with a CHECK constraint.
	MinRetentionMonths = 1
	MaxRetentionMonths = 24

	// retentionFirstRunDelay holds the first expiry run back after startup so
	// it never competes with the server coming up, and retentionInterval is
	// how often it runs after that.
	retentionFirstRunDelay = time.Minute
	retentionInterval      = 24 * time.Hour
)

// ErrInvalidRetention is returned for a retention period outside the allowed
// range.
var ErrInvalidRetention = errors.New("invalid retention period")

// settingsAuditObjectID stands in for container_log_settings' singleton row in
// an audit_log entry - object_id is a real uuid, so the conventional all-zero
// uuid is used, as for theme_settings.
const settingsAuditObjectID = "00000000-0000-0000-0000-000000000000"

// Settings returns the container log settings. Requires
// rbac.CanViewSettings (Admin): the Settings page is one all-or-nothing Admin
// gate.
func (s *Service) Settings(ctx context.Context, actor rbac.Actor) (*db.ContainerLogSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get container log settings: %w", err)
	}
	return cfg, nil
}

// UpdateRetention sets how many months archived container logs are kept
// (MinRetentionMonths to MaxRetentionMonths). Requires rbac.CanViewSettings.
// Audited as updated_container_log_retention; as with the other Settings
// writes, a failed audit write reverts the change rather than leaving an
// unaudited one in place.
func (s *Service) UpdateRetention(ctx context.Context, actor rbac.Actor, months int) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if months < MinRetentionMonths || months > MaxRetentionMonths {
		return fmt.Errorf("%w: keep logs between %d and %d months", ErrInvalidRetention, MinRetentionMonths, MaxRetentionMonths)
	}
	current, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current container log settings: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.settings.Update(ctx, months, actorID); err != nil {
		return fmt.Errorf("update container log settings: %w", err)
	}
	detail := map[string]any{"retention_months": months, "previous_retention_months": current.RetentionMonths}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_container_log_retention", "container_log_settings", settingsAuditObjectID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.settings.Update(ctx, current.RetentionMonths, current.UpdatedBy); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// ExpireOnce deletes archived container logs older than the retention period
// and returns how many it deleted. A run that deletes something is recorded in
// the audit log as expired_container_logs by the system: a null actor that is
// not the break-glass account (SCHEMA.md Audit log). The record follows the
// delete, since the count is not known before; if it cannot be written the
// failure is logged loudly and returned.
func (s *Service) ExpireOnce(ctx context.Context) (int64, error) {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("get container log settings: %w", err)
	}
	cutoff := s.now().AddDate(0, -cfg.RetentionMonths, 0)
	n, err := s.store.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	detail := map[string]any{"deleted": n, "retention_months": cfg.RetentionMonths, "older_than": cutoff.UTC().Format(time.RFC3339)}
	if err := s.audit.Record(ctx, nil, false, "expired_container_logs", "container_log_archive", settingsAuditObjectID, detail); err != nil {
		return n, fmt.Errorf("record audit for %d expired container logs: %w", n, err)
	}
	return n, nil
}

// RunRetention enforces the retention period until ctx is cancelled: a first
// run shortly after startup, then once a day. A failed run is logged and
// retried at the next tick - nothing here may take the server down.
func (s *Service) RunRetention(ctx context.Context) {
	timer := time.NewTimer(retentionFirstRunDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if n, err := s.ExpireOnce(ctx); err != nil {
			s.logger.Printf("containerlogs: expire old container logs: %v", err)
		} else if n > 0 {
			s.logger.Printf("containerlogs: expired %d container logs past the retention period", n)
		}
		timer.Reset(retentionInterval)
	}
}
