// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/schedule"
)

const (
	// notifyTimeout bounds the database work of deciding who to email about one
	// alert, so a slow database cannot hold up whatever raised the alert.
	notifyTimeout = 10 * time.Second

	// digestMaxAge is how long an alert waits for a digest that never came
	// (email off for a while, say) before it is dropped: a digest of last
	// week's alerts is not news.
	digestMaxAge = 48 * time.Hour
)

// AlertRaised is what internal/alerts calls when an alert is raised for the
// first time or becomes more severe (a repeat at the same severity is not
// news). It decides who should hear about it by email - the people the alert
// is addressed to who have an address and want this severity - and either
// queues an email for each who wants them at once, or adds the alert to the
// digest of each who prefers one. It never returns an error and never blocks
// for long: whatever raised the alert must not fail because mail could not be
// arranged, and the alert is in the inbox either way.
func (s *Service) AlertRaised(ctx context.Context, a *db.Alert, escalated bool) {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	cfg, err := s.settings.Get(ctx)
	if err != nil {
		s.logger.Printf("email: read settings to email about alert %s: %v", a.ID, err)
		return
	}
	if !cfg.Enabled {
		return
	}
	recipients, err := s.users.RecipientsForAlert(ctx, a.ID, a.Severity)
	if err != nil {
		s.logger.Printf("email: find who to email about alert %s: %v", a.ID, err)
		return
	}
	if len(recipients) == 0 {
		return
	}

	loc := s.location(ctx)
	subject, body := alertMessage(a, escalated, loc, s.env.PublicURL)
	for _, r := range recipients {
		switch r.Delivery {
		case db.EmailDeliveryImmediate:
			uid := r.UserID
			if err := s.queueChecked(ctx, Message{Kind: db.EmailKindAlert, To: r.Email, UserID: &uid, Subject: subject, Body: body}); err != nil {
				s.logger.Printf("email: queue the email about alert %s for user %s: %v", a.ID, r.UserID, err)
			}
		case db.EmailDeliveryHourly, db.EmailDeliveryDaily:
			if err := s.users.AddDigestItem(ctx, r.UserID, a.ID, a.Severity, a.Title); err != nil {
				s.logger.Printf("email: add alert %s to the digest of user %s: %v", a.ID, r.UserID, err)
			}
		}
	}
}

// MyPreferences are how one person wants alert email to reach them.
type MyPreferences struct {
	Delivery    db.EmailDelivery
	MinSeverity db.AlertSeverity
}

