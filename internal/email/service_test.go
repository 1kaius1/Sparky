// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/mailer"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var (
	admin      = rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}
	developer  = rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}
	breakGlass = rbac.Actor{IsSuperAdmin: true}
)

const relayPassword = "relay-password-do-not-leak"

type fakeSettings struct {
	mu       sync.Mutex
	cfg      db.EmailSettings
	restored bool
	getErr   error
}

func (f *fakeSettings) Get(context.Context) (*db.EmailSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c := f.cfg
	return &c, nil
}

func (f *fakeSettings) Update(_ context.Context, s db.EmailSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = s
	return nil
}

func (f *fakeSettings) Restore(_ context.Context, prev *db.EmailSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = true
	f.cfg = *prev
	return nil
}

func (f *fakeSettings) MarkDigestRun(_ context.Context, kind db.DigestKind, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == db.DigestHourly {
		f.cfg.LastHourlyDigestAt = &at
	} else {
		f.cfg.LastDailyDigestAt = &at
	}
	return nil
}

// fakeQueue behaves like the real queue where the service depends on it: Claim
// counts an attempt, leases the row and returns only what is due.
type fakeQueue struct {
	mu     sync.Mutex
	rows   []*db.Email
	now    func() time.Time
	nextID int

	enqueueErr error
}

func (q *fakeQueue) Enqueue(_ context.Context, in db.NewEmail) (*db.Email, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return nil, q.enqueueErr
	}
	q.nextID++
	e := &db.Email{ID: fmt.Sprintf("e%d", q.nextID), Kind: in.Kind, ToAddress: in.ToAddress, UserID: in.UserID, Subject: in.Subject, Body: in.Body,
		Status: db.EmailPending, NextAttemptAt: q.now(), CreatedAt: q.now()}
	q.rows = append(q.rows, e)
	return e, nil
}

func (q *fakeQueue) Claim(_ context.Context, limit int, lease time.Duration) ([]*db.Email, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*db.Email
	for _, e := range q.rows {
		if len(out) >= limit {
			break
		}
		if e.Status == db.EmailPending && !e.NextAttemptAt.After(q.now()) {
			e.Attempts++
			e.NextAttemptAt = q.now().Add(lease)
			c := *e
			out = append(out, &c)
		}
	}
	return out, nil
}

func (q *fakeQueue) find(id string) *db.Email {
	for _, e := range q.rows {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func (q *fakeQueue) MarkSent(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.find(id)
	t := q.now()
	e.Status, e.SentAt, e.LastError = db.EmailSent, &t, nil
	return nil
}

func (q *fakeQueue) MarkRetry(_ context.Context, id string, next time.Time, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.find(id)
	if e.Status == db.EmailPending {
		e.NextAttemptAt, e.LastError = next, &reason
	}
	return nil
}

func (q *fakeQueue) MarkFailed(_ context.Context, id string, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.find(id); e.Status == db.EmailPending {
		e.Status, e.LastError = db.EmailFailed, &reason
	}
	return nil
}

func (q *fakeQueue) FailOlderThan(_ context.Context, cutoff time.Time, reason string) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var n int64
	for _, e := range q.rows {
		if e.Status == db.EmailPending && e.CreatedAt.Before(cutoff) {
			e.Status, e.LastError = db.EmailFailed, &reason
			n++
		}
	}
	return n, nil
}

func (q *fakeQueue) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var kept []*db.Email
	var n int64
	for _, e := range q.rows {
		if e.Status != db.EmailPending && e.CreatedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, e)
	}
	q.rows = kept
	return n, nil
}

func (q *fakeQueue) Stats(context.Context) (*db.EmailQueueStats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := &db.EmailQueueStats{}
	for _, e := range q.rows {
		if e.Status == db.EmailPending {
			s.Pending++
		}
	}
	return s, nil
}

func (q *fakeQueue) Recent(_ context.Context, limit int) ([]*db.Email, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*db.Email(nil), q.rows...), nil
}

func (q *fakeQueue) get(id string) db.Email {
	q.mu.Lock()
	defer q.mu.Unlock()
	return *q.find(id)
}

