// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

func raisedAlert(sev db.AlertSeverity, title string) *db.Alert {
	return &db.Alert{
		ID: "alert-1", Kind: "stale_container", Severity: sev, Title: title, Body: "Container sparky-x has been stopped for 9 days.",
		LastRaisedAt: time.Date(2026, 10, 27, 12, 30, 0, 0, time.UTC), Occurrences: 1,
	}
}

func (f *fixture) queued() []db.Email {
	f.queue.mu.Lock()
	defer f.queue.mu.Unlock()
	out := make([]db.Email, 0, len(f.queue.rows))
	for _, r := range f.queue.rows {
		out = append(out, *r)
	}
	return out
}

func TestAlertRaised_ImmediateRecipientsGetOneEmailEach(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.users.recipients = []db.EmailRecipient{
		{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate},
		{UserID: "u2", Email: "bo@example.com", Delivery: db.EmailDeliveryImmediate},
	}
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityCritical, "Container is dead"), false)

	got := f.queued()
	if len(got) != 2 {
		t.Fatalf("%d emails queued, want 2", len(got))
	}
	m := got[0]
	if m.Kind != db.EmailKindAlert || m.ToAddress != "ana@example.com" || m.UserID == nil || *m.UserID != "u1" {
		t.Errorf("first email = %+v", m)
	}
	if m.Subject != "[Sparky] critical: Container is dead" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"Container is dead", "Severity: critical", "2026-10-27 12:30 UTC", "has been stopped for 9 days", "https://sparky.example.com/alerts?selected=alert-1", "https://sparky.example.com/account"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.Body)
		}
	}
	if got[1].ToAddress != "bo@example.com" {
		t.Errorf("second email went to %q", got[1].ToAddress)
	}
	if len(f.users.digest) != 0 {
		t.Error("an immediate recipient was also put in a digest")
	}
}

func TestAlertRaised_EscalationIsSaidSo(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.users.recipients = []db.EmailRecipient{{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate}}
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityCritical, "Disk is filling"), true)

	m := f.queued()[0]
	if m.Subject != "[Sparky] critical (escalated): Disk is filling" || !strings.Contains(m.Body, "has become more severe") {
		t.Errorf("escalation email = %q\n%s", m.Subject, m.Body)
	}
}

func TestAlertRaised_NoPublicURLMeansNoLink(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.svc.env.PublicURL = ""
	f.users.recipients = []db.EmailRecipient{{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate}}
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityWarning, "Something"), false)

	body := f.queued()[0].Body
	if strings.Contains(body, "http") {
		t.Errorf("a link was written though the server has no public address:\n%s", body)
	}
	if !strings.Contains(body, "Alerts inbox") {
		t.Errorf("body does not point at the inbox:\n%s", body)
	}
}

func TestAlertRaised_DigestRecipientsAreQueuedForTheirDigestNotMailed(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.users.recipients = []db.EmailRecipient{
		{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryHourly},
		{UserID: "u2", Email: "bo@example.com", Delivery: db.EmailDeliveryDaily},
		{UserID: "u3", Email: "cy@example.com", Delivery: db.EmailDeliveryOff},
	}
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityWarning, "Something"), false)

	if n := len(f.queued()); n != 0 {
		t.Errorf("%d emails queued at once, want none for digest and off recipients", n)
	}
	if len(f.users.digest["u1"]) != 1 || len(f.users.digest["u2"]) != 1 {
		t.Errorf("digest items = %+v", f.users.digest)
	}
	if len(f.users.digest["u3"]) != 0 {
		t.Error("a recipient who turned email off was put in a digest")
	}
}

func TestAlertRaised_DoesNothingWhenEmailIsOffOrLookupFails(t *testing.T) {
	f := newFixture(t) // email is off by default
	f.users.recipients = []db.EmailRecipient{{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate}}
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityCritical, "x"), false)
	if len(f.queued()) != 0 {
		t.Error("an email was queued while email is off")
	}

	f.enable()
	f.users.recipErr = errors.New("database is down")
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityCritical, "x"), false) // must not panic or return
	if len(f.queued()) != 0 {
		t.Error("an email was queued though the recipients could not be found")
	}

	f.users.recipErr = nil
	f.settings.getErr = errors.New("database is down")
	f.svc.AlertRaised(context.Background(), raisedAlert(db.AlertSeverityCritical, "x"), false)
	if len(f.queued()) != 0 {
		t.Error("an email was queued though the settings could not be read")
	}
}

