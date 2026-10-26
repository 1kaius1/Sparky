// SPDX-License-Identifier: AGPL-3.0-or-later

package alerts

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
	// matching the audit log's and the container logs' range (SCHEMA.md
	// Audit settings). The database enforces the same range with a CHECK
	// constraint.
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

// Settings returns the alert settings. Requires rbac.CanViewSettings (Admin):
// the Settings page is one all-or-nothing Admin gate.
func (s *Service) Settings(ctx context.Context, actor rbac.Actor) (*db.AlertSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get alert settings: %w", err)
	}
	return cfg, nil
}

// UpdateRetention sets how many months alerts are kept after they were last
// raised (MinRetentionMonths to MaxRetentionMonths). Requires
// rbac.CanViewSettings. Audited as updated_alert_retention; as with the other
// Settings writes, a failed audit write reverts the change rather than
// leaving an unaudited one in place.
func (s *Service) UpdateRetention(ctx context.Context, actor rbac.Actor, months int) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if months < MinRetentionMonths || months > MaxRetentionMonths {
		return fmt.Errorf("%w: keep alerts between %d and %d months", ErrInvalidRetention, MinRetentionMonths, MaxRetentionMonths)
	}
	current, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current alert settings: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	if err := s.settings.Update(ctx, months, actorID); err != nil {
		return fmt.Errorf("update alert settings: %w", err)
	}
	detail := map[string]any{"retention_months": months, "previous_retention_months": current.RetentionMonths}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_alert_retention", "alert_settings", zeroUUID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.settings.Update(ctx, current.RetentionMonths, current.UpdatedBy); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// ExpireOnce deletes alerts last raised before the retention period and
// returns how many it deleted. A run that deletes something is recorded in
// the audit log as expired_alerts by the system: a null actor that is not the
// break-glass account (SCHEMA.md Audit log). The record follows the delete,
// since the count is not known before; if it cannot be written the failure is
// returned and logged by RunRetention.
func (s *Service) ExpireOnce(ctx context.Context) (int64, error) {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("get alert settings: %w", err)
	}
	cutoff := s.now().AddDate(0, -cfg.RetentionMonths, 0)
	n, err := s.store.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	s.changed()
	detail := map[string]any{"deleted": n, "retention_months": cfg.RetentionMonths, "older_than": cutoff.UTC().Format(time.RFC3339)}
	if err := s.audit.Record(ctx, nil, false, "expired_alerts", "alert", zeroUUID, detail); err != nil {
		return n, fmt.Errorf("record audit for %d expired alerts: %w", n, err)
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
			s.logger.Printf("alerts: expire old alerts: %v", err)
		} else if n > 0 {
			s.logger.Printf("alerts: expired %d alerts past the retention period", n)
		}
		timer.Reset(retentionInterval)
	}
}
