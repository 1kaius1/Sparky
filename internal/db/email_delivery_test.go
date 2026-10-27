// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestEmailSettingsRepository_DigestScheduleRestartsOnlyWhenTheTimeChanges(t *testing.T) {
	pool := newTestPool(t)
	repo := NewEmailSettingsRepository(pool)
	ctx := context.Background()
	original, _ := repo.Get(ctx)
	t.Cleanup(func() { _ = repo.Restore(ctx, original) })
	pause := func() { time.Sleep(10 * time.Millisecond) }

	base := EmailSettings{Port: 587, TLSMode: EmailTLSStartTLS, FromName: "Sparky", DailyDigestTime: "08:00"}
	if err := repo.Update(ctx, base); err != nil {
		t.Fatal(err)
	}
	first, _ := repo.Get(ctx)

	// A relay change leaves the digest schedule's clock alone.
	pause()
	other := base
	other.Host, other.FromAddress = "relay.example.com", "sparky@example.com"
	if err := repo.Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	same, _ := repo.Get(ctx)
	if !same.DigestScheduleSetAt.Equal(first.DigestScheduleSetAt) {
		t.Errorf("changing the relay restarted the digest schedule: %v -> %v", first.DigestScheduleSetAt, same.DigestScheduleSetAt)
	}

	// A new digest time restarts it.
	pause()
	other.DailyDigestTime = "17:30"
	if err := repo.Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	moved, _ := repo.Get(ctx)
	if moved.DailyDigestTime != "17:30" || !moved.DigestScheduleSetAt.After(first.DigestScheduleSetAt) {
		t.Errorf("changing the time: %+v", moved)
	}

	// Digest runs are recorded separately and survive a settings save.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.MarkDigestRun(ctx, DigestHourly, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkDigestRun(ctx, DigestDaily, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Get(ctx)
	if got.LastHourlyDigestAt == nil || !got.LastHourlyDigestAt.Equal(now) || got.LastDailyDigestAt == nil || !got.LastDailyDigestAt.Equal(now.Add(time.Minute)) {
		t.Errorf("digest run times = %v / %v", got.LastHourlyDigestAt, got.LastDailyDigestAt)
	}
	if err := repo.Restore(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, _ := repo.Get(ctx)
	if !back.DigestScheduleSetAt.Equal(got.DigestScheduleSetAt) || !back.LastDailyDigestAt.Equal(*got.LastDailyDigestAt) {
		t.Error("Restore() did not bring back the digest bookkeeping")
	}
}

