// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func strptr(s string) *string { return &s }

// raiseTestAlert raises an alert and deletes it (and so its audiences and
// per-user state) when the test ends.
func raiseTestAlert(t *testing.T, repo *AlertRepository, in NewAlert) *RaiseResult {
	t.Helper()
	res, err := repo.Raise(context.Background(), in)
	if err != nil {
		t.Fatalf("Raise() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), `DELETE FROM alerts WHERE id = $1`, res.Alert.ID)
	})
	return res
}

func TestAlertRepository_RaiseDedupesAnOpenAlertAndEscalates(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAlertRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	u := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-u", t.Name()))
	key := "test-dedupe-" + t.Name()
	in := NewAlert{
		Kind: "test_kind", Severity: AlertSeverityWarning, Title: "first", Body: "b1",
		DedupeKey: &key, Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}},
	}

	first := raiseTestAlert(t, repo, in)
	if !first.Created || first.Alert.Occurrences != 1 {
		t.Fatalf("first raise: %+v", first)
	}

	// The viewer reads it, then a repeat at the same severity must not make it
	// unread again and must not add a row.
	viewer := AlertViewer{UserID: u.ID, Tier: TierReadOnly}
	if changed, err := repo.MarkRead(ctx, u.ID, first.Alert.ID); err != nil || !changed {
		t.Fatalf("MarkRead() = %v, %v", changed, err)
	}
	in.Title = "second"
	again, err := repo.Raise(ctx, in)
	if err != nil {
		t.Fatalf("repeat Raise() error: %v", err)
	}
	if again.Created || again.Escalated || again.Alert.ID != first.Alert.ID || again.Alert.Occurrences != 2 || again.Alert.Title != "second" {
		t.Fatalf("repeat raise: %+v", again)
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 0 {
		t.Errorf("unread after a same-severity repeat = %d, want 0", n)
	}

	// A higher severity escalates: same row, unread again.
	in.Severity = AlertSeverityCritical
	esc, err := repo.Raise(ctx, in)
	if err != nil {
		t.Fatalf("escalating Raise() error: %v", err)
	}
	if esc.Created || !esc.Escalated || esc.Alert.Severity != AlertSeverityCritical {
		t.Fatalf("escalating raise: %+v", esc)
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 1 {
		t.Errorf("unread after escalation = %d, want 1", n)
	}

	// Once resolved, the same key is a brand new alert.
	ids, err := repo.ResolveByDedupeKey(ctx, key)
	if err != nil || len(ids) != 1 || ids[0] != first.Alert.ID {
		t.Fatalf("ResolveByDedupeKey() = %v, %v", ids, err)
	}
	fresh := raiseTestAlert(t, repo, in)
	if !fresh.Created || fresh.Alert.ID == first.Alert.ID || fresh.Alert.Occurrences != 1 {
		t.Fatalf("raise after resolve: %+v", fresh)
	}
	if ids, _ := repo.ResolveByDedupeKey(ctx, "no-such-key-"+t.Name()); len(ids) != 0 {
		t.Errorf("resolving an unknown key closed %v", ids)
	}
}

func TestAlertRepository_VisibilityByUserAndTier(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAlertRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	owner := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-owner", t.Name()))
	other := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-other", t.Name()))

	personal := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "personal",
		Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: owner.ID}},
	}).Alert
	devs := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "developers and up",
		Audiences: []AlertAudience{{Type: AlertAudienceTier, MinTier: TierDeveloper}},
	}).Alert
	admins := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "admins",
		Audiences: []AlertAudience{{Type: AlertAudienceTier, MinTier: TierAdmin}},
	}).Alert

	visible := func(v AlertViewer) map[string]bool {
		t.Helper()
		list, err := repo.ListVisible(ctx, v, AlertViewAll, 100)
		if err != nil {
			t.Fatalf("ListVisible() error: %v", err)
		}
		seen := map[string]bool{}
		for _, a := range list {
			seen[a.ID] = true
		}
		return seen
	}

	cases := []struct {
		name   string
		viewer AlertViewer
		want   map[string]bool
	}{
		{"owner, read-only", AlertViewer{UserID: owner.ID, Tier: TierReadOnly}, map[string]bool{personal.ID: true}},
		{"owner, developer", AlertViewer{UserID: owner.ID, Tier: TierDeveloper}, map[string]bool{personal.ID: true, devs.ID: true}},
		{"other, read-only", AlertViewer{UserID: other.ID, Tier: TierReadOnly}, map[string]bool{}},
		{"other, power-dev", AlertViewer{UserID: other.ID, Tier: TierPowerDev}, map[string]bool{devs.ID: true}},
		{"other, admin", AlertViewer{UserID: other.ID, Tier: TierAdmin}, map[string]bool{devs.ID: true, admins.ID: true}},
		// The SuperAdmin has no user id: tier audiences as Admin, no user audience.
		{"superadmin", AlertViewer{Tier: TierAdmin}, map[string]bool{devs.ID: true, admins.ID: true}},
	}
	for _, c := range cases {
		got := visible(c.viewer)
		for id := range c.want {
			if !got[id] {
				t.Errorf("%s: alert %s should be visible", c.name, id)
			}
		}
		for _, mine := range []string{personal.ID, devs.ID, admins.ID} {
			if got[mine] && !c.want[mine] {
				t.Errorf("%s: alert %s should not be visible", c.name, mine)
			}
		}
	}

	// A viewer who cannot see an alert gets not-found, not a permission error.
	if _, err := repo.FindVisible(ctx, AlertViewer{UserID: other.ID, Tier: TierReadOnly}, personal.ID); err != ErrAlertNotFound {
		t.Errorf("FindVisible() for a hidden alert = %v, want ErrAlertNotFound", err)
	}
	if v, err := repo.FindVisible(ctx, AlertViewer{UserID: owner.ID, Tier: TierReadOnly}, personal.ID); err != nil || v.ID != personal.ID {
		t.Errorf("FindVisible() for the owner = %v, %v", v, err)
	}
}