type auditCall struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	detail     map[string]any
}

type fakeAudit struct {
	mu    sync.Mutex
	calls []auditCall
	err   error
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, superAdmin bool, action, objectType, _ string, detail map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, auditCall{actorID, superAdmin, action, objectType, detail})
	return nil
}

// sink records every message handed to the "relay" and answers from script.
type sink struct {
	mu       sync.Mutex
	sent     []mailer.Message
	configs  []mailer.Config
	failWith func(mailer.Message) error
}

func (s *sink) send(_ context.Context, cfg mailer.Config, m mailer.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs = append(s.configs, cfg)
	if s.failWith != nil {
		if err := s.failWith(m); err != nil {
			return err
		}
	}
	s.sent = append(s.sent, m)
	return nil
}

type fixture struct {
	svc      *Service
	settings *fakeSettings
	queue    *fakeQueue
	users    *fakeUsers
	audit    *fakeAudit
	sink     *sink
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clock: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	now := func() time.Time { return f.clock }
	f.settings = &fakeSettings{cfg: db.EmailSettings{Port: 587, TLSMode: db.EmailTLSStartTLS, FromName: "Sparky"}}
	f.queue = &fakeQueue{now: now}
	f.audit = &fakeAudit{}
	f.sink = &sink{}
	f.users = newFakeUsers(now)
	f.svc = NewService(f.settings, f.queue, f.users, f.audit, Env{SMTPPassword: relayPassword, SMTPCAFile: "/etc/ca.pem", PublicURL: "https://sparky.example.com"}, "sparky.example.com", log.New(io.Discard, "", 0))
	f.svc.now = now
	f.svc.send = f.sink.send
	return f
}

func validInput() SettingsInput {
	return SettingsInput{Enabled: true, Host: "relay.example.com", Port: 587, TLSMode: db.EmailTLSStartTLS, Username: "svc", FromAddress: "sparky@example.com", FromName: "Sparky", DailyDigestTime: "08:00"}
}

// enable turns email on in the fixture's saved settings.
func (f *fixture) enable() {
	f.settings.cfg = db.EmailSettings{Enabled: true, Host: "relay.example.com", Port: 587, TLSMode: db.EmailTLSStartTLS, Username: "svc", FromAddress: "sparky@example.com", FromName: "Sparky", DailyDigestTime: "08:00"}
}

func TestUpdateSettings_AdminOnly(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.UpdateSettings(context.Background(), developer, validInput()); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	if _, err := f.svc.Settings(context.Background(), developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer Settings: %v", err)
	}
	if err := f.svc.SendTest(context.Background(), developer, "ana@example.org"); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer SendTest: %v", err)
	}
	if f.settings.cfg.Enabled || len(f.audit.calls) != 0 || len(f.sink.sent) != 0 {
		t.Error("a refused request changed, audited or sent something")
	}
}

