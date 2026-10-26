// SPDX-License-Identifier: AGPL-3.0-or-later

package alerts

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	alertA  = "11111111-1111-1111-1111-111111111111"
	alertB  = "22222222-2222-2222-2222-222222222222"
	userOne = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	userTwo = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// fakeStore is an in-memory stand-in for *db.AlertRepository. It models only
// what the service's rules depend on: which alerts a viewer can see, and
// whether a per-user state change really changed something.
type fakeStore struct {
	visible   map[string]*db.AlertView // by alert id; every listed alert is visible to every viewer
	audiences map[string][]db.AlertAudience
	read      map[string]bool // "user/alert" -> read
	dismissed map[string]bool
	resolved  map[string]bool

	raiseResult *db.RaiseResult
	raised      []db.NewAlert
	resolveKeys map[string][]string // dedupe key -> ids it closes
	markAllIDs  []string
	deleted     int64
	cutoff      time.Time

	calls []string
	err   error // returned by every call when set
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		visible: map[string]*db.AlertView{}, audiences: map[string][]db.AlertAudience{},
		read: map[string]bool{}, dismissed: map[string]bool{}, resolved: map[string]bool{},
		resolveKeys: map[string][]string{},
	}
}

func (f *fakeStore) add(id, kind string, auds ...db.AlertAudience) {
	f.visible[id] = &db.AlertView{Alert: db.Alert{ID: id, Kind: kind, Title: kind, Severity: db.AlertSeverityWarning}}
	f.audiences[id] = auds
}

func key(user, alert string) string { return user + "/" + alert }

func (f *fakeStore) Raise(_ context.Context, in db.NewAlert) (*db.RaiseResult, error) {
	f.calls = append(f.calls, "Raise")
	f.raised = append(f.raised, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.raiseResult != nil {
		return f.raiseResult, nil
	}
	return &db.RaiseResult{Alert: &db.Alert{ID: alertA}, Created: true}, nil
}

func (f *fakeStore) ResolveByDedupeKey(_ context.Context, k string) ([]string, error) {
	f.calls = append(f.calls, "ResolveByDedupeKey")
	return f.resolveKeys[k], f.err
}

func (f *fakeStore) ResolveByUser(_ context.Context, id string, _ *string) (bool, error) {
	f.calls = append(f.calls, "ResolveByUser")
	if f.err != nil {
		return false, f.err
	}
	if f.resolved[id] {
		return false, nil
	}
	f.resolved[id] = true
	return true, nil
}

func (f *fakeStore) Reopen(_ context.Context, id string) error {
	f.calls = append(f.calls, "Reopen")
	delete(f.resolved, id)
	return nil
}

func (f *fakeStore) FindVisible(_ context.Context, _ db.AlertViewer, id string) (*db.AlertView, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.visible[id]
	if !ok {
		return nil, db.ErrAlertNotFound
	}
	cp := *v
	if f.resolved[id] {
		now := time.Now()
		cp.ResolvedAt = &now
	}
	return &cp, nil
}

func (f *fakeStore) ListVisible(_ context.Context, _ db.AlertViewer, view db.AlertListView, limit int) ([]*db.AlertView, error) {
	f.calls = append(f.calls, "ListVisible:"+string(view))
	if f.err != nil {
		return nil, f.err
	}
	var out []*db.AlertView
	for _, v := range f.visible {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeStore) CountUnread(_ context.Context, _ db.AlertViewer) (int, error) {
	return len(f.visible), f.err
}

func (f *fakeStore) Audiences(_ context.Context, id string) ([]db.AlertAudience, error) {
	return f.audiences[id], f.err
}

func (f *fakeStore) MarkRead(_ context.Context, user, alert string) (bool, error) {
	f.calls = append(f.calls, "MarkRead")
	if f.err != nil {
		return false, f.err
	}
	if f.read[key(user, alert)] {
		return false, nil
	}
	f.read[key(user, alert)] = true
	return true, nil
}

func (f *fakeStore) MarkUnread(_ context.Context, user, alert string) (bool, error) {
	f.calls = append(f.calls, "MarkUnread")
	if f.err != nil {
		return false, f.err
	}
	if !f.read[key(user, alert)] {
		return false, nil
	}
	delete(f.read, key(user, alert))
	return true, nil
}

func (f *fakeStore) Dismiss(_ context.Context, user, alert string) (bool, error) {
	f.calls = append(f.calls, "Dismiss")
	if f.err != nil {
		return false, f.err
	}
	if f.dismissed[key(user, alert)] {
		return false, nil
	}
	f.dismissed[key(user, alert)] = true
	return true, nil
}

func (f *fakeStore) RestoreDismissed(_ context.Context, user, alert string) error {
	f.calls = append(f.calls, "RestoreDismissed")
	delete(f.dismissed, key(user, alert))
	return nil
}

func (f *fakeStore) MarkAllRead(_ context.Context, _ db.AlertViewer) ([]string, error) {
	f.calls = append(f.calls, "MarkAllRead")
	for _, id := range f.markAllIDs {
		f.read[key(userOne, id)] = true
	}
	return f.markAllIDs, f.err
}

func (f *fakeStore) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	f.cutoff = cutoff
	return f.deleted, f.err
}

type fakeSettings struct {
	months    int
	updatedBy *string
	updates   []int
	err       error
}

func (f *fakeSettings) Get(context.Context) (*db.AlertSettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &db.AlertSettings{RetentionMonths: f.months, UpdatedBy: f.updatedBy}, nil
}

func (f *fakeSettings) Update(_ context.Context, months int, by *string) error {
	f.updates = append(f.updates, months)
	f.months, f.updatedBy = months, by
	return nil
}

type auditEntry struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	objectID   string
	detail     map[string]any
}