func TestAlertRaised_ATitleCannotInjectHeadersOrLines(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.users.recipients = []db.EmailRecipient{{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate}}
	a := raisedAlert(db.AlertSeverityWarning, "Real\r\nBcc: attacker@example.com\r\n\r\nspoofed\x00\x07\x1b[31m\u0085  body")
	a.Body = "line one\r\nline two\x00\x07 bell"
	f.svc.AlertRaised(context.Background(), a, false)

	m := f.queued()[0]
	if strings.ContainsAny(m.Subject, "\r\n\x00\x07\x1b\u0085 ") {
		t.Errorf("subject has a line break or control character: %q", m.Subject)
	}
	if !strings.Contains(m.Subject, "Real Bcc: attacker@example.com spoofed") {
		t.Errorf("subject = %q, want the line breaks flattened to spaces", m.Subject)
	}
	if strings.ContainsAny(m.Body, "\r\x00\x07") {
		t.Errorf("body has control characters: %q", m.Body)
	}
	if !strings.Contains(m.Body, "line one\nline two") {
		t.Errorf("body lost its line breaks: %q", m.Body)
	}
}

func TestAlertRaised_LongTextIsCut(t *testing.T) {
	f := newFixture(t)
	f.enable()
	f.users.recipients = []db.EmailRecipient{{UserID: "u1", Email: "ana@example.com", Delivery: db.EmailDeliveryImmediate}}
	a := raisedAlert(db.AlertSeverityWarning, strings.Repeat("t", 5000))
	a.Body = strings.Repeat("b", 100000)
	f.svc.AlertRaised(context.Background(), a, false)

	m := f.queued()[0]
	if len(m.Subject) > 300 || len(m.Body) > 4000 {
		t.Errorf("subject %d bytes and body %d bytes were not cut", len(m.Subject), len(m.Body))
	}
}

func TestSetMyPreferences(t *testing.T) {
	ctx := context.Background()

	t.Run("saves and audits", func(t *testing.T) {
		f := newFixture(t)
		f.users.addLocal(localUser)
		err := f.svc.SetMyPreferences(ctx, localUser, MyPreferences{Delivery: db.EmailDeliveryDaily, MinSeverity: db.AlertSeverityCritical})
		if err != nil {
			t.Fatal(err)
		}
		u := f.users.users[localUser]
		if u.Delivery != db.EmailDeliveryDaily || u.MinSeverity != db.AlertSeverityCritical {
			t.Errorf("saved %+v", u)
		}
		if len(f.audit.calls) != 1 || f.audit.calls[0].action != "updated_email_preferences" || f.audit.calls[0].detail["previous_delivery"] != "immediate" {
			t.Errorf("audit = %+v", f.audit.calls)
		}
	})

	t.Run("an unchanged choice writes and audits nothing", func(t *testing.T) {
		f := newFixture(t)
		f.users.addLocal(localUser)
		if err := f.svc.SetMyPreferences(ctx, localUser, MyPreferences{Delivery: db.EmailDeliveryImmediate, MinSeverity: db.AlertSeverityWarning}); err != nil {
			t.Fatal(err)
		}
		if len(f.audit.calls) != 0 {
			t.Errorf("audit = %+v", f.audit.calls)
		}
	})

	t.Run("refuses values it does not know", func(t *testing.T) {
		f := newFixture(t)
		f.users.addLocal(localUser)
		for name, p := range map[string]MyPreferences{
			"delivery":     {Delivery: "weekly", MinSeverity: db.AlertSeverityWarning},
			"empty":        {Delivery: "", MinSeverity: db.AlertSeverityWarning},
			"severity":     {Delivery: db.EmailDeliveryOff, MinSeverity: "urgent"},
			"empty sev":    {Delivery: db.EmailDeliveryOff, MinSeverity: ""},
			"sql-ish text": {Delivery: "off'; --", MinSeverity: db.AlertSeverityWarning},
		} {
			if err := f.svc.SetMyPreferences(ctx, localUser, p); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("%s: %v, want ErrInvalidSettings", name, err)
			}
		}
		if u := f.users.users[localUser]; u.Delivery != db.EmailDeliveryImmediate || len(f.audit.calls) != 0 {
			t.Errorf("a refused change was kept or audited: %+v %+v", u, f.audit.calls)
		}
	})

	t.Run("a failed audit puts the old choice back", func(t *testing.T) {
		f := newFixture(t)
		f.users.addLocal(localUser)
		f.audit.err = errors.New("audit down")
		err := f.svc.SetMyPreferences(ctx, localUser, MyPreferences{Delivery: db.EmailDeliveryOff, MinSeverity: db.AlertSeverityCritical})
		if err == nil || !strings.Contains(err.Error(), "audit") {
			t.Fatalf("err = %v", err)
		}
		if u := f.users.users[localUser]; u.Delivery != db.EmailDeliveryImmediate || u.MinSeverity != db.AlertSeverityWarning {
			t.Errorf("preferences stayed changed after the audit failed: %+v", u)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		f := newFixture(t)
		err := f.svc.SetMyPreferences(ctx, "nobody", MyPreferences{Delivery: db.EmailDeliveryOff, MinSeverity: db.AlertSeverityInfo})
		if !errors.Is(err, db.ErrUserNotFound) {
			t.Errorf("err = %v", err)
		}
	})
}