func TestUpdateSettings_RefusesWhatIsInvalid(t *testing.T) {
	bad := map[string]func(*SettingsInput){
		"port 0":                                       func(in *SettingsInput) { in.Port = 0 },
		"port 70000":                                   func(in *SettingsInput) { in.Port = 70000 },
		"unknown TLS mode":                             func(in *SettingsInput) { in.TLSMode = "ssl" },
		"host with a space":                            func(in *SettingsInput) { in.Host = "relay example" },
		"host with a slash":                            func(in *SettingsInput) { in.Host = "smtp://relay" },
		"on with no host":                              func(in *SettingsInput) { in.Host = "" },
		"on with no from address":                      func(in *SettingsInput) { in.FromAddress = "" },
		"a from address that isn't":                    func(in *SettingsInput) { in.FromAddress = "sparky" },
		"a from address with a name":                   func(in *SettingsInput) { in.FromAddress = "Sparky <sparky@example.com>" },
		"a newline in the from name":                   func(in *SettingsInput) { in.FromName = "Sparky\r\nBcc: x@y.com" },
		"a newline in the user name":                   func(in *SettingsInput) { in.Username = "svc\n" + "x" },
		"a user name over TLS none to another machine": func(in *SettingsInput) { in.TLSMode = db.EmailTLSNone },
		"a digest time of 24:00":                       func(in *SettingsInput) { in.DailyDigestTime = "24:00" },
		"a digest time of 8:00":                        func(in *SettingsInput) { in.DailyDigestTime = "8:00" },
		"a digest time of 08:60":                       func(in *SettingsInput) { in.DailyDigestTime = "08:60" },
		"an empty digest time":                         func(in *SettingsInput) { in.DailyDigestTime = "" },
		"a digest time with a newline":                 func(in *SettingsInput) { in.DailyDigestTime = "08:00\n09:00" },
	}
	for name, mutate := range bad {
		f := newFixture(t)
		in := validInput()
		mutate(&in)
		if err := f.svc.UpdateSettings(context.Background(), admin, in); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%s: %v, want ErrInvalidSettings", name, err)
		}
		if f.settings.cfg.Enabled || len(f.audit.calls) != 0 {
			t.Errorf("%s: a refused change was saved or audited", name)
		}
	}

	// A user name with no password in the environment cannot work.
	f := newFixture(t)
	f.svc.env.SMTPPassword = ""
	if err := f.svc.UpdateSettings(context.Background(), admin, validInput()); !errors.Is(err, ErrInvalidSettings) || !strings.Contains(err.Error(), "SMTP_PASSWORD") {
		t.Errorf("no SMTP_PASSWORD: %v", err)
	}

	// What is acceptable: off with nothing filled in, an anonymous relay, a
	// local relay with no encryption.
	f = newFixture(t)
	for name, in := range map[string]SettingsInput{
		"off, empty":          {Port: 587, TLSMode: db.EmailTLSStartTLS, DailyDigestTime: "08:00"},
		"anonymous relay":     {Enabled: true, Host: "relay.example.com", Port: 25, TLSMode: db.EmailTLSNone, FromAddress: "sparky@example.com", DailyDigestTime: "00:00"},
		"local relay, no TLS": {Enabled: true, Host: "localhost", Port: 25, TLSMode: db.EmailTLSNone, Username: "svc", FromAddress: "sparky@example.com", DailyDigestTime: "23:59"},
		"implicit TLS":        {Enabled: true, Host: "relay.example.com", Port: 465, TLSMode: db.EmailTLSImplicit, Username: "svc", FromAddress: "sparky@example.com", DailyDigestTime: " 17:30 "},
	} {
		if err := f.svc.UpdateSettings(context.Background(), admin, in); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestUpdateSettings_NormalizesAndAuditsWithoutTheSecret(t *testing.T) {
	f := newFixture(t)
	in := validInput()
	in.Host, in.FromAddress, in.Username = "  relay.example.com  ", "  sparky@example.com ", " svc "
	if err := f.svc.UpdateSettings(context.Background(), admin, in); err != nil {
		t.Fatal(err)
	}
	cfg := f.settings.cfg
	if cfg.Host != "relay.example.com" || cfg.FromAddress != "sparky@example.com" || cfg.Username != "svc" || !cfg.Enabled || cfg.UpdatedBy == nil || *cfg.UpdatedBy != "admin-1" {
		t.Errorf("saved %+v", cfg)
	}
	if len(f.audit.calls) != 1 {
		t.Fatalf("%d audit records, want 1", len(f.audit.calls))
	}
	c := f.audit.calls[0]
	if c.action != "updated_email_settings" || c.objectType != "email_settings" || c.actorID == nil || *c.actorID != "admin-1" || c.superAdmin ||
		c.detail["host"] != "relay.example.com" || c.detail["tls_mode"] != "starttls" || c.detail["username"] != "svc" || c.detail["previous_enabled"] != false {
		t.Errorf("audit = %+v", c)
	}
	if strings.Contains(fmt.Sprint(c.detail), relayPassword) {
		t.Error("the relay password reached the audit record")
	}

	f2 := newFixture(t)
	if err := f2.svc.UpdateSettings(context.Background(), breakGlass, validInput()); err != nil {
		t.Fatal(err)
	}
	if c := f2.audit.calls[0]; c.actorID != nil || !c.superAdmin || f2.settings.cfg.UpdatedBy != nil {
		t.Errorf("break-glass: %+v / UpdatedBy %v", c, f2.settings.cfg.UpdatedBy)
	}
}

func TestUpdateSettings_AFailedAuditWriteRevertsTheChange(t *testing.T) {
	f := newFixture(t)
	f.audit.err = errors.New("audit down")
	if err := f.svc.UpdateSettings(context.Background(), admin, validInput()); err == nil {
		t.Fatal("succeeded without an audit record")
	}
	if !f.settings.restored || f.settings.cfg.Enabled {
		t.Errorf("not reverted: %+v", f.settings.cfg)
	}
}

func TestSettingsView_ShowsWhatTheEnvironmentProvidesButNeverThePassword(t *testing.T) {
	f := newFixture(t)
	f.enable()
	if _, err := f.queue.Enqueue(context.Background(), db.NewEmail{Kind: db.EmailKindAlert, ToAddress: "ana@example.org", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Settings(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if !view.PasswordSet || !view.CAFileSet || view.PublicURL != "https://sparky.example.com" || view.Stats.Pending != 1 || len(view.Recent) != 1 {
		t.Errorf("view = %+v", view)
	}
	if strings.Contains(fmt.Sprintf("%+v", view), relayPassword) {
		t.Error("the relay password is reachable from the settings view")
	}
	f.svc.env = Env{}
	view, _ = f.svc.Settings(context.Background(), admin)
	if view.PasswordSet || view.CAFileSet || view.PublicURL != "" {
		t.Errorf("empty environment: %+v", view)
	}
}

func TestSendTest(t *testing.T) {
	ctx := context.Background()

	// Needs a saved relay and from address, even with email off.
	f := newFixture(t)
	if err := f.svc.SendTest(ctx, admin, "ana@example.org"); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("unconfigured: %v, want ErrInvalidSettings", err)
	}
	if len(f.sink.sent) != 0 {
		t.Error("sent with no relay configured")
	}

	// A bad address is refused before any send.
	f.enable()
	for _, to := range []string{"", "ana", "ana@example.org\r\nBcc: x@y.com", "Ana <ana@example.org>"} {
		if err := f.svc.SendTest(ctx, admin, to); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%q: %v", to, err)
		}
	}
	if len(f.sink.sent) != 0 || len(f.audit.calls) != 0 {
		t.Error("a refused address sent or audited something")
	}

	// It goes straight out (not via the queue), with the relay settings and the
	// environment's password, and is audited.
	f.settings.cfg.Enabled = false // a test must work before email is switched on
	if err := f.svc.SendTest(ctx, admin, "  ana@example.org "); err != nil {
		t.Fatal(err)
	}
	if len(f.sink.sent) != 1 || f.sink.sent[0].To != "ana@example.org" || !strings.Contains(f.sink.sent[0].Subject, "Test") {
		t.Fatalf("sent %+v", f.sink.sent)
	}
	mc := f.sink.configs[0]
	if mc.Host != "relay.example.com" || mc.Port != 587 || mc.TLSMode != mailer.TLSStartTLS || mc.Username != "svc" || mc.Password != relayPassword || mc.CAFile != "/etc/ca.pem" || mc.HelloName != "sparky.example.com" {
		t.Errorf("relay config = %+v", mc)
	}
	if len(f.queue.rows) != 0 {
		t.Error("a test email went through the queue")
	}
	calls := f.audit.calls
	if len(calls) != 1 || calls[0].action != "sent_test_email" || calls[0].detail["to"] != "ana@example.org" || calls[0].detail["sent"] != true {
		t.Errorf("audit = %+v", calls)
	}

	// A failure is returned with the relay's reason, and audited as not sent.
	f.sink.failWith = func(mailer.Message) error { return errors.New("connect to relay: connection refused") }
	err := f.svc.SendTest(ctx, admin, "ana@example.org")
	if !errors.Is(err, ErrSendFailed) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("relay down: %v", err)
	}
	if last := f.audit.calls[len(f.audit.calls)-1]; last.detail["sent"] != false {
		t.Errorf("a failed test was audited as sent: %+v", last)
	}

	// Failing to write the audit record does not turn a delivered test into an error.
	f.sink.failWith = nil
	f.audit.err = errors.New("audit down")
	if err := f.svc.SendTest(ctx, admin, "ana@example.org"); err != nil {
		t.Errorf("audit failure turned a sent test into an error: %v", err)
	}
}

func TestEnqueue(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	msg := Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "[Sparky] critical: x", Body: "body"}

	if err := f.svc.Enqueue(ctx, msg); !errors.Is(err, ErrDisabled) {
		t.Fatalf("email off: %v, want ErrDisabled", err)
	}
	if len(f.queue.rows) != 0 {
		t.Error("queued while email is off")
	}

	f.enable()
	uid := "user-1"
	msg.UserID = &uid
	if err := f.svc.Enqueue(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if len(f.queue.rows) != 1 {
		t.Fatalf("%d queued", len(f.queue.rows))
	}
	if e := f.queue.rows[0]; e.Kind != db.EmailKindAlert || e.ToAddress != "ana@example.org" || e.UserID == nil || *e.UserID != "user-1" || e.Subject != msg.Subject || e.Status != db.EmailPending {
		t.Errorf("queued %+v", e)
	}

	for name, m := range map[string]Message{
		"bad address":     {Kind: db.EmailKindAlert, To: "ana", Subject: "s", Body: "b"},
		"empty subject":   {Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "  ", Body: "b"},
		"newline subject": {Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "a\nb", Body: "b"},
		"long subject":    {Kind: db.EmailKindAlert, To: "ana@example.org", Subject: strings.Repeat("s", maxSubjectLen+1), Body: "b"},
		"huge body":       {Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: strings.Repeat("b", maxBodyLen+1)},
	} {
		before := len(f.queue.rows)
		if err := f.svc.Enqueue(ctx, m); err == nil || len(f.queue.rows) != before {
			t.Errorf("%s: %v, queued %d more", name, err, len(f.queue.rows)-before)
		}
	}
}

func TestProcessOnce_DeliversAndRecords(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.enable()
	for i := 0; i < 3; i++ {
		_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: fmt.Sprintf("u%d@example.org", i), Subject: fmt.Sprintf("s%d", i), Body: "b"})
	}
	if err := f.svc.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.sink.sent) != 3 {
		t.Fatalf("%d sent, want 3", len(f.sink.sent))
	}
	for _, e := range f.queue.rows {
		if e.Status != db.EmailSent || e.SentAt == nil || e.Attempts != 1 {
			t.Errorf("row %+v", e)
		}
	}
	// Nothing is sent twice.
	if err := f.svc.ProcessOnce(ctx); err != nil || len(f.sink.sent) != 3 {
		t.Errorf("second pass sent %d total, %v", len(f.sink.sent), err)
	}
}