type fakeAudit struct {
	entries []auditEntry
	fail    bool
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, isSuperAdmin bool, action, objectType, objectID string, detail map[string]any) error {
	if f.fail {
		return errors.New("audit down")
	}
	f.entries = append(f.entries, auditEntry{actorID, isSuperAdmin, action, objectType, objectID, detail})
	return nil
}

type fakePub struct{ n int }

func (f *fakePub) Publish(e events.Event) {
	if e.Type == EventAlertsChanged {
		f.n++
	}
}

type harness struct {
	svc   *Service
	store *fakeStore
	set   *fakeSettings
	aud   *fakeAudit
	pub   *fakePub
}

func newHarness() *harness {
	h := &harness{store: newFakeStore(), set: &fakeSettings{months: 12}, aud: &fakeAudit{}, pub: &fakePub{}}
	h.svc = NewService(h.store, h.set, h.aud, h.pub, log.New(io.Discard, "", 0))
	return h
}

func dev(id string) rbac.Actor   { return rbac.Actor{UserID: id, Tier: db.TierDeveloper} }
func admin(id string) rbac.Actor { return rbac.Actor{UserID: id, Tier: db.TierAdmin} }

var superAdmin = rbac.Actor{IsSuperAdmin: true}

func userAud(id string) db.AlertAudience {
	return db.AlertAudience{Type: db.AlertAudienceUser, UserID: id}
}
func tierAud(t db.Tier) db.AlertAudience {
	return db.AlertAudience{Type: db.AlertAudienceTier, MinTier: t}
}

func validParams() RaiseParams {
	return RaiseParams{
		Kind: "instance_dead", Severity: db.AlertSeverityCritical, Title: "Instance died", Body: "body",
		ObjectType: "running_instance", ObjectID: alertB, DedupeKey: "instance_dead:" + alertB,
		Audiences: []db.AlertAudience{userAud(userOne), tierAud(db.TierAdmin)},
	}
}