func TestAlertRepository_ReadDismissAndViews(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAlertRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	u := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-u", t.Name()))
	viewer := AlertViewer{UserID: u.ID, Tier: TierReadOnly}
	mk := func(title string) *Alert {
		return raiseTestAlert(t, repo, NewAlert{
			Kind: "test_kind", Severity: AlertSeverityInfo, Title: title,
			Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}},
		}).Alert
	}
	a1, a2, a3 := mk("a1"), mk("a2"), mk("a3")

	count := func(view AlertListView) int {
		list, err := repo.ListVisible(ctx, viewer, view, 100)
		if err != nil {
			t.Fatalf("ListVisible(%s) error: %v", view, err)
		}
		return len(list)
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 3 || count(AlertViewUnread) != 3 || count(AlertViewAll) != 3 {
		t.Fatalf("initial counts wrong: unread=%d", n)
	}

	// Reading is idempotent: the second call reports no change.
	if changed, _ := repo.MarkRead(ctx, u.ID, a1.ID); !changed {
		t.Error("first MarkRead() reported no change")
	}
	if changed, _ := repo.MarkRead(ctx, u.ID, a1.ID); changed {
		t.Error("second MarkRead() reported a change")
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 2 {
		t.Errorf("unread after reading one = %d, want 2", n)
	}
	if changed, _ := repo.MarkUnread(ctx, u.ID, a1.ID); !changed {
		t.Error("MarkUnread() reported no change")
	}
	if changed, _ := repo.MarkUnread(ctx, u.ID, a1.ID); changed {
		t.Error("second MarkUnread() reported a change")
	}

	// Dismissing hides for this user only, in every view.
	if changed, _ := repo.Dismiss(ctx, u.ID, a2.ID); !changed {
		t.Error("Dismiss() reported no change")
	}
	if changed, _ := repo.Dismiss(ctx, u.ID, a2.ID); changed {
		t.Error("second Dismiss() reported a change")
	}
	if count(AlertViewAll) != 2 || count(AlertViewUnread) != 2 {
		t.Errorf("dismissed alert still listed: all=%d unread=%d", count(AlertViewAll), count(AlertViewUnread))
	}
	if err := repo.RestoreDismissed(ctx, u.ID, a2.ID); err != nil || count(AlertViewAll) != 3 {
		t.Errorf("RestoreDismissed() = %v, all=%d", err, count(AlertViewAll))
	}

	// Resolving moves an alert out of Unread and into Resolved, and out of the badge.
	if changed, err := repo.ResolveByUser(ctx, a3.ID, strptr(u.ID)); err != nil || !changed {
		t.Fatalf("ResolveByUser() = %v, %v", changed, err)
	}
	if changed, _ := repo.ResolveByUser(ctx, a3.ID, strptr(u.ID)); changed {
		t.Error("resolving twice reported a change")
	}
	if count(AlertViewResolved) != 1 || count(AlertViewUnread) != 2 || count(AlertViewAll) != 3 {
		t.Errorf("after resolve: resolved=%d unread=%d all=%d", count(AlertViewResolved), count(AlertViewUnread), count(AlertViewAll))
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 2 {
		t.Errorf("badge counts a resolved alert: %d", n)
	}
	got, err := repo.FindVisible(ctx, viewer, a3.ID)
	if err != nil || got.Resolution == nil || *got.Resolution != AlertResolutionUser || got.ResolvedBy == nil || *got.ResolvedBy != u.ID {
		t.Errorf("resolved alert: %+v, %v", got, err)
	}
	if err := repo.Reopen(ctx, a3.ID); err != nil || count(AlertViewResolved) != 0 {
		t.Errorf("Reopen() = %v, resolved=%d", err, count(AlertViewResolved))
	}

	// Mark all read returns exactly the alerts it changed.
	ids, err := repo.MarkAllRead(ctx, viewer)
	if err != nil || len(ids) != 3 {
		t.Fatalf("MarkAllRead() = %v, %v", ids, err)
	}
	if again, _ := repo.MarkAllRead(ctx, viewer); len(again) != 0 {
		t.Errorf("second MarkAllRead() changed %v", again)
	}
	if n, _ := repo.CountUnread(ctx, viewer); n != 0 {
		t.Errorf("unread after mark all = %d", n)
	}

	// The SuperAdmin has no read state to count or change.
	if n, _ := repo.CountUnread(ctx, AlertViewer{Tier: TierAdmin}); n != 0 {
		t.Errorf("SuperAdmin unread = %d, want 0", n)
	}
	if ids, err := repo.MarkAllRead(ctx, AlertViewer{Tier: TierAdmin}); err != nil || len(ids) != 0 {
		t.Errorf("SuperAdmin MarkAllRead() = %v, %v", ids, err)
	}
}

func TestAlertRepository_AudiencesAndShapeConstraint(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAlertRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	u := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-u", t.Name()))
	key := "test-aud-" + t.Name()
	res := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "t", DedupeKey: &key,
		Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}, {Type: AlertAudienceTier, MinTier: TierAdmin}},
	})
	// A repeat naming the same audiences, plus a new one, adds only the new one.
	if _, err := repo.Raise(ctx, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "t", DedupeKey: &key,
		Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}, {Type: AlertAudienceTier, MinTier: TierDeveloper}},
	}); err != nil {
		t.Fatalf("repeat Raise() error: %v", err)
	}
	auds, err := repo.Audiences(ctx, res.Alert.ID)
	if err != nil || len(auds) != 3 {
		t.Fatalf("Audiences() = %v, %v; want 3", auds, err)
	}

	// The database refuses a row whose shape does not match its type.
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_audiences (alert_id, audience_type, user_id, min_tier) VALUES ($1, 'tier', $2, 'admin')`,
		res.Alert.ID, u.ID); err == nil {
		t.Error("a tier audience that also names a user was accepted")
	}
	// A resolved row without a resolution is refused too.
	if _, err := pool.Exec(ctx, `UPDATE alerts SET resolved_at = now() WHERE id = $1`, res.Alert.ID); err == nil {
		t.Error("resolved_at without a resolution was accepted")
	}
}

func TestAlertRepository_DeleteOlderThanCascades(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAlertRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	u := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-u", t.Name()))
	old := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "old",
		Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}},
	}).Alert
	recent := raiseTestAlert(t, repo, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "recent",
		Audiences: []AlertAudience{{Type: AlertAudienceUser, UserID: u.ID}},
	}).Alert
	if _, err := repo.MarkRead(ctx, u.ID, old.ID); err != nil {
		t.Fatalf("MarkRead() error: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET last_raised_at = now() - interval '400 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatalf("age the alert: %v", err)
	}

	n, err := repo.DeleteOlderThan(ctx, time.Now().AddDate(0, -12, 0))
	if err != nil || n < 1 {
		t.Fatalf("DeleteOlderThan() = %d, %v", n, err)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE id = $1`, old.ID).Scan(&rows)
	if rows != 0 {
		t.Error("the old alert survived")
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alert_audiences WHERE alert_id = $1`, old.ID).Scan(&rows)
	if rows != 0 {
		t.Error("the old alert's audiences survived")
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alert_user_state WHERE alert_id = $1`, old.ID).Scan(&rows)
	if rows != 0 {
		t.Error("the old alert's per-user state survived")
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE id = $1`, recent.ID).Scan(&rows)
	if rows != 1 {
		t.Error("the recent alert was deleted")
	}
}