func TestProcessOnce_ABatchIsBounded(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.enable()
	for i := 0; i < batchSize+4; i++ {
		_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})
	}
	_ = f.svc.ProcessOnce(ctx)
	if len(f.sink.sent) != batchSize {
		t.Errorf("%d sent in one pass, want %d", len(f.sink.sent), batchSize)
	}
	_ = f.svc.ProcessOnce(ctx)
	if len(f.sink.sent) != batchSize+4 {
		t.Errorf("%d sent after two passes, want all %d", len(f.sink.sent), batchSize+4)
	}
}

func TestProcessOnce_TransientFailuresAreRetriedWithBackoffThenGivenUp(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.enable()
	f.sink.failWith = func(mailer.Message) error { return errors.New("connect to relay: connection refused") }
	_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})

	wantWaits := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	for attempt := 1; attempt <= len(wantWaits); attempt++ {
		_ = f.svc.ProcessOnce(ctx)
		e := f.queue.get("e1")
		if e.Status != db.EmailPending || e.Attempts != attempt || e.LastError == nil || !strings.Contains(*e.LastError, "connection refused") {
			t.Fatalf("after attempt %d: %+v", attempt, e)
		}
		if got := e.NextAttemptAt.Sub(f.clock); got != wantWaits[attempt-1] {
			t.Errorf("after attempt %d the next try is in %v, want %v", attempt, got, wantWaits[attempt-1])
		}
		// Not tried again before it is due.
		before := len(f.sink.configs)
		_ = f.svc.ProcessOnce(ctx)
		if len(f.sink.configs) != before {
			t.Errorf("attempt %d was retried before its backoff passed", attempt)
		}
		f.clock = f.clock.Add(wantWaits[attempt-1])
	}
	_ = f.svc.ProcessOnce(ctx) // the sixth attempt
	e := f.queue.get("e1")
	if e.Status != db.EmailFailed || e.Attempts != maxAttempts || e.LastError == nil || !strings.Contains(*e.LastError, "gave up after 6 attempts") {
		t.Errorf("after the last attempt: %+v", e)
	}
	if len(f.sink.sent) != 0 {
		t.Error("a message that never went was recorded as sent")
	}
}