func TestRaise_RejectsInvalidInput(t *testing.T) {
	long := make([]rune, MaxTitleLen+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := map[string]func(*RaiseParams){
		"empty kind":            func(p *RaiseParams) { p.Kind = "" },
		"kind with capitals":    func(p *RaiseParams) { p.Kind = "Bad Kind" },
		"unknown severity":      func(p *RaiseParams) { p.Severity = "urgent" },
		"blank title":           func(p *RaiseParams) { p.Title = "   " },
		"title too long":        func(p *RaiseParams) { p.Title = string(long) },
		"object type, no id":    func(p *RaiseParams) { p.ObjectID = "" },
		"malformed object type": func(p *RaiseParams) { p.ObjectType = "Not A Type" },
		"no audience":           func(p *RaiseParams) { p.Audiences = nil },
		"user audience, no id":  func(p *RaiseParams) { p.Audiences = []db.AlertAudience{{Type: db.AlertAudienceUser}} },
		"unknown tier":          func(p *RaiseParams) { p.Audiences = []db.AlertAudience{tierAud("superuser")} },
		"unknown audience":      func(p *RaiseParams) { p.Audiences = []db.AlertAudience{{Type: "group"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			p := validParams()
			mutate(&p)
			if _, err := h.svc.Raise(context.Background(), p); !errors.Is(err, ErrInvalidAlert) {
				t.Fatalf("Raise() error = %v, want ErrInvalidAlert", err)
			}
			if len(h.store.raised) != 0 {
				t.Error("an invalid alert reached the store")
			}
		})
	}
}

func TestRaise_PublishesOnlyWhenSomethingNewHappened(t *testing.T) {
	cases := []struct {
		name      string
		result    *db.RaiseResult
		wantEvent int
	}{
		{"new alert", &db.RaiseResult{Alert: &db.Alert{ID: alertA}, Created: true}, 1},
		{"escalation", &db.RaiseResult{Alert: &db.Alert{ID: alertA}, Escalated: true}, 1},
		{"plain repeat", &db.RaiseResult{Alert: &db.Alert{ID: alertA}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.store.raiseResult = c.result
			if _, err := h.svc.Raise(context.Background(), validParams()); err != nil {
				t.Fatalf("Raise() error: %v", err)
			}
			if h.pub.n != c.wantEvent {
				t.Errorf("events = %d, want %d", h.pub.n, c.wantEvent)
			}
			if len(h.aud.entries) != 0 {
				t.Error("a system raise was audited; the alert row is its record")
			}
		})
	}

	// Optional fields reach the store as nil, not as empty strings.
	h := newHarness()
	p := validParams()
	p.ObjectType, p.ObjectID, p.DedupeKey = "", "", ""
	if _, err := h.svc.Raise(context.Background(), p); err != nil {
		t.Fatalf("Raise() error: %v", err)
	}
	if got := h.store.raised[0]; got.ObjectType != nil || got.ObjectID != nil || got.DedupeKey != nil {
		t.Errorf("empty optional fields were stored as %+v", got)
	}
}

func TestResolveCondition(t *testing.T) {
	h := newHarness()
	h.store.resolveKeys["k"] = []string{alertA}

	if n, err := h.svc.ResolveCondition(context.Background(), "nothing-open"); err != nil || n != 0 || h.pub.n != 0 {
		t.Errorf("no open alert: n=%d err=%v events=%d", n, err, h.pub.n)
	}
	if n, err := h.svc.ResolveCondition(context.Background(), "k"); err != nil || n != 1 || h.pub.n != 1 {
		t.Errorf("open alert: n=%d err=%v events=%d", n, err, h.pub.n)
	}
	if len(h.aud.entries) != 0 {
		t.Error("an automatic resolve was audited")
	}
	h.store.err = errors.New("db down")
	if _, err := h.svc.ResolveCondition(context.Background(), "k"); err == nil {
		t.Error("a store error was swallowed")
	}
}

func TestMarkRead_AuditsOnlyTheTransition(t *testing.T) {
	h := newHarness()
	h.store.add(alertA, "instance_dead", userAud(userOne))
	actor := dev(userOne)

	changed, err := h.svc.MarkRead(context.Background(), actor, alertA)
	if err != nil || !changed {
		t.Fatalf("first MarkRead() = %v, %v", changed, err)
	}
	if len(h.aud.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(h.aud.entries))
	}
	e := h.aud.entries[0]
	if e.action != "read_alert" || e.objectType != "alert" || e.objectID != alertA || e.superAdmin || e.actorID == nil || *e.actorID != userOne {
		t.Errorf("audit entry = %+v", e)
	}
	if e.detail["kind"] != "instance_dead" {
		t.Errorf("audit detail = %v", e.detail)
	}
	if h.pub.n != 1 {
		t.Errorf("events = %d, want 1", h.pub.n)
	}

	// Opening it again changes nothing, so it is neither audited nor announced.
	changed, err = h.svc.MarkRead(context.Background(), actor, alertA)
	if err != nil || changed || len(h.aud.entries) != 1 || h.pub.n != 1 {
		t.Errorf("second MarkRead() = %v, %v, entries=%d events=%d", changed, err, len(h.aud.entries), h.pub.n)
	}
}

func TestMarkRead_RevertsWhenAuditFails(t *testing.T) {
	h := newHarness()
	h.store.add(alertA, "instance_dead", userAud(userOne))
	h.aud.fail = true

	changed, err := h.svc.MarkRead(context.Background(), dev(userOne), alertA)
	if err == nil || changed {
		t.Fatalf("MarkRead() = %v, %v; want an error and no change", changed, err)
	}
	if h.store.read[key(userOne, alertA)] {
		t.Error("the alert stayed read after its audit record failed")
	}
	if h.pub.n != 0 {
		t.Error("browsers were told about a change that was reverted")
	}
}

func TestMarkUnreadAndDismiss(t *testing.T) {
	h := newHarness()
	h.store.add(alertA, "k", userAud(userOne))
	ctx := context.Background()
	actor := dev(userOne)

	if changed, _ := h.svc.MarkUnread(ctx, actor, alertA); changed {
		t.Error("marking an unread alert unread reported a change")
	}
	if _, err := h.svc.MarkRead(ctx, actor, alertA); err != nil {
		t.Fatal(err)
	}
	if changed, err := h.svc.MarkUnread(ctx, actor, alertA); err != nil || !changed {
		t.Fatalf("MarkUnread() = %v, %v", changed, err)
	}
	if changed, err := h.svc.Dismiss(ctx, actor, alertA); err != nil || !changed {
		t.Fatalf("Dismiss() = %v, %v", changed, err)
	}

	var actions []string
	for _, e := range h.aud.entries {
		actions = append(actions, e.action)
	}
	want := []string{"read_alert", "marked_alert_unread", "dismissed_alert"}
	if len(actions) != len(want) {
		t.Fatalf("audited %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audited %v, want %v", actions, want)
		}
	}

	// A failed audit write un-dismisses.
	h.store.add(alertB, "k", userAud(userOne))
	h.aud.fail = true
	if _, err := h.svc.Dismiss(ctx, actor, alertB); err == nil {
		t.Fatal("Dismiss() succeeded with a failing audit")
	}
	if h.store.dismissed[key(userOne, alertB)] {
		t.Error("the alert stayed dismissed after its audit record failed")
	}
}

