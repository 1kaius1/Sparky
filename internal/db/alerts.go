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

// AlertSeverity mirrors the alert_severity enum, declared in ascending order.
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// Rank orders severities so "got worse" can be decided in Go; an unknown
// value ranks below info.
func (s AlertSeverity) Rank() int {
	switch s {
	case AlertSeverityInfo:
		return 1
	case AlertSeverityWarning:
		return 2
	case AlertSeverityCritical:
		return 3
	default:
		return 0
	}
}

// AlertResolution mirrors the alert_resolution enum.
type AlertResolution string

const (
	// AlertResolutionCleared means the condition that raised the alert went
	// away.
	AlertResolutionCleared AlertResolution = "cleared"
	// AlertResolutionUser means a person closed the alert.
	AlertResolutionUser AlertResolution = "user"
)

// AlertAudienceType mirrors the alert_audience_type enum.
type AlertAudienceType string

const (
	AlertAudienceUser AlertAudienceType = "user"
	AlertAudienceTier AlertAudienceType = "tier"
)

// AlertAudience is one addressee of an alert: a single user, or every user at
// MinTier and above. Exactly one of UserID and MinTier is set, matching Type.
type AlertAudience struct {
	Type    AlertAudienceType
	UserID  string
	MinTier Tier
}

// AlertListView selects which of a viewer's alerts a list returns.
type AlertListView string

const (
	// AlertViewUnread is open alerts the viewer has not read.
	AlertViewUnread AlertListView = "unread"
	// AlertViewAll is every alert the viewer has not dismissed, open or
	// resolved.
	AlertViewAll AlertListView = "all"
	// AlertViewResolved is resolved alerts the viewer has not dismissed.
	AlertViewResolved AlertListView = "resolved"
)

// ErrAlertNotFound is returned when an alert does not exist or is not visible
// to the viewer asking.
var ErrAlertNotFound = errors.New("alert not found")

// Alert mirrors an alerts row - see SCHEMA.md Alerts.
type Alert struct {
	ID            string
	Kind          string
	Severity      AlertSeverity
	Title         string
	Body          string
	ObjectType    *string
	ObjectID      *string
	DedupeKey     *string
	FirstRaisedAt time.Time
	LastRaisedAt  time.Time
	Occurrences   int
	ResolvedAt    *time.Time
	ResolvedBy    *string
	Resolution    *AlertResolution
}

// AlertView is an alert as one viewer sees it: the alert plus that viewer's
// own read time (nil means unread).
type AlertView struct {
	Alert
	ReadAt *time.Time
}

// AlertViewer identifies who is looking at alerts. UserID is empty for the
// break-glass SuperAdmin, which is not a users row: it matches tier
// audiences (as Admin) but no user audience and has no stored read state.
type AlertViewer struct {
	UserID string
	Tier   Tier
}

// NewAlert is the input to AlertRepository.Raise.
type NewAlert struct {
	Kind       string
	Severity   AlertSeverity
	Title      string
	Body       string
	ObjectType *string
	ObjectID   *string
	DedupeKey  *string
	Audiences  []AlertAudience
}

// RaiseResult reports what Raise did. Created is true for a new row;
// Escalated is true when a repeat of an open alert came in at a higher
// severity (which makes it unread again for everyone).
type RaiseResult struct {
	Alert     *Alert
	Created   bool
	Escalated bool
}

// AlertRepository is the only component that queries the alerts,
// alert_audiences and alert_user_state tables directly.
type AlertRepository struct {
	pool *pgxpool.Pool
}

// NewAlertRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewAlertRepository(pool *pgxpool.Pool) *AlertRepository {
	return &AlertRepository{pool: pool}
}

const alertColumns = `a.id, a.kind, a.severity, a.title, a.body, a.object_type, a.object_id, a.dedupe_key,
	a.first_raised_at, a.last_raised_at, a.occurrences, a.resolved_at, a.resolved_by, a.resolution`