// digestFixture has one person with an address who wants the given digest and
// has the given items waiting.
func digestFixture(t *testing.T, mode db.EmailDelivery, items ...db.DigestItem) *fixture {
	t.Helper()
	f := newFixture(t)
	f.enable()
	f.users.addLocal(localUser)
	addr := "ana@example.com"
	src := db.EmailSourceSelf
	u := f.users.users[localUser]
	u.Email, u.Source, u.Delivery = &addr, &src, mode
	f.users.digest[localUser] = items
	return f
}

func item(id string, sev db.AlertSeverity, title string, age time.Duration, f *fixture) db.DigestItem {
	return db.DigestItem{AlertID: id, Severity: sev, Title: title, QueuedAt: f.clock.Add(-age), Open: true}
}

func TestRunDigest_OneEmailListsOpenAlertsWorstFirstAndClearsThem(t *testing.T) {
	f := digestFixture(t, db.EmailDeliveryDaily)
	f.users.digest[localUser] = []db.DigestItem{
		item("a1", db.AlertSeverityWarning, "Disk filling", 3*time.Hour, f),
		item("a2", db.AlertSeverityCritical, "Instance dead", 2*time.Hour, f),
		{AlertID: "a3", Severity: db.AlertSeverityCritical, Title: "Already fixed", QueuedAt: f.clock.Add(-time.Hour), Open: false},
	}
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil {
		t.Fatal(err)
	}

	got := f.queued()
	if len(got) != 1 {
		t.Fatalf("%d emails queued, want 1", len(got))
	}
	m := got[0]
	if m.Kind != db.EmailKindDigest || m.ToAddress != "ana@example.com" || m.Subject != "[Sparky] 2 new alerts (daily digest)" {
		t.Errorf("digest = %q to %s kind %s", m.Subject, m.ToAddress, m.Kind)
	}
	if strings.Contains(m.Body, "Already fixed") {
		t.Errorf("a resolved alert was listed:\n%s", m.Body)
	}
	if ci, wi := strings.Index(m.Body, "Instance dead"), strings.Index(m.Body, "Disk filling"); ci < 0 || wi < 0 || ci > wi {
		t.Errorf("critical must come before warning:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "https://sparky.example.com/alerts?selected=a2") {
		t.Errorf("no link to the alert:\n%s", m.Body)
	}
	if len(f.users.digest[localUser]) != 0 {
		t.Errorf("items were left waiting after the digest was queued: %+v", f.users.digest[localUser])
	}
}

func TestRunDigest_SendsNothingWhenThereIsNothingOpenToList(t *testing.T) {
	f := digestFixture(t, db.EmailDeliveryHourly)
	f.users.digest[localUser] = []db.DigestItem{{AlertID: "a1", Severity: db.AlertSeverityCritical, Title: "Fixed", QueuedAt: f.clock, Open: false}}
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryHourly); err != nil {
		t.Fatal(err)
	}
	if len(f.queued()) != 0 {
		t.Error("an empty digest was sent")
	}
	if len(f.users.digest[localUser]) != 0 {
		t.Error("closed items were left to be looked at again next time")
	}
}

