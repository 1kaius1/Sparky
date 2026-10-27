// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/mailer"
)

const (
	// workerInterval is how often the worker looks for mail that is due.
	workerInterval = 10 * time.Second

	// batchSize bounds how many emails one pass claims. With sendTimeout it
	// keeps a stalled relay's worst case (batchSize * sendTimeout) well inside
	// leaseDuration.
	batchSize     = 5
	leaseDuration = 5 * time.Minute

	// maxAttempts is how many times an email is tried before it is given up
	// on, and maxAge how long it may wait: mail that is a day old is no longer
	// news.
	maxAttempts = 6
	maxAge      = 24 * time.Hour

	// retention is how long finished mail is kept as a record.
	retention = 30 * 24 * time.Hour

	// purgeEvery is how often old records are removed.
	purgeEvery = time.Hour
)

// backoff is the wait after the nth failed attempt (index n-1): a relay that is
// briefly down is retried soon, one that stays down is retried less often.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// Run delivers queued mail until ctx is cancelled. A failed pass is logged and
// retried at the next tick - nothing here may take the server down.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.ProcessOnce(ctx); err != nil {
			s.logger.Printf("email: delivery pass: %v", err)
		}
		if s.now().Sub(lastPurge) >= purgeEvery {
			lastPurge = s.now()
			if n, err := s.queue.DeleteOlderThan(ctx, s.now().Add(-retention)); err != nil {
				s.logger.Printf("email: remove old records: %v", err)
			} else if n > 0 {
				s.logger.Printf("email: removed %d delivery records older than %d days", n, int(retention.Hours()/24))
			}
		}
	}
}

// ProcessOnce makes one delivery pass: gives up on mail that has waited too
// long, then sends what is due. It does nothing at all about sending while
// email is turned off (mail queued earlier waits, and is given up on once it is
// a day old). Exported for tests.
func (s *Service) ProcessOnce(ctx context.Context) error {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get email settings: %w", err)
	}
	if n, err := s.queue.FailOlderThan(ctx, s.now().Add(-maxAge), "not delivered within 24 hours"); err != nil {
		return fmt.Errorf("give up on old mail: %w", err)
	} else if n > 0 {
		s.logger.Printf("email: gave up on %d emails older than 24 hours", n)
	}
	if !cfg.Enabled {
		return nil
	}

	batch, err := s.queue.Claim(ctx, batchSize, leaseDuration)
	if err != nil {
		return err
	}
	mc := s.mailerConfig(cfg)
	for _, e := range batch {
		if ctx.Err() != nil {
			// Shutting down: the claimed rest stay leased and are retried later.
			return nil
		}
		s.deliver(ctx, mc, e)
	}
	return nil
}

// deliver sends one claimed email and records the outcome.
func (s *Service) deliver(ctx context.Context, mc mailer.Config, e *db.Email) {
	err := s.send(ctx, mc, mailer.Message{To: e.ToAddress, Subject: e.Subject, Body: e.Body})
	switch {
	case err == nil:
		if mErr := s.queue.MarkSent(ctx, e.ID); mErr != nil {
			// It went, but we cannot say so: it will be sent again when the
			// lease runs out. Louder than a normal failure for that reason.
			s.logger.Printf("email: %s was delivered but recording it failed (it may be sent twice): %v", e.ID, mErr)
		}
	case mailer.IsPermanent(err):
		s.logger.Printf("email: %s to %s refused by the relay: %v", e.ID, e.ToAddress, err)
		s.record(s.queue.MarkFailed(ctx, e.ID, clip(err.Error())), e.ID)
	case e.Attempts >= maxAttempts:
		s.logger.Printf("email: %s to %s given up on after %d attempts: %v", e.ID, e.ToAddress, e.Attempts, err)
		s.record(s.queue.MarkFailed(ctx, e.ID, clip(fmt.Sprintf("gave up after %d attempts: %v", e.Attempts, err))), e.ID)
	default:
		wait := backoff[min(e.Attempts, len(backoff))-1]
		s.logger.Printf("email: %s to %s not delivered (attempt %d), trying again in %s: %v", e.ID, e.ToAddress, e.Attempts, wait, err)
		s.record(s.queue.MarkRetry(ctx, e.ID, s.now().Add(wait), clip(err.Error())), e.ID)
	}
}

func (s *Service) record(err error, id string) {
	if err != nil {
		s.logger.Printf("email: record the outcome for %s: %v", id, err)
	}
}

// clip prepares an error message to be stored on a row: valid UTF-8 with no NUL
// bytes (Postgres refuses either, which would lose the very record of the
// failure), cut to a bounded length at a character boundary.
func clip(msg string) string {
	const max = 500
	msg = strings.ReplaceAll(strings.ToValidUTF8(msg, "?"), "\x00", "?")
	if len(msg) <= max {
		return msg
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
