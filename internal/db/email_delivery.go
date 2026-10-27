// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"
)

// SetPreferences saves how a person wants alert email to reach them. Moving
// to off or immediate drops any alerts waiting for a digest: they are in the
// inbox, and a digest of them would arrive hours late and out of the blue.
func (r *UserEmailRepository) SetPreferences(ctx context.Context, userID string, delivery EmailDelivery, minSeverity AlertSeverity) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set email preferences: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE users SET email_delivery = $2, email_min_severity = $3 WHERE id = $1`, userID, delivery, minSeverity)
	if err != nil {
		return fmt.Errorf("set email preferences: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	if delivery == EmailDeliveryOff || delivery == EmailDeliveryImmediate {
		if _, err := tx.Exec(ctx, `DELETE FROM email_digest_items WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("drop waiting digest items: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("set email preferences: %w", err)
	}
	return nil
}

// EmailRecipient is a person an alert's email should go to.
type EmailRecipient struct {
	UserID   string
	Email    string
	Delivery EmailDelivery
}

// RecipientsForAlert returns the people an alert is addressed to who should
// get email about it: they have an address, have not turned email off, and
// want at least this severity. "Addressed to" is exactly what the inbox
// uses - a user audience, a tier audience (that tier and above), or an
// application group the person belongs to (a local account added by hand, or
// one the AD membership cache has in a mapped group, compared ignoring case) -
// so a person is emailed about the alerts they would see.
func (r *UserEmailRepository) RecipientsForAlert(ctx context.Context, alertID string, severity AlertSeverity) ([]EmailRecipient, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT u.id, u.email, u.email_delivery
		 FROM users u
		 WHERE u.email IS NOT NULL AND u.email_delivery <> 'off' AND u.email_min_severity <= $2::alert_severity
		 AND EXISTS (
		   SELECT 1 FROM alert_audiences au
		   WHERE au.alert_id = $1::uuid AND (
		     (au.audience_type = 'user' AND au.user_id = u.id)
		     OR (au.audience_type = 'tier' AND au.min_tier <= u.tier)
		     OR (au.audience_type = 'group' AND (
		       EXISTS (SELECT 1 FROM app_group_local_members m
		         WHERE m.app_group_id = au.app_group_id AND m.user_id = u.id)
		       OR EXISTS (SELECT 1 FROM app_group_ad_groups g
		         JOIN user_ad_group_membership c ON lower(c.ad_group_dn) = lower(g.ad_group_dn)
		         WHERE g.app_group_id = au.app_group_id AND c.user_id = u.id)
		     ))
		   )
		 )
		 ORDER BY u.id`, alertID, severity)
	if err != nil {
		return nil, fmt.Errorf("find email recipients: %w", err)
	}
	defer rows.Close()
	var out []EmailRecipient
	for rows.Next() {
		var rec EmailRecipient
		if err := rows.Scan(&rec.UserID, &rec.Email, &rec.Delivery); err != nil {
			return nil, fmt.Errorf("scan email recipient: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DigestItem is one alert waiting for a person's digest.
type DigestItem struct {
	AlertID  string
	Severity AlertSeverity
	Title    string
	QueuedAt time.Time

	// Open is true if the alert is still unresolved and the person has not
	// dismissed it - the ones worth listing.
	Open bool
}

// AddDigestItem queues an alert for a person's next digest. A repeat at a
// higher severity updates the row; one at the same or a lower severity leaves
// the severity where it was.
func (r *UserEmailRepository) AddDigestItem(ctx context.Context, userID, alertID string, severity AlertSeverity, title string) error {
	if _, err := r.pool.Exec(ctx,
		`INSERT INTO email_digest_items (user_id, alert_id, severity, title) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id, alert_id) DO UPDATE SET
		   severity = GREATEST(email_digest_items.severity, EXCLUDED.severity), title = EXCLUDED.title`,
		userID, alertID, severity, title); err != nil {
		return fmt.Errorf("add digest item: %w", err)
	}
	return nil
}

// UsersWithDigestItems returns the people with alerts waiting whose preference
// is the given digest mode, in a stable order.
func (r *UserEmailRepository) UsersWithDigestItems(ctx context.Context, mode EmailDelivery) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT d.user_id FROM email_digest_items d JOIN users u ON u.id = d.user_id
		 WHERE u.email_delivery = $1 ORDER BY d.user_id`, mode)
	if err != nil {
		return nil, fmt.Errorf("find users with digest items: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PendingDigest returns everything waiting for one person's digest, oldest
// first, each marked open or not (see DigestItem.Open).
func (r *UserEmailRepository) PendingDigest(ctx context.Context, userID string) ([]DigestItem, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT d.alert_id, d.severity, d.title, d.queued_at,
		        (a.resolved_at IS NULL AND s.dismissed_at IS NULL) AS open
		 FROM email_digest_items d
		 JOIN alerts a ON a.id = d.alert_id
		 LEFT JOIN alert_user_state s ON s.alert_id = d.alert_id AND s.user_id = d.user_id
		 WHERE d.user_id = $1 ORDER BY d.queued_at, d.alert_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("get pending digest: %w", err)
	}
	defer rows.Close()
	var out []DigestItem
	for rows.Next() {
		var it DigestItem
		if err := rows.Scan(&it.AlertID, &it.Severity, &it.Title, &it.QueuedAt, &it.Open); err != nil {
			return nil, fmt.Errorf("scan digest item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteDigestItems removes the named alerts from one person's waiting list -
// the ones just sent, and the ones skipped as no longer open. Items queued after
// the digest was read are not named, so they wait for the next one.
func (r *UserEmailRepository) DeleteDigestItems(ctx context.Context, userID string, alertIDs []string) error {
	if len(alertIDs) == 0 {
		return nil
	}
	if _, err := r.pool.Exec(ctx,
		`DELETE FROM email_digest_items WHERE user_id = $1 AND alert_id = ANY($2::uuid[])`, userID, alertIDs); err != nil {
		return fmt.Errorf("delete digest items: %w", err)
	}
	return nil
}

// DeleteDigestItemsOlderThan removes waiting items queued before cutoff - mail
// about something that happened days ago is no longer news - and returns how
// many.
func (r *UserEmailRepository) DeleteDigestItemsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM email_digest_items WHERE queued_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old digest items: %w", err)
	}
	return tag.RowsAffected(), nil
}