func TestRunDigest_ChangedMindsAndMissingAddressesSendNothing(t *testing.T) {
	cases := map[string]func(*db.UserEmail){
		"turned email off":          func(u *db.UserEmail) { u.Delivery = db.EmailDeliveryOff },
		"switched to immediate":     func(u *db.UserEmail) { u.Delivery = db.EmailDeliveryImmediate },
		"switched to the other one": func(u *db.UserEmail) { u.Delivery = db.EmailDeliveryHourly },
		"address removed":           func(u *db.UserEmail) { u.Email, u.Source = nil, nil },
		"raised the bar":            func(u *db.UserEmail) { u.MinSeverity = db.AlertSeverityCritical },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := digestFixture(t, db.EmailDeliveryDaily)
			f.users.digest[localUser] = []db.DigestItem{item("a1", db.AlertSeverityWarning, "Disk", time.Hour, f)}
			change(f.users.users[localUser])
			if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil {
				t.Fatal(err)
			}
			if n := len(f.queued()); n != 0 {
				t.Errorf("%d emails queued", n)
			}
		})
	}
}

func TestRunDigest_AFailedQueueLeavesItemsForNextTime(t *testing.T) {
	f := digestFixture(t, db.EmailDeliveryDaily)
	f.users.digest[localUser] = []db.DigestItem{item("a1", db.AlertSeverityWarning, "Disk", time.Hour, f)}
	f.queue.enqueueErr = errors.New("database is down")
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil {
		t.Fatal(err)
	}
	if len(f.users.digest[localUser]) != 1 {
		t.Errorf("items were dropped though the digest was never queued: %+v", f.users.digest[localUser])
	}

	// Next time it works, and nothing was lost.
	f.queue.enqueueErr = nil
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil {
		t.Fatal(err)
	}
	if len(f.queued()) != 1 || len(f.users.digest[localUser]) != 0 {
		t.Errorf("queued %d, waiting %d", len(f.queued()), len(f.users.digest[localUser]))
	}
}

func TestRunDigest_DropsStaleItemsAndHonoursOffAndMode(t *testing.T) {
	f := digestFixture(t, db.EmailDeliveryDaily)
	f.users.digest[localUser] = []db.DigestItem{item("old", db.AlertSeverityCritical, "Ancient", 72*time.Hour, f)}
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil {
		t.Fatal(err)
	}
	if len(f.queued()) != 0 || len(f.users.digest[localUser]) != 0 {
		t.Errorf("a 72 hour old item was mailed or kept: queued %d", len(f.queued()))
	}

	// The hourly run does not touch a daily person's items.
	f.users.digest[localUser] = []db.DigestItem{item("a1", db.AlertSeverityWarning, "Disk", time.Hour, f)}
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryHourly); err != nil {
		t.Fatal(err)
	}
	if len(f.queued()) != 0 || len(f.users.digest[localUser]) != 1 {
		t.Error("the hourly run handled a daily person")
	}

	// Email off: nothing is sent.
	f.settings.cfg.Enabled = false
	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryDaily); err != nil || len(f.queued()) != 0 {
		t.Errorf("email off: %v, queued %d", err, len(f.queued()))
	}

	if err := f.svc.RunDigest(context.Background(), db.EmailDeliveryImmediate); err == nil {
		t.Error("immediate is not a digest, but RunDigest accepted it")
	}
}

func TestDigestSpecs_AreValidAndFireOnTheHourAndAtTheDailyTime(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	now := time.Date(2026, 10, 27, 10, 15, 0, 0, loc)

	h := hourlySpec()
	if err := h.Validate(); err != nil {
		t.Fatalf("hourly spec: %v", err)
	}
	// Last digest was at 09:00; at 10:15 the 10:00 one is owed, and the 09:00
	// one is not owed again.
	got, ok := h.Latest(anchor, time.Date(2026, 10, 27, 9, 0, 0, 0, loc), now, loc)
	if !ok || !got.Equal(time.Date(2026, 10, 27, 10, 0, 0, 0, loc)) {
		t.Errorf("hourly latest = %v, %v", got, ok)
	}
	if _, ok := h.Latest(anchor, time.Date(2026, 10, 27, 10, 0, 0, 0, loc), now, loc); ok {
		t.Error("an hourly digest that already ran this hour is owed again")
	}

	d := dailySpec("08:00")
	if err := d.Validate(); err != nil {
		t.Fatalf("daily spec: %v", err)
	}
	got, ok = d.Latest(anchor, time.Date(2026, 10, 26, 8, 0, 0, 0, loc), now, loc)
	if !ok || !got.Equal(time.Date(2026, 10, 27, 8, 0, 0, 0, loc)) {
		t.Errorf("daily latest = %v, %v", got, ok)
	}
	if _, ok := d.Latest(anchor, time.Date(2026, 10, 27, 8, 0, 0, 0, loc), now, loc); ok {
		t.Error("a daily digest that already ran today is owed again")
	}
	// Before the daily time nothing is owed that was not already sent.
	if _, ok := d.Latest(anchor, time.Date(2026, 10, 27, 8, 0, 0, 0, loc), time.Date(2026, 10, 27, 7, 59, 0, 0, loc), loc); ok {
		t.Error("a daily digest is owed before its time")
	}
}