func TestUserActions_UnknownOrHiddenAlertIsNotFound(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	for _, id := range []string{alertA, "not-a-uuid", ""} {
		if _, err := h.svc.MarkRead(ctx, dev(userOne), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("MarkRead(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := h.svc.Dismiss(ctx, dev(userOne), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Dismiss(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := h.svc.ResolveAlert(ctx, admin(userOne), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveAlert(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := h.svc.Get(ctx, dev(userOne), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestSuperAdmin_HasNoPerUserState(t *testing.T) {
	h := newHarness()
	h.store.add(alertA, "k", tierAud(db.TierAdmin))
	ctx := context.Background()

	for name, fn := range map[string]func() (bool, error){
		"MarkRead":   func() (bool, error) { return h.svc.MarkRead(ctx, superAdmin, alertA) },
		"MarkUnread": func() (bool, error) { return h.svc.MarkUnread(ctx, superAdmin, alertA) },
		"Dismiss":    func() (bool, error) { return h.svc.Dismiss(ctx, superAdmin, alertA) },
	} {
		if changed, err := fn(); err != nil || changed {
			t.Errorf("%s() = %v, %v; want a quiet no-op", name, changed, err)
		}
	}
	if n, err := h.svc.MarkAllRead(ctx, superAdmin); err != nil || n != 0 {
		t.Errorf("MarkAllRead() = %d, %v", n, err)
	}
	if len(h.aud.entries) != 0 {
		t.Errorf("no-ops were audited: %+v", h.aud.entries)
	}

	// It reads everything as already read, and has nothing unread to list.
	items, err := h.svc.List(ctx, superAdmin, db.AlertViewAll, 0)
	if err != nil || len(items) != 1 || !items[0].Read {
		t.Errorf("List(all) = %+v, %v", items, err)
	}
	if items, _ := h.svc.List(ctx, superAdmin, db.AlertViewUnread, 0); len(items) != 0 {
		t.Errorf("List(unread) for the SuperAdmin = %d items", len(items))
	}

	// But it can resolve (Admin level), and that is audited as the SuperAdmin.
	changed, err := h.svc.ResolveAlert(ctx, superAdmin, alertA)
	if err != nil || !changed {
		t.Fatalf("ResolveAlert() = %v, %v", changed, err)
	}
	e := h.aud.entries[len(h.aud.entries)-1]
	if e.action != "resolved_alert" || !e.superAdmin || e.actorID != nil {
		t.Errorf("audit entry = %+v", e)
	}
}

func TestResolveAlert_Permissions(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		actor rbac.Actor
		auds  []db.AlertAudience
		want  bool
	}{
		{"admin, tier alert", admin(userTwo), []db.AlertAudience{tierAud(db.TierDeveloper)}, true},
		{"sole addressee", dev(userOne), []db.AlertAudience{userAud(userOne)}, true},
		{"addressee among several", dev(userOne), []db.AlertAudience{userAud(userOne), tierAud(db.TierAdmin)}, false},
		{"developer on a tier alert", dev(userOne), []db.AlertAudience{tierAud(db.TierDeveloper)}, false},
		{"someone else's personal alert", dev(userTwo), []db.AlertAudience{userAud(userOne)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.store.add(alertA, "k", c.auds...)
			changed, err := h.svc.ResolveAlert(ctx, c.actor, alertA)
			if c.want {
				if err != nil || !changed {
					t.Fatalf("ResolveAlert() = %v, %v; want allowed", changed, err)
				}
				return
			}
			if !errors.Is(err, rbac.ErrNotPermitted) {
				t.Fatalf("ResolveAlert() error = %v, want ErrNotPermitted", err)
			}
			if h.store.resolved[alertA] || len(h.aud.entries) != 0 {
				t.Error("a refused resolve changed state or was audited")
			}
		})
	}
}

func TestResolveAlert_AlreadyResolvedAndAuditFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	h.store.add(alertA, "k", tierAud(db.TierDeveloper))

	if changed, err := h.svc.ResolveAlert(ctx, admin(userTwo), alertA); err != nil || !changed {
		t.Fatalf("ResolveAlert() = %v, %v", changed, err)
	}
	if changed, err := h.svc.ResolveAlert(ctx, admin(userTwo), alertA); err != nil || changed || len(h.aud.entries) != 1 {
		t.Errorf("second ResolveAlert() = %v, %v, entries=%d; want no change and no second record", changed, err, len(h.aud.entries))
	}

	h2 := newHarness()
	h2.store.add(alertA, "k", tierAud(db.TierDeveloper))
	h2.aud.fail = true
	if _, err := h2.svc.ResolveAlert(ctx, admin(userTwo), alertA); err == nil {
		t.Fatal("ResolveAlert() succeeded with a failing audit")
	}
	if h2.store.resolved[alertA] {
		t.Error("the alert stayed resolved after its audit record failed")
	}
}

func TestGet_CanResolve(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	h.store.add(alertA, "k", userAud(userOne))
	h.store.add(alertB, "k", tierAud(db.TierDeveloper))

	if d, err := h.svc.Get(ctx, dev(userOne), alertA); err != nil || !d.CanResolve {
		t.Errorf("sole addressee: %+v, %v", d, err)
	}
	if d, err := h.svc.Get(ctx, dev(userOne), alertB); err != nil || d.CanResolve {
		t.Errorf("developer on a tier alert: %+v, %v", d, err)
	}
	if d, err := h.svc.Get(ctx, admin(userTwo), alertB); err != nil || !d.CanResolve {
		t.Errorf("admin: %+v, %v", d, err)
	}
	// Viewing never changes read state or writes the audit log.
	if len(h.aud.entries) != 0 || len(h.store.read) != 0 {
		t.Error("Get changed state")
	}
	// A resolved alert has nothing left to resolve.
	h.store.resolved[alertB] = true
	if d, _ := h.svc.Get(ctx, admin(userTwo), alertB); d.CanResolve {
		t.Error("a resolved alert offers Resolve")
	}
}

func TestMarkAllRead(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	actor := dev(userOne)

	if n, err := h.svc.MarkAllRead(ctx, actor); err != nil || n != 0 || len(h.aud.entries) != 0 || h.pub.n != 0 {
		t.Errorf("nothing to mark: n=%d err=%v entries=%d events=%d", n, err, len(h.aud.entries), h.pub.n)
	}

	h.store.markAllIDs = []string{alertA, alertB}
	n, err := h.svc.MarkAllRead(ctx, actor)
	if err != nil || n != 2 {
		t.Fatalf("MarkAllRead() = %d, %v", n, err)
	}
	if len(h.aud.entries) != 1 {
		t.Fatalf("audit entries = %d, want one entry for the whole batch", len(h.aud.entries))
	}
	if e := h.aud.entries[0]; e.action != "marked_all_alerts_read" || e.detail["count"] != 2 || e.objectID != zeroUUID {
		t.Errorf("audit entry = %+v", e)
	}

	// A failed audit write un-reads every alert it marked.
	h2 := newHarness()
	h2.store.markAllIDs = []string{alertA, alertB}
	h2.aud.fail = true
	if _, err := h2.svc.MarkAllRead(ctx, actor); err == nil {
		t.Fatal("MarkAllRead() succeeded with a failing audit")
	}
	if len(h2.store.read) != 0 {
		t.Errorf("alerts stayed read after the audit failed: %v", h2.store.read)
	}
}

func TestList_DefaultsAndClamps(t *testing.T) {
	h := newHarness()
	h.store.add(alertA, "k", userAud(userOne))
	ctx := context.Background()

	if _, err := h.svc.List(ctx, dev(userOne), "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.List(ctx, dev(userOne), "bogus", 100000); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.List(ctx, dev(userOne), db.AlertViewResolved, 5); err != nil {
		t.Fatal(err)
	}
	want := []string{"ListVisible:unread", "ListVisible:unread", "ListVisible:resolved"}
	for i, w := range want {
		if h.store.calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, h.store.calls[i], w)
		}
	}
	if len(h.aud.entries) != 0 {
		t.Error("listing was audited")
	}
}