func scanAlert(row pgx.Row) (*Alert, error) {
	var a Alert
	if err := row.Scan(&a.ID, &a.Kind, &a.Severity, &a.Title, &a.Body, &a.ObjectType, &a.ObjectID, &a.DedupeKey,
		&a.FirstRaisedAt, &a.LastRaisedAt, &a.Occurrences, &a.ResolvedAt, &a.ResolvedBy, &a.Resolution); err != nil {
		return nil, err
	}
	return &a, nil
}

func scanAlertView(row pgx.Row) (*AlertView, error) {
	var v AlertView
	if err := row.Scan(&v.ID, &v.Kind, &v.Severity, &v.Title, &v.Body, &v.ObjectType, &v.ObjectID, &v.DedupeKey,
		&v.FirstRaisedAt, &v.LastRaisedAt, &v.Occurrences, &v.ResolvedAt, &v.ResolvedBy, &v.Resolution, &v.ReadAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// visibleAlerts is the shared FROM/WHERE for everything a viewer may see: an
// alert addressed to them (by user id, or by a tier at or below theirs) that
// they have not dismissed. $1 is the viewer's user id (NULL for the
// SuperAdmin), $2 their tier. user_tier is declared in ascending order, so
// <= means "this tier and above".
const visibleAlerts = `FROM alerts a
	LEFT JOIN alert_user_state s ON s.alert_id = a.id AND s.user_id = $1::uuid
	WHERE s.dismissed_at IS NULL
	AND EXISTS (
		SELECT 1 FROM alert_audiences au
		WHERE au.alert_id = a.id AND (
			(au.audience_type = 'user' AND au.user_id = $1::uuid)
			OR (au.audience_type = 'tier' AND au.min_tier <= $2::user_tier)
		)
	)`

func viewerUserArg(v AlertViewer) *string {
	if v.UserID == "" {
		return nil
	}
	return &v.UserID
}

// Raise records an alert. With a DedupeKey that matches an unresolved alert,
// it updates that alert instead (one more occurrence, the latest title and
// body, any new audiences added) and reports whether the severity went up.
func (r *AlertRepository) Raise(ctx context.Context, in NewAlert) (*RaiseResult, error) {
	// Two concurrent first raises of one key race on the partial unique
	// index; the loser retries once and finds the winner's row.
	for attempt := 0; attempt < 2; attempt++ {
		res, err := r.raiseOnce(ctx, in)
		if err != nil && in.DedupeKey != nil && isUniqueViolation(err) {
			continue
		}
		return res, err
	}
	return nil, fmt.Errorf("raise alert %q: lost the dedupe race twice", *in.DedupeKey)
}

func (r *AlertRepository) raiseOnce(ctx context.Context, in NewAlert) (*RaiseResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin raise alert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingID string
	var existingSeverity AlertSeverity
	found := false
	if in.DedupeKey != nil {
		err := tx.QueryRow(ctx,
			`SELECT id, severity FROM alerts WHERE dedupe_key = $1 AND resolved_at IS NULL FOR UPDATE`,
			*in.DedupeKey).Scan(&existingID, &existingSeverity)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return nil, fmt.Errorf("look up open alert by dedupe key: %w", err)
		}
	}

	var result RaiseResult
	if found {
		result.Escalated = in.Severity.Rank() > existingSeverity.Rank()
		row := tx.QueryRow(ctx,
			`UPDATE alerts a SET last_raised_at = now(), occurrences = occurrences + 1, severity = $2, title = $3, body = $4
			 WHERE a.id = $1 RETURNING `+alertColumns,
			existingID, in.Severity, in.Title, in.Body)
		alert, err := scanAlert(row)
		if err != nil {
			return nil, fmt.Errorf("update open alert: %w", err)
		}
		result.Alert = alert
		if result.Escalated {
			if _, err := tx.Exec(ctx, `UPDATE alert_user_state SET read_at = NULL WHERE alert_id = $1`, existingID); err != nil {
				return nil, fmt.Errorf("mark escalated alert unread: %w", err)
			}
		}
	} else {
		row := tx.QueryRow(ctx,
			`INSERT INTO alerts AS a (kind, severity, title, body, object_type, object_id, dedupe_key)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+alertColumns,
			in.Kind, in.Severity, in.Title, in.Body, in.ObjectType, in.ObjectID, in.DedupeKey)
		alert, err := scanAlert(row)
		if err != nil {
			return nil, fmt.Errorf("insert alert: %w", err)
		}
		result.Alert = alert
		result.Created = true
	}

	for _, aud := range in.Audiences {
		var userID *string
		var minTier *Tier
		switch aud.Type {
		case AlertAudienceUser:
			id := aud.UserID
			userID = &id
		case AlertAudienceTier:
			t := aud.MinTier
			minTier = &t
		default:
			return nil, fmt.Errorf("unknown alert audience type %q", aud.Type)
		}
		// DO NOTHING covers a repeat naming an audience the alert already has.
		if _, err := tx.Exec(ctx,
			`INSERT INTO alert_audiences (alert_id, audience_type, user_id, min_tier) VALUES ($1, $2, $3, $4)
			 ON CONFLICT DO NOTHING`,
			result.Alert.ID, aud.Type, userID, minTier); err != nil {
			return nil, fmt.Errorf("insert alert audience: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit raise alert: %w", err)
	}
	return &result, nil
}

// ResolveByDedupeKey closes the unresolved alert with this key because its
// condition cleared, and returns the ids it closed (none when there is no
// open alert for the key).
func (r *AlertRepository) ResolveByDedupeKey(ctx context.Context, dedupeKey string) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`UPDATE alerts SET resolved_at = now(), resolution = 'cleared'
		 WHERE dedupe_key = $1 AND resolved_at IS NULL RETURNING id`, dedupeKey)
	if err != nil {
		return nil, fmt.Errorf("resolve alert by dedupe key: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resolved alert id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve alert by dedupe key: %w", err)
	}
	return ids, nil
}

// ResolveByUser closes an open alert on a person's behalf. resolvedBy is nil
// only for the break-glass SuperAdmin. It reports whether the alert changed
// (false when it was already resolved).
func (r *AlertRepository) ResolveByUser(ctx context.Context, id string, resolvedBy *string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE alerts SET resolved_at = now(), resolved_by = $2, resolution = 'user'
		 WHERE id = $1 AND resolved_at IS NULL`, id, resolvedBy)
	if err != nil {
		return false, fmt.Errorf("resolve alert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Reopen undoes ResolveByUser. It exists so the service can revert a
// resolution whose audit record could not be written.
func (r *AlertRepository) Reopen(ctx context.Context, id string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE alerts SET resolved_at = NULL, resolved_by = NULL, resolution = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("reopen alert: %w", err)
	}
	return nil
}

// FindVisible returns one alert as the viewer sees it, or ErrAlertNotFound
// when it does not exist, is not addressed to them, or they dismissed it - so
// a viewer cannot tell an alert exists that they may not see.
func (r *AlertRepository) FindVisible(ctx context.Context, viewer AlertViewer, id string) (*AlertView, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+alertColumns+`, s.read_at `+visibleAlerts+` AND a.id = $3::uuid`,
		viewerUserArg(viewer), viewer.Tier, id)
	v, err := scanAlertView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlertNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find alert: %w", err)
	}
	return v, nil
}

// ListVisible returns the viewer's alerts for view, newest activity first, at
// most limit of them.
func (r *AlertRepository) ListVisible(ctx context.Context, viewer AlertViewer, view AlertListView, limit int) ([]*AlertView, error) {
	filter := ""
	switch view {
	case AlertViewUnread:
		filter = ` AND a.resolved_at IS NULL AND s.read_at IS NULL`
	case AlertViewResolved:
		filter = ` AND a.resolved_at IS NOT NULL`
	case AlertViewAll:
	default:
		return nil, fmt.Errorf("unknown alert view %q", view)
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+alertColumns+`, s.read_at `+visibleAlerts+filter+` ORDER BY a.last_raised_at DESC, a.id LIMIT $3`,
		viewerUserArg(viewer), viewer.Tier, limit)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()
	var out []*AlertView
	for rows.Next() {
		v, err := scanAlertView(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return out, nil
}

// CountUnread is how many open alerts the viewer has not read - the sidebar
// badge. It is 0 for the SuperAdmin, which has no read state to count.
func (r *AlertRepository) CountUnread(ctx context.Context, viewer AlertViewer) (int, error) {
	if viewer.UserID == "" {
		return 0, nil
	}
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*) `+visibleAlerts+` AND a.resolved_at IS NULL AND s.read_at IS NULL`,
		viewerUserArg(viewer), viewer.Tier).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread alerts: %w", err)
	}
	return n, nil
}

// Audiences returns who an alert is addressed to.
func (r *AlertRepository) Audiences(ctx context.Context, alertID string) ([]AlertAudience, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT audience_type, user_id, min_tier FROM alert_audiences WHERE alert_id = $1`, alertID)
	if err != nil {
		return nil, fmt.Errorf("list alert audiences: %w", err)
	}
	defer rows.Close()
	var out []AlertAudience
	for rows.Next() {
		var aud AlertAudience
		var userID *string
		var minTier *Tier
		if err := rows.Scan(&aud.Type, &userID, &minTier); err != nil {
			return nil, fmt.Errorf("scan alert audience: %w", err)
		}
		if userID != nil {
			aud.UserID = *userID
		}
		if minTier != nil {
			aud.MinTier = *minTier
		}
		out = append(out, aud)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alert audiences: %w", err)
	}
	return out, nil
}

// MarkRead records that userID read the alert. It reports whether the state
// changed (false when it was already read).
func (r *AlertRepository) MarkRead(ctx context.Context, userID, alertID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO alert_user_state (alert_id, user_id, read_at) VALUES ($1, $2, now())
		 ON CONFLICT (alert_id, user_id) DO UPDATE SET read_at = now() WHERE alert_user_state.read_at IS NULL`,
		alertID, userID)
	if err != nil {
		return false, fmt.Errorf("mark alert read: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkUnread clears userID's read time. It reports whether the state changed.
func (r *AlertRepository) MarkUnread(ctx context.Context, userID, alertID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE alert_user_state SET read_at = NULL WHERE alert_id = $1 AND user_id = $2 AND read_at IS NOT NULL`,
		alertID, userID)
	if err != nil {
		return false, fmt.Errorf("mark alert unread: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Dismiss hides the alert from userID only. It reports whether the state
// changed (false when it was already dismissed).
func (r *AlertRepository) Dismiss(ctx context.Context, userID, alertID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO alert_user_state (alert_id, user_id, dismissed_at) VALUES ($1, $2, now())
		 ON CONFLICT (alert_id, user_id) DO UPDATE SET dismissed_at = now() WHERE alert_user_state.dismissed_at IS NULL`,
		alertID, userID)
	if err != nil {
		return false, fmt.Errorf("dismiss alert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RestoreDismissed undoes Dismiss. It exists so the service can revert a
// dismissal whose audit record could not be written.
func (r *AlertRepository) RestoreDismissed(ctx context.Context, userID, alertID string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE alert_user_state SET dismissed_at = NULL WHERE alert_id = $1 AND user_id = $2`, alertID, userID); err != nil {
		return fmt.Errorf("restore dismissed alert: %w", err)
	}
	return nil
}

// MarkAllRead marks every open, unread alert the viewer can see as read and
// returns their ids.
func (r *AlertRepository) MarkAllRead(ctx context.Context, viewer AlertViewer) ([]string, error) {
	if viewer.UserID == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx,
		`WITH target AS (SELECT a.id `+visibleAlerts+` AND a.resolved_at IS NULL AND s.read_at IS NULL)
		 INSERT INTO alert_user_state (alert_id, user_id, read_at)
		 SELECT id, $1::uuid, now() FROM target
		 ON CONFLICT (alert_id, user_id) DO UPDATE SET read_at = now() WHERE alert_user_state.read_at IS NULL
		 RETURNING alert_id`,
		viewer.UserID, viewer.Tier)
	if err != nil {
		return nil, fmt.Errorf("mark all alerts read: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan marked alert id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mark all alerts read: %w", err)
	}
	return ids, nil
}

// DeleteOlderThan removes every alert last raised before cutoff (its
// audiences and per-user state go with it) and returns how many it removed -
// the retention job's only write.
func (r *AlertRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM alerts WHERE last_raised_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired alerts: %w", err)
	}
	return tag.RowsAffected(), nil
}