func TestUserEmailRepository_PreferencesDefaultToWarningAndAboveImmediately(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	ctx := context.Background()
	u := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))

	got, err := repo.Get(ctx, u.ID)
	if err != nil || got.Delivery != EmailDeliveryImmediate || got.MinSeverity != AlertSeverityWarning {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	if err := repo.SetPreferences(ctx, u.ID, EmailDeliveryDaily, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.Get(ctx, u.ID)
	if got.Delivery != EmailDeliveryDaily || got.MinSeverity != AlertSeverityInfo {
		t.Errorf("after: %+v", got)
	}
	if all, _ := repo.ListAll(ctx); all[u.ID].Delivery != EmailDeliveryDaily {
		t.Error("ListAll() does not carry preferences")
	}
	if err := repo.SetPreferences(ctx, "00000000-0000-0000-0000-000000000000", EmailDeliveryOff, AlertSeverityInfo); err == nil {
		t.Error("preferences for an unknown user were accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email_delivery = 'weekly' WHERE id = $1`, u.ID); err == nil {
		t.Error("the database accepted an unknown delivery mode")
	}
}

func TestUserEmailRepository_RecipientsAreThosePeopleTheAlertIsAddressedToWhoWantIt(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	alerts, groups := NewAlertRepository(pool), NewAppGroupRepository(pool)
	ctx := context.Background()

	// The test database may hold other people (an earlier manual run, other
	// tests); only the ones made here are looked at.
	mine := map[string]bool{}
	mk := func(name string, tier Tier, addr string, delivery EmailDelivery, minSev AlertSeverity) *User {
		u := newTestLocalUser(t, users, name)
		if _, err := pool.Exec(ctx, `UPDATE users SET tier = $2 WHERE id = $1`, u.ID, tier); err != nil {
			t.Fatal(err)
		}
		if addr != "" {
			mine[addr] = true
			if _, err := repo.SetLocal(ctx, u.ID, &addr, EmailSourceAdmin); err != nil {
				t.Fatal(err)
			}
		}
		if err := repo.SetPreferences(ctx, u.ID, delivery, minSev); err != nil {
			t.Fatal(err)
		}
		return u
	}
	adminU := mk("admin", TierAdmin, "admin@example.org", EmailDeliveryImmediate, AlertSeverityWarning)
	mk("power", TierPowerDev, "power@example.org", EmailDeliveryDaily, AlertSeverityWarning)
	mk("dev", TierDeveloper, "dev@example.org", EmailDeliveryImmediate, AlertSeverityWarning)
	mk("infoer", TierAdmin, "info@example.org", EmailDeliveryImmediate, AlertSeverityInfo)
	mk("critonly", TierAdmin, "crit@example.org", EmailDeliveryImmediate, AlertSeverityCritical)
	mk("off", TierAdmin, "off@example.org", EmailDeliveryOff, AlertSeverityInfo)
	mk("noaddr", TierAdmin, "", EmailDeliveryImmediate, AlertSeverityInfo)
	viaUser := mk("viauser", TierReadOnly, "viauser@example.org", EmailDeliveryImmediate, AlertSeverityInfo)
	viaGroup := mk("viagroup", TierReadOnly, "viagroup@example.org", EmailDeliveryHourly, AlertSeverityInfo)
	viaAD := newTestADUser(t, users, "viaad", "CN=ViaAD,"+t.Name())
	adAddr := "viaad@example.org"
	mine[adAddr] = true
	if _, err := repo.SetDirectory(ctx, viaAD.ID, &adAddr); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPreferences(ctx, viaAD.ID, EmailDeliveryImmediate, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(t, groups, uniqueName(t, "Team"))
	adDN := "CN=Team," + t.Name() + ",DC=example,DC=internal"
	if err := groups.AddADMapping(ctx, g.ID, adDN, "Team", nil); err != nil {
		t.Fatal(err)
	}
	if err := groups.ReplaceUserMemberships(ctx, viaAD.ID, []string{adDN}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.AddLocalMember(ctx, g.ID, viaGroup.ID, nil); err != nil {
		t.Fatal(err)
	}

	raise := func(sev AlertSeverity, auds ...AlertAudience) string {
		return raiseTestAlert(t, alerts, NewAlert{Kind: "test_kind", Severity: sev, Title: "t", Audiences: auds}).Alert.ID
	}
	emails := func(alertID string, sev AlertSeverity) map[string]EmailDelivery {
		rs, err := repo.RecipientsForAlert(ctx, alertID, sev)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]EmailDelivery{}
		for _, r := range rs {
			if mine[r.Email] {
				out[r.Email] = r.Delivery
			}
		}
		return out
	}

	// A tier audience is that tier and above; severity and preferences filter.
	got := emails(raise(AlertSeverityWarning, AlertAudience{Type: AlertAudienceTier, MinTier: TierPowerDev}), AlertSeverityWarning)
	want := map[string]EmailDelivery{
		"admin@example.org": EmailDeliveryImmediate, "power@example.org": EmailDeliveryDaily, "info@example.org": EmailDeliveryImmediate,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("power_dev and above, warning:\n got  %v\n want %v\n(the Developer is below the tier; the critical-only, off and no-address people are filtered out)", got, want)
	}

	// An info alert goes only to those who asked for info; a critical one to
	// the critical-only person too.
	got = emails(raise(AlertSeverityInfo, AlertAudience{Type: AlertAudienceTier, MinTier: TierAdmin}), AlertSeverityInfo)
	if len(got) != 1 || got["info@example.org"] != EmailDeliveryImmediate {
		t.Errorf("an info alert to Admins: %v, want only the person who wants info", got)
	}
	got = emails(raise(AlertSeverityCritical, AlertAudience{Type: AlertAudienceTier, MinTier: TierAdmin}), AlertSeverityCritical)
	if got["crit@example.org"] != EmailDeliveryImmediate || got["admin@example.org"] != EmailDeliveryImmediate || got["off@example.org"] != "" || got["noaddr"] != "" {
		t.Errorf("a critical alert to Admins: %v", got)
	}

	// A user audience is exactly that person, whatever their tier.
	got = emails(raise(AlertSeverityWarning, AlertAudience{Type: AlertAudienceUser, UserID: viaUser.ID}), AlertSeverityWarning)
	if len(got) != 1 || got["viauser@example.org"] != EmailDeliveryImmediate {
		t.Errorf("a user audience: %v", got)
	}

	// A group audience reaches both a hand-added member and one in a mapped AD
	// group, and nobody else - not even an Admin.
	got = emails(raise(AlertSeverityWarning, AlertAudience{Type: AlertAudienceGroup, AppGroupID: g.ID}), AlertSeverityWarning)
	if len(got) != 2 || got["viagroup@example.org"] != EmailDeliveryHourly || got["viaad@example.org"] != EmailDeliveryImmediate {
		t.Errorf("a group audience: %v, want the manual member and the AD member only", got)
	}

	// Several audiences naming one person give one recipient, not two.
	rs, _ := repo.RecipientsForAlert(ctx, raise(AlertSeverityWarning,
		AlertAudience{Type: AlertAudienceUser, UserID: adminU.ID},
		AlertAudience{Type: AlertAudienceTier, MinTier: TierAdmin}), AlertSeverityWarning)
	seen := map[string]int{}
	for _, r := range rs {
		if mine[r.Email] {
			seen[r.Email]++
		}
	}
	if seen["admin@example.org"] != 1 {
		t.Errorf("a person addressed twice got %d recipient rows", seen["admin@example.org"])
	}
}

func TestUserEmailRepository_DigestItems(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	alerts := NewAlertRepository(pool)
	ctx := context.Background()
	u := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	other := createTestLocalUser(t, users, fmt.Sprintf("%s-o-%d", t.Name(), time.Now().UnixNano()))
	if err := repo.SetPreferences(ctx, u.ID, EmailDeliveryDaily, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPreferences(ctx, other.ID, EmailDeliveryHourly, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}
	raise := func(title string) *Alert {
		return raiseTestAlert(t, alerts, NewAlert{Kind: "test_kind", Severity: AlertSeverityWarning, Title: title,
			Audiences: []AlertAudience{{Type: AlertAudienceTier, MinTier: TierAdmin}}}).Alert
	}
	open, resolved, dismissed := raise("open"), raise("resolved"), raise("dismissed")

	for _, a := range []*Alert{open, resolved, dismissed} {
		if err := repo.AddDigestItem(ctx, u.ID, a.ID, AlertSeverityWarning, a.Title); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.AddDigestItem(ctx, other.ID, open.ID, AlertSeverityWarning, "open"); err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.ResolveByUser(ctx, resolved.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.Dismiss(ctx, u.ID, dismissed.ID); err != nil {
		t.Fatal(err)
	}

	// Escalation raises the stored severity; a lower repeat does not lower it.
	if err := repo.AddDigestItem(ctx, u.ID, open.ID, AlertSeverityCritical, "open (worse)"); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddDigestItem(ctx, u.ID, open.ID, AlertSeverityInfo, "open (quieter)"); err != nil {
		t.Fatal(err)
	}
	items, err := repo.PendingDigest(ctx, u.ID)
	if err != nil || len(items) != 3 {
		t.Fatalf("PendingDigest() = %d items, %v; want 3 (one row per alert)", len(items), err)
	}
	byTitle := map[string]DigestItem{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	if it := byTitle["open (quieter)"]; it.AlertID != open.ID || it.Severity != AlertSeverityCritical || !it.Open {
		t.Errorf("open alert = %+v, want critical (never lowered) and open", it)
	}
	if it := byTitle["resolved"]; it.Open {
		t.Error("a resolved alert is marked open")
	}
	if it := byTitle["dismissed"]; it.Open {
		t.Error("an alert the person dismissed is marked open")
	}

	// Who has something waiting, by mode.
	daily, _ := repo.UsersWithDigestItems(ctx, EmailDeliveryDaily)
	hourly, _ := repo.UsersWithDigestItems(ctx, EmailDeliveryHourly)
	has := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	if !has(daily, u.ID) || has(daily, other.ID) || !has(hourly, other.ID) || has(hourly, u.ID) {
		t.Errorf("daily %v, hourly %v", daily, hourly)
	}

	// Deleting names alerts; others stay.
	if err := repo.DeleteDigestItems(ctx, u.ID, []string{resolved.ID, dismissed.ID}); err != nil {
		t.Fatal(err)
	}
	if items, _ := repo.PendingDigest(ctx, u.ID); len(items) != 1 || items[0].AlertID != open.ID {
		t.Errorf("after deleting two: %+v", items)
	}
	if err := repo.DeleteDigestItems(ctx, u.ID, nil); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}

	// Old items are swept.
	if _, err := pool.Exec(ctx, `UPDATE email_digest_items SET queued_at = now() - interval '5 days' WHERE user_id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.DeleteDigestItemsOlderThan(ctx, time.Now().Add(-48*time.Hour)); err != nil || n < 1 {
		t.Fatalf("DeleteDigestItemsOlderThan() = %d, %v", n, err)
	}
	if items, _ := repo.PendingDigest(ctx, u.ID); len(items) != 0 {
		t.Errorf("an old item survived: %+v", items)
	}
	if items, _ := repo.PendingDigest(ctx, other.ID); len(items) != 1 {
		t.Error("a recent item was swept")
	}

	// Changing to immediate or off drops what was waiting; a digest mode keeps it.
	if err := repo.SetPreferences(ctx, other.ID, EmailDeliveryDaily, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}
	if items, _ := repo.PendingDigest(ctx, other.ID); len(items) != 1 {
		t.Error("switching between digests dropped the waiting items")
	}
	if err := repo.SetPreferences(ctx, other.ID, EmailDeliveryImmediate, AlertSeverityInfo); err != nil {
		t.Fatal(err)
	}
	if items, _ := repo.PendingDigest(ctx, other.ID); len(items) != 0 {
		t.Error("switching to immediate kept the waiting items")
	}

	// The alert going (retention) takes its items with it.
	if err := repo.AddDigestItem(ctx, u.ID, open.ID, AlertSeverityWarning, "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM alerts WHERE id = $1`, open.ID); err != nil {
		t.Fatal(err)
	}
	if items, _ := repo.PendingDigest(ctx, u.ID); len(items) != 0 {
		t.Error("a deleted alert's digest items survived")
	}
}