// SetMyPreferences saves how a person wants alert email to reach them - off,
// immediately, or in an hourly or daily digest - and the lowest severity they
// want by email. Audited as updated_email_preferences; a failed audit write
// puts the old choice back.
func (s *Service) SetMyPreferences(ctx context.Context, userID string, p MyPreferences) error {
	if !p.Delivery.Valid() {
		return fmt.Errorf("%w: choose how you want alert email delivered", ErrInvalidSettings)
	}
	if p.MinSeverity.Rank() == 0 {
		return fmt.Errorf("%w: choose the lowest severity you want by email", ErrInvalidSettings)
	}
	before, err := s.users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if before.Delivery == p.Delivery && before.MinSeverity == p.MinSeverity {
		return nil
	}
	if err := s.users.SetPreferences(ctx, userID, p.Delivery, p.MinSeverity); err != nil {
		return fmt.Errorf("save the preferences: %w", err)
	}
	detail := map[string]any{
		"delivery": string(p.Delivery), "min_severity": string(p.MinSeverity),
		"previous_delivery": string(before.Delivery), "previous_min_severity": string(before.MinSeverity),
	}
	if err := s.audit.Record(ctx, &userID, false, "updated_email_preferences", "user", userID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.users.SetPreferences(ctx, userID, before.Delivery, before.MinSeverity); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// RunDigest sends each person who prefers the given kind of digest one email
// listing the alerts that arrived since their last one and are still open. An
// alert since resolved, or one the person dismissed, is left out; nothing is
// sent to a person with nothing left to list. Waiting items go only once the
// email is queued, so a failure to queue leaves them for the next digest.
func (s *Service) RunDigest(ctx context.Context, mode db.EmailDelivery) error {
	if mode != db.EmailDeliveryHourly && mode != db.EmailDeliveryDaily {
		return fmt.Errorf("not a digest: %q", mode)
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get email settings: %w", err)
	}
	if !cfg.Enabled {
		return nil
	}
	if n, err := s.users.DeleteDigestItemsOlderThan(ctx, s.now().Add(-digestMaxAge)); err != nil {
		s.logger.Printf("email: drop old digest items: %v", err)
	} else if n > 0 {
		s.logger.Printf("email: dropped %d digest items older than %s", n, digestMaxAge)
	}

	userIDs, err := s.users.UsersWithDigestItems(ctx, mode)
	if err != nil {
		return err
	}
	loc := s.location(ctx)
	for _, uid := range userIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.digestFor(ctx, uid, mode, loc)
	}
	return nil
}

// digestFor builds and queues one person's digest. A problem with one person is
// logged and does not stop the others.
func (s *Service) digestFor(ctx context.Context, userID string, mode db.EmailDelivery, loc *time.Location) {
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		s.logger.Printf("email: read user %s for their digest: %v", userID, err)
		return
	}
	items, err := s.users.PendingDigest(ctx, userID)
	if err != nil {
		s.logger.Printf("email: read the digest of user %s: %v", userID, err)
		return
	}
	var all []string
	var entries []digestEntry
	for _, it := range items {
		all = append(all, it.AlertID)
		if it.Open && it.Severity.Rank() >= u.MinSeverity.Rank() {
			entries = append(entries, digestEntry{alertID: it.AlertID, severity: it.Severity, title: it.Title, queued: it.QueuedAt})
		}
	}

	// A person with no address, or who has since turned email off or picked
	// another way, is sent nothing; what was waiting is cleared.
	if u.Email == nil || u.Delivery != mode || len(entries) == 0 {
		if err := s.users.DeleteDigestItems(ctx, userID, all); err != nil {
			s.logger.Printf("email: clear the digest of user %s: %v", userID, err)
		}
		return
	}
	how := "hourly"
	if mode == db.EmailDeliveryDaily {
		how = "daily"
	}
	subject, body := digestMessage(entries, how, loc, s.env.PublicURL)
	if err := s.queueChecked(ctx, Message{Kind: db.EmailKindDigest, To: *u.Email, UserID: &userID, Subject: subject, Body: body}); err != nil {
		s.logger.Printf("email: queue the digest for user %s (it stays waiting): %v", userID, err)
		return
	}
	if err := s.users.DeleteDigestItems(ctx, userID, all); err != nil {
		// Queued but not cleared: the next digest repeats these. Better than
		// losing them.
		s.logger.Printf("email: clear the digest of user %s after queueing it (it may be sent again): %v", userID, err)
	}
}

// digestSpecs are the two digest schedules: every hour on the hour, and every
// day at the Admin-set time.
func hourlySpec() schedule.Spec {
	times := make([]string, 24)
	for h := 0; h < 24; h++ {
		times[h] = fmt.Sprintf("%02d:00", h)
	}
	return schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Times: times}
}

func dailySpec(at string) schedule.Spec {
	return schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Times: []string{at}}
}

// DigestJobs returns the two digests as scheduler jobs, hourly and daily. Each
// reads its schedule and last run from the settings row at every check, so a
// change of the daily time applies without a restart; both are off while email
// is off.
func (s *Service) DigestJobs() []schedule.Job {
	job := func(name string, kind db.DigestKind, mode db.EmailDelivery) schedule.Job {
		return schedule.Job{
			Name: name,
			Load: func(ctx context.Context) (schedule.State, error) {
				cfg, err := s.settings.Get(ctx)
				if err != nil {
					return schedule.State{}, err
				}
				st := schedule.State{Enabled: cfg.Enabled, Anchor: cfg.DigestScheduleSetAt}
				if kind == db.DigestHourly {
					st.Spec = hourlySpec()
					if cfg.LastHourlyDigestAt != nil {
						st.LastRun = *cfg.LastHourlyDigestAt
					}
				} else {
					st.Spec = dailySpec(cfg.DailyDigestTime)
					if cfg.LastDailyDigestAt != nil {
						st.LastRun = *cfg.LastDailyDigestAt
					}
				}
				return st, nil
			},
			Begin: func(ctx context.Context, startedAt time.Time) error {
				return s.settings.MarkDigestRun(ctx, kind, startedAt)
			},
			Run: func(ctx context.Context, _ time.Time) error { return s.RunDigest(ctx, mode) },
		}
	}
	return []schedule.Job{
		job("hourly email digest", db.DigestHourly, db.EmailDeliveryHourly),
		job("daily email digest", db.DigestDaily, db.EmailDeliveryDaily),
	}
}