func TestProcessOnce_ARelayRefusalFailsAtOnceAndOtherMailStillGoes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.enable()
	f.sink.failWith = func(m mailer.Message) error {
		if m.To == "gone@example.org" {
			return fmt.Errorf("%w: RCPT TO: 550 5.1.1 no such user", mailer.ErrPermanent)
		}
		return nil
	}
	_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "gone@example.org", Subject: "s", Body: "b"})
	_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})
	_ = f.svc.ProcessOnce(ctx)
	if e := f.queue.get("e1"); e.Status != db.EmailFailed || e.Attempts != 1 || !strings.Contains(*e.LastError, "no such user") {
		t.Errorf("refused mail: %+v", e)
	}
	if e := f.queue.get("e2"); e.Status != db.EmailSent {
		t.Errorf("the next message was held up by the refused one: %+v", e)
	}
}

func TestProcessOnce_WhileOffNothingIsSentAndOldMailIsGivenUpOn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.enable()
	_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})
	f.settings.cfg.Enabled = false
	_ = f.svc.ProcessOnce(ctx)
	if len(f.sink.sent) != 0 || f.queue.get("e1").Status != db.EmailPending {
		t.Error("mail was sent, or given up on early, while email was off")
	}
	f.clock = f.clock.Add(25 * time.Hour)
	_ = f.svc.ProcessOnce(ctx)
	if e := f.queue.get("e1"); e.Status != db.EmailFailed || !strings.Contains(*e.LastError, "24 hours") {
		t.Errorf("a day-old email while off: %+v", e)
	}

	// Also while on: stale mail is not sent late.
	f.enable()
	_ = f.svc.Enqueue(ctx, Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})
	f.clock = f.clock.Add(25 * time.Hour)
	_ = f.svc.ProcessOnce(ctx)
	if e := f.queue.get("e2"); e.Status != db.EmailFailed || len(f.sink.sent) != 0 {
		t.Errorf("a day-old email while on: %+v, sent %d", e, len(f.sink.sent))
	}
}

func TestProcessOnce_StopsClaimedMailWhenShuttingDown(t *testing.T) {
	f := newFixture(t)
	f.enable()
	for i := 0; i < 3; i++ {
		_ = f.svc.Enqueue(context.Background(), Message{Kind: db.EmailKindAlert, To: "ana@example.org", Subject: "s", Body: "b"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.sink.failWith = func(mailer.Message) error { cancel(); return nil } // the first send cancels
	_ = f.svc.ProcessOnce(ctx)
	if len(f.sink.sent) != 1 {
		t.Errorf("%d sent after shutdown began, want 1", len(f.sink.sent))
	}
}

func TestClipKeepsStoredErrorsValid(t *testing.T) {
	if got := clip("plain"); got != "plain" {
		t.Errorf("clip(plain) = %q", got)
	}
	if got := clip("bad\xffbyte and \x00 nul"); !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
		t.Errorf("clip left invalid UTF-8 or a NUL: %q", got)
	}
	long := strings.Repeat("é", 400) // two bytes each, 800 bytes
	got := clip(long)
	if len(got) > 500 || !utf8.ValidString(got) {
		t.Errorf("clip(long) = %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.svc.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