func TestPurge_RemovesOnlyExpiredConfirmations(t *testing.T) {
	f := newFixture(t)
	f.users.pending["expired"] = &db.EmailConfirmation{UserID: "expired", ExpiresAt: f.clock.Add(-time.Minute)}
	f.users.pending["live"] = &db.EmailConfirmation{UserID: "live", ExpiresAt: f.clock.Add(time.Hour)}
	f.svc.purge(context.Background())
	if _, ok := f.users.pending["expired"]; ok {
		t.Error("an expired confirmation was kept")
	}
	if _, ok := f.users.pending["live"]; !ok {
		t.Error("a live confirmation was removed")
	}
}

func TestDigestMessage_LongDigestIsCappedAndCounted(t *testing.T) {
	var entries []digestEntry
	for i := 0; i < 80; i++ {
		entries = append(entries, digestEntry{alertID: fmt.Sprintf("a%d", i), severity: db.AlertSeverityWarning, title: "t", queued: time.Unix(int64(i), 0)})
	}
	subject, body := digestMessage(entries, "hourly", time.UTC, "https://s.example.com")
	if subject != "[Sparky] 80 new alerts (hourly digest)" {
		t.Errorf("subject = %q", subject)
	}
	if n := strings.Count(body, "https://s.example.com/alerts?selected="); n != maxDigestItems {
		t.Errorf("%d alerts listed, want %d", n, maxDigestItems)
	}
	if !strings.Contains(body, "and 30 more") {
		t.Errorf("the rest were not counted:\n%s", body)
	}
	if s, _ := digestMessage(entries[:1], "daily", time.UTC, ""); s != "[Sparky] 1 new alert (daily digest)" {
		t.Errorf("singular subject = %q", s)
	}
}

func TestDigestJobs_ReadScheduleAndLastRunFromSettings(t *testing.T) {
	f := newFixture(t)
	f.enable()
	last := f.clock.Add(-time.Hour)
	setAt := f.clock.Add(-48 * time.Hour)
	f.settings.cfg.LastDailyDigestAt = &last
	f.settings.cfg.DigestScheduleSetAt = setAt
	f.settings.cfg.DailyDigestTime = "06:45"

	jobs := f.svc.DigestJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs", len(jobs))
	}
	hourly, daily := jobs[0], jobs[1]

	st, err := daily.Load(context.Background())
	if err != nil || !st.Enabled || !st.LastRun.Equal(last) || !st.Anchor.Equal(setAt) || len(st.Spec.Times) != 1 || st.Spec.Times[0] != "06:45" {
		t.Errorf("daily state = %+v, %v", st, err)
	}
	st, err = hourly.Load(context.Background())
	if err != nil || len(st.Spec.Times) != 24 || !st.LastRun.IsZero() {
		t.Errorf("hourly state = %d times, last run %v, %v", len(st.Spec.Times), st.LastRun, err)
	}

	f.settings.cfg.Enabled = false
	if st, _ := daily.Load(context.Background()); st.Enabled {
		t.Error("the digest is scheduled while email is off")
	}

	// Beginning a run records it against the right digest only.
	at := f.clock
	if err := hourly.Begin(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if f.settings.cfg.LastHourlyDigestAt == nil || !f.settings.cfg.LastHourlyDigestAt.Equal(at) || !f.settings.cfg.LastDailyDigestAt.Equal(last) {
		t.Errorf("hourly %v daily %v", f.settings.cfg.LastHourlyDigestAt, f.settings.cfg.LastDailyDigestAt)
	}
}
