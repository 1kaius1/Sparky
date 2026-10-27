// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmailKind is what a queued email is for.
type EmailKind string

const (
	EmailKindAlert        EmailKind = "alert"
	EmailKindDigest       EmailKind = "digest"
	EmailKindConfirmation EmailKind = "confirmation"
	EmailKindTest         EmailKind = "test"
)

// EmailStatus is where a queued email is in its life.
type EmailStatus string

const (
	EmailPending EmailStatus = "pending"
	EmailSent    EmailStatus = "sent"
	EmailFailed  EmailStatus = "failed"
)

// Email mirrors one email_queue row - see SCHEMA.md Email queue.
type Email struct {
	ID            string
	Kind          EmailKind
	ToAddress     string
	UserID        *string
	Subject       string
	Body          string
	Status        EmailStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     *string
	CreatedAt     time.Time
	SentAt        *time.Time
}

// NewEmail is what a caller queues.
type NewEmail struct {
	Kind      EmailKind
	ToAddress string
	UserID    *string
	Subject   string
	Body      string
}

// EmailQueueStats is a summary of the queue for the Settings page.
type EmailQueueStats struct {
	Pending        int
	SentLastDay    int
	FailedLastWeek int
	// LastError and LastErrorAt describe the most recent failed attempt that
	// is still on record, "" and zero when there is none.
	LastError   string
	LastErrorAt time.Time
}

// EmailQueueRepository is the only component that queries the email_queue
// table directly.
type EmailQueueRepository struct {
	pool *pgxpool.Pool
}

// NewEmailQueueRepository wraps an already-established, already-verified pool
// - see New in db.go.
func NewEmailQueueRepository(pool *pgxpool.Pool) *EmailQueueRepository {
	return &EmailQueueRepository{pool: pool}
}

const emailColumns = `id, kind, to_address, user_id, subject, body, status, attempts, next_attempt_at, last_error, created_at, sent_at`

func scanEmail(row pgx.Row) (*Email, error) {
	var e Email
	if err := row.Scan(&e.ID, &e.Kind, &e.ToAddress, &e.UserID, &e.Subject, &e.Body, &e.Status, &e.Attempts,
		&e.NextAttemptAt, &e.LastError, &e.CreatedAt, &e.SentAt); err != nil {
		return nil, fmt.Errorf("scan email: %w", err)
	}
	return &e, nil
}

// Enqueue adds an email, due at once.
func (r *EmailQueueRepository) Enqueue(ctx context.Context, in NewEmail) (*Email, error) {
	e, err := scanEmail(r.pool.QueryRow(ctx,
		`INSERT INTO email_queue (kind, to_address, user_id, subject, body) VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+emailColumns, in.Kind, in.ToAddress, in.UserID, in.Subject, in.Body))
	if err != nil {
		return nil, fmt.Errorf("enqueue email: %w", err)
	}
	return e, nil
}

// Claim takes up to limit pending emails that are due, oldest first, counts
// an attempt against each and pushes its next attempt out by lease, so that if
// the caller crashes mid-send the email is retried later rather than lost or
// sent twice at once. Rows locked by a concurrent Claim are skipped.
func (r *EmailQueueRepository) Claim(ctx context.Context, limit int, lease time.Duration) ([]*Email, error) {
	rows, err := r.pool.Query(ctx,
		`UPDATE email_queue SET attempts = attempts + 1, next_attempt_at = now() + $2::interval
		 WHERE id IN (
		   SELECT id FROM email_queue WHERE status = 'pending' AND next_attempt_at <= now()
		   ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		 RETURNING `+emailColumns, limit, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("claim emails: %w", err)
	}
	defer rows.Close()
	var out []*Email
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim emails: %w", err)
	}
	return out, nil
}

// MarkSent records a delivery.
func (r *EmailQueueRepository) MarkSent(ctx context.Context, id string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_queue SET status = 'sent', sent_at = now(), last_error = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark email sent: %w", err)
	}
	return nil
}

// MarkRetry records a failed attempt and when to try again.
func (r *EmailQueueRepository) MarkRetry(ctx context.Context, id string, next time.Time, reason string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_queue SET next_attempt_at = $2, last_error = $3 WHERE id = $1 AND status = 'pending'`, id, next, reason); err != nil {
		return fmt.Errorf("mark email for retry: %w", err)
	}
	return nil
}

// MarkFailed gives up on an email.
func (r *EmailQueueRepository) MarkFailed(ctx context.Context, id string, reason string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE email_queue SET status = 'failed', last_error = $2 WHERE id = $1 AND status = 'pending'`, id, reason); err != nil {
		return fmt.Errorf("mark email failed: %w", err)
	}
	return nil
}

// FailOlderThan gives up on every still-pending email created before cutoff,
// with reason, and returns how many it gave up on - mail too old to be worth
// sending, or queued while email was off.
func (r *EmailQueueRepository) FailOlderThan(ctx context.Context, cutoff time.Time, reason string) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE email_queue SET status = 'failed', last_error = $2 WHERE status = 'pending' AND created_at < $1`, cutoff, reason)
	if err != nil {
		return 0, fmt.Errorf("fail old emails: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteOlderThan removes finished (sent or failed) emails created before
// cutoff and returns how many it removed. A pending email is never removed.
func (r *EmailQueueRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM email_queue WHERE status <> 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old emails: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Stats summarizes the queue.
func (r *EmailQueueRepository) Stats(ctx context.Context) (*EmailQueueStats, error) {
	var s EmailQueueStats
	err := r.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE status = 'pending'),
		   count(*) FILTER (WHERE status = 'sent' AND sent_at > now() - interval '1 day'),
		   count(*) FILTER (WHERE status = 'failed' AND created_at > now() - interval '7 days')
		 FROM email_queue`).Scan(&s.Pending, &s.SentLastDay, &s.FailedLastWeek)
	if err != nil {
		return nil, fmt.Errorf("email queue stats: %w", err)
	}
	var lastErr *string
	var at *time.Time
	err = r.pool.QueryRow(ctx,
		`SELECT last_error, created_at FROM email_queue
		 WHERE last_error IS NOT NULL AND status <> 'sent' ORDER BY created_at DESC LIMIT 1`).Scan(&lastErr, &at)
	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("email queue last error: %w", err)
	}
	if lastErr != nil {
		s.LastError = *lastErr
	}
	if at != nil {
		s.LastErrorAt = *at
	}
	return &s, nil
}

// Recent returns the newest limit emails, any status, newest first - what the
// Settings page lists so an Admin can see what went and what did not.
func (r *EmailQueueRepository) Recent(ctx context.Context, limit int) ([]*Email, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+emailColumns+` FROM email_queue ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent emails: %w", err)
	}
	defer rows.Close()
	var out []*Email
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