func TestUpdateRetention(t *testing.T) {
	ctx := context.Background()

	t.Run("permission", func(t *testing.T) {
		h := newHarness()
		if err := h.svc.UpdateRetention(ctx, dev(userOne), 6); !errors.Is(err, rbac.ErrNotPermitted) {
			t.Errorf("developer: %v, want ErrNotPermitted", err)
		}
		if _, err := h.svc.Settings(ctx, dev(userOne)); !errors.Is(err, rbac.ErrNotPermitted) {
			t.Errorf("Settings() for a developer: %v", err)
		}
	})
	t.Run("range", func(t *testing.T) {
		h := newHarness()
		for _, bad := range []int{0, -3, 25} {
			if err := h.svc.UpdateRetention(ctx, admin(userOne), bad); !errors.Is(err, ErrInvalidRetention) {
				t.Errorf("UpdateRetention(%d) = %v, want ErrInvalidRetention", bad, err)
			}
		}
		if len(h.set.updates) != 0 {
			t.Error("an out-of-range value reached the store")
		}
	})
	t.Run("audited with previous value", func(t *testing.T) {
		h := newHarness()
		if err := h.svc.UpdateRetention(ctx, admin(userOne), 6); err != nil {
			t.Fatal(err)
		}
		e := h.aud.entries[0]
		if e.action != "updated_alert_retention" || e.objectType != "alert_settings" || e.detail["retention_months"] != 6 || e.detail["previous_retention_months"] != 12 {
			t.Errorf("audit entry = %+v", e)
		}
	})
	t.Run("reverted when the audit fails", func(t *testing.T) {
		h := newHarness()
		h.aud.fail = true
		if err := h.svc.UpdateRetention(ctx, admin(userOne), 6); err == nil {
			t.Fatal("succeeded with a failing audit")
		}
		if h.set.months != 12 {
			t.Errorf("retention stayed %d after the audit failed", h.set.months)
		}
	})
}

func TestExpireOnce(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	h := newHarness()
	h.svc.now = func() time.Time { return fixed }
	h.set.months = 3

	if n, err := h.svc.ExpireOnce(ctx); err != nil || n != 0 || len(h.aud.entries) != 0 || h.pub.n != 0 {
		t.Errorf("nothing expired: n=%d err=%v entries=%d events=%d", n, err, len(h.aud.entries), h.pub.n)
	}
	if want := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC); !h.store.cutoff.Equal(want) {
		t.Errorf("cutoff = %v, want %v", h.store.cutoff, want)
	}

	h.store.deleted = 4
	n, err := h.svc.ExpireOnce(ctx)
	if err != nil || n != 4 || h.pub.n != 1 {
		t.Fatalf("ExpireOnce() = %d, %v, events=%d", n, err, h.pub.n)
	}
	// Audited as the system: no actor, and not the break-glass account.
	e := h.aud.entries[0]
	if e.action != "expired_alerts" || e.actorID != nil || e.superAdmin || e.detail["deleted"] != int64(4) {
		t.Errorf("audit entry = %+v", e)
	}

	h.aud.fail = true
	if n, err := h.svc.ExpireOnce(ctx); err == nil || n != 4 {
		t.Errorf("a failed audit record was not reported: n=%d err=%v", n, err)
	}
}
