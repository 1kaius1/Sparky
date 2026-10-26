// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	testAlertID  = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	testAlertID2 = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
)

// fakeAlerts implements alertManager. It records what the handlers asked for
// and returns canned data; the service's own rules (visibility, audit,
// permission to resolve) are tested in internal/alerts, so here only the
// Settings calls repeat the real tier rule.
type fakeAlerts struct {
	items  []*alerts.Item
	detail map[string]*alerts.Detail
	unread int

	listedViews []db.AlertListView
	calls       []string

	// actionErr, if set, is returned by every state-changing call; errFor
	// overrides it per alert id.
	actionErr error
	errFor    map[string]error

	markAllCount int

	retention    int
	retentionErr error
	updatedTo    []int
}

func (f *fakeAlerts) List(_ context.Context, _ rbac.Actor, view db.AlertListView, _ int) ([]*alerts.Item, error) {
	f.listedViews = append(f.listedViews, view)
	return f.items, nil
}

func (f *fakeAlerts) Get(_ context.Context, _ rbac.Actor, id string) (*alerts.Detail, error) {
	if d, ok := f.detail[id]; ok {
		return d, nil
	}
	return nil, alerts.ErrNotFound
}

func (f *fakeAlerts) UnreadCount(context.Context, rbac.Actor) (int, error) { return f.unread, nil }

func (f *fakeAlerts) act(name, id string) (bool, error) {
	f.calls = append(f.calls, name+":"+id)
	if err, ok := f.errFor[id]; ok {
		return false, err
	}
	return true, f.actionErr
}

func (f *fakeAlerts) MarkRead(_ context.Context, _ rbac.Actor, id string) (bool, error) {
	return f.act("read", id)
}
func (f *fakeAlerts) MarkUnread(_ context.Context, _ rbac.Actor, id string) (bool, error) {
	return f.act("unread", id)
}
func (f *fakeAlerts) Dismiss(_ context.Context, _ rbac.Actor, id string) (bool, error) {
	return f.act("dismiss", id)
}
func (f *fakeAlerts) ResolveAlert(_ context.Context, _ rbac.Actor, id string) (bool, error) {
	return f.act("resolve", id)
}
func (f *fakeAlerts) MarkAllRead(context.Context, rbac.Actor) (int, error) {
	f.calls = append(f.calls, "markall")
	return f.markAllCount, f.actionErr
}

func (f *fakeAlerts) Settings(_ context.Context, actor rbac.Actor) (*db.AlertSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	m := f.retention
	if m == 0 {
		m = 12
	}
	return &db.AlertSettings{RetentionMonths: m, UpdatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeAlerts) UpdateRetention(_ context.Context, actor rbac.Actor, months int) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if f.retentionErr != nil {
		return f.retentionErr
	}
	f.updatedTo = append(f.updatedTo, months)
	return nil
}

func sampleAlertItem(id, title string, read bool) *alerts.Item {
	return &alerts.Item{
		Alert: db.Alert{
			ID: id, Kind: "instance_dead", Severity: db.AlertSeverityCritical, Title: title, Body: "body",
			FirstRaisedAt: time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC),
			LastRaisedAt:  time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC),
			Occurrences:   1,
		},
		Read: read,
	}
}

func alertsAPI(t *testing.T, tier db.Tier, fake *fakeAlerts) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{})
	api.alerts = fake
	return api, "u-1"
}

func TestAlertsInbox_ListsRowsWithReadStateSeverityAndTabs(t *testing.T) {
	multi := sampleAlertItem(testAlertID2, "Second", true)
	multi.Occurrences = 12
	multi.Severity = db.AlertSeverityInfo
	fake := &fakeAlerts{items: []*alerts.Item{sampleAlertItem(testAlertID, "Qwen3 is no longer running", false), multi}}
	api, uid := alertsAPI(t, db.TierDeveloper, fake)

	rec := getAs(t, api, uid, "/alerts?view=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Qwen3 is no longer running", `class="alert-row unread"`, `class="alert-row"`,
		"Critical", "Info", "x12", "2026-10-26 02:00:00",
		`href="/alerts?view=unread"`, `href="/alerts?view=all"`, `href="/alerts?view=resolved"`,
		`name="ids" value="` + testAlertID + `"`, "Mark all read", "Mark selected read", "Mark selected unread", "Dismiss selected",
		`data-sse-topics="alerts_changed"`, "Select an alert to read it.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox is missing %q", want)
		}
	}
	if len(fake.listedViews) != 1 || fake.listedViews[0] != db.AlertViewAll {
		t.Errorf("listed views = %v, want [all]", fake.listedViews)
	}
	// Looking at the inbox changes nothing and records nothing.
	if len(fake.calls) != 0 {
		t.Errorf("a GET called %v", fake.calls)
	}
}

// htmx attributes are inherited by child elements. The inbox's bulk form sets
// hx-swap="none" for its own POST, so a row link inside it that does not set
// its own hx-swap inherits "none": the click fetches the alert and then throws
// the response away, and nothing seems to happen. Found in manual testing.
func TestAlertsInbox_RowLinksSetTheirOwnSwapInsideTheBulkForm(t *testing.T) {
	fake := &fakeAlerts{items: []*alerts.Item{sampleAlertItem(testAlertID, "one", false), sampleAlertItem(testAlertID2, "two", true)}}
	api, uid := alertsAPI(t, db.TierDeveloper, fake)
	body := getAs(t, api, uid, "/alerts").Body.String()

	links := regexp.MustCompile(`<a class="alert-row-link"[^>]*>`).FindAllString(body, -1)
	if len(links) != 2 {
		t.Fatalf("found %d row links, want 2", len(links))
	}
	for _, link := range links {
		if !strings.Contains(link, `hx-swap="innerHTML"`) {
			t.Errorf("row link has no hx-swap of its own, so it inherits the bulk form's %q: %s", `hx-swap="none"`, link)
		}
	}
}

func TestAlertsInbox_ViewDefaultsToUnreadForAnythingUnknown(t *testing.T) {
	for _, v := range []string{"", "bogus", "UNREAD", "all%20", "<script>"} {
		fake := &fakeAlerts{}
		api, uid := alertsAPI(t, db.TierDeveloper, fake)
		if rec := getAs(t, api, uid, "/alerts?view="+url.QueryEscape(v)); rec.Code != http.StatusOK {
			t.Fatalf("view %q: status %d", v, rec.Code)
		}
		if len(fake.listedViews) != 1 || fake.listedViews[0] != db.AlertViewUnread {
			t.Errorf("view %q listed %v, want unread", v, fake.listedViews)
		}
	}
}

func TestAlertsInbox_EmptyStates(t *testing.T) {
	api, uid := alertsAPI(t, db.TierDeveloper, &fakeAlerts{})
	for view, want := range map[string]string{"unread": "Nothing unread.", "all": "No alerts.", "resolved": "No resolved alerts."} {
		if body := getAs(t, api, uid, "/alerts?view="+view).Body.String(); !strings.Contains(body, want) {
			t.Errorf("view %s is missing %q", view, want)
		}
	}
}

func TestAlertsInbox_OpenAlertShowsDetailAndFiresMarkReadOnlyWhenUnread(t *testing.T) {
	item := sampleAlertItem(testAlertID, "Qwen3 is no longer running", false)
	item.Body = "line one <b>bold</b>\nline two"
	typ := "running_instance"
	item.ObjectType = &typ
	fake := &fakeAlerts{
		items:  []*alerts.Item{item},
		detail: map[string]*alerts.Detail{testAlertID: {Item: item, CanResolve: true}},
	}
	api, uid := alertsAPI(t, db.TierAdmin, fake)

	body := getAs(t, api, uid, "/alerts?view=unread&selected="+testAlertID).Body.String()
	for _, want := range []string{
		"line one &lt;b&gt;bold&lt;/b&gt;",                                // the body is escaped
		`hx-post="/alerts/` + testAlertID + `/read"`, `hx-trigger="load"`, // unread: marks itself read
		"Dismiss", "Resolve for everyone", "View the Dashboard", `href="/dashboard"`,
		"First raised", "2026-10-26 01:00:00", `class="alert-row unread selected"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail is missing %q", want)
		}
	}
	if strings.Contains(body, "Mark unread") {
		t.Error("an unread alert offers Mark unread")
	}
	if strings.Contains(body, "<b>bold</b>") {
		t.Error("the alert body was not escaped")
	}
	// Opening an alert in the page must not itself change state.
	if len(fake.calls) != 0 {
		t.Errorf("opening the pane called %v; marking read must be a separate POST", fake.calls)
	}

	// Once read, the pane no longer fires the request (which would loop with
	// the refresh it causes) and offers Mark unread instead.
	item.Read = true
	body = getAs(t, api, uid, "/alerts?view=unread&selected="+testAlertID).Body.String()
	if strings.Contains(body, `hx-trigger="load"`) {
		t.Error("an already-read alert still fires mark-read")
	}
	if !strings.Contains(body, "Mark unread") {
		t.Error("a read alert does not offer Mark unread")
	}
}

func TestAlertsInbox_ResolveButtonOnlyWhenAllowedAndResolvedNote(t *testing.T) {
	item := sampleAlertItem(testAlertID, "t", true)
	fake := &fakeAlerts{detail: map[string]*alerts.Detail{testAlertID: {Item: item, CanResolve: false}}}
	api, uid := alertsAPI(t, db.TierDeveloper, fake)
	if body := getAs(t, api, uid, "/alerts?selected="+testAlertID).Body.String(); strings.Contains(body, "Resolve for everyone") {
		t.Error("Resolve is offered to someone who may not resolve")
	}

	cleared := db.AlertResolutionCleared
	at := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)
	item.ResolvedAt, item.Resolution = &at, &cleared
	body := getAs(t, api, uid, "/alerts?view=resolved&selected="+testAlertID).Body.String()
	if !strings.Contains(body, "the condition cleared") || !strings.Contains(body, "Resolved") {
		t.Errorf("resolved alert page: %.400s", body)
	}
}

func TestAlertsInbox_UnavailableAlertShowsAMessageNotAnError(t *testing.T) {
	api, uid := alertsAPI(t, db.TierDeveloper, &fakeAlerts{})
	rec := getAs(t, api, uid, "/alerts?selected="+testAlertID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "That alert is no longer available.") {
		t.Errorf("status %d body %.300s", rec.Code, rec.Body.String())
	}
}

func TestAlertsBadge_SidebarAndFragment(t *testing.T) {
	api, uid := alertsAPI(t, db.TierDeveloper, &fakeAlerts{unread: 3})
	page := getAs(t, api, uid, "/dashboard").Body.String()
	for _, want := range []string{`href="/alerts"`, `id="alerts-badge"`, `aria-label="Unread alerts: 3"`, ">3</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("sidebar is missing %q", want)
		}
	}
	if strings.Contains(page, `id="alerts-badge" class="nav-badge" hidden`) {
		t.Error("the badge is hidden although there are unread alerts")
	}

	frag := getAs(t, api, uid, "/alerts/badge")
	if frag.Code != http.StatusOK || !strings.Contains(frag.Body.String(), ">3</span>") || strings.Contains(frag.Body.String(), "<html") {
		t.Errorf("badge fragment: status %d body %q", frag.Code, frag.Body.String())
	}

	// Nothing unread: the span stays in the markup (the live refresh swaps it
	// by id) but is hidden.
	zero, uid0 := alertsAPI(t, db.TierDeveloper, &fakeAlerts{})
	if page := getAs(t, zero, uid0, "/dashboard").Body.String(); !strings.Contains(page, `id="alerts-badge" class="nav-badge" hidden`) {
		t.Error("an empty badge is not hidden")
	}
	if frag := getAs(t, zero, uid0, "/alerts/badge").Body.String(); !strings.Contains(frag, "hidden") || !strings.Contains(frag, `id="alerts-badge"`) {
		t.Errorf("empty badge fragment: %q", frag)
	}
}

func TestAlertsRoutes_RequireASession(t *testing.T) {
	api, _ := alertsAPI(t, db.TierDeveloper, &fakeAlerts{})
	for _, target := range []string{"/alerts", "/alerts/badge"} {
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s without a session returned 200", target)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alerts/"+testAlertID+"/read", nil)
	addValidCSRF(req)
	api.Router().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
		t.Errorf("POST without a session returned %d", rec.Code)
	}
}

func TestAlertsPosts_RequireCSRF(t *testing.T) {
	fake := &fakeAlerts{}
	api, uid := alertsAPI(t, db.TierDeveloper, fake)
	for _, target := range []string{"/alerts/" + testAlertID + "/read", "/alerts/" + testAlertID + "/dismiss", "/alerts/mark-all-read", "/alerts/bulk", "/settings/alerts"} {
		// A session, but no CSRF token. The form content type is what a real
		// htmx or <form> submission sends, and what RequireCSRF checks.
		req := newAuthenticatedRequest(t, http.MethodPost, target, uid)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF = %d, want 403", target, rec.Code)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("a request without CSRF reached the service: %v", fake.calls)
	}
}

func TestAlertActions_RedirectKeepsOrDropsTheOpenAlert(t *testing.T) {
	cases := []struct {
		action     string
		call       string
		wantStatus int
		wantRedir  string
	}{
		// The automatic mark-read answers quietly: the event it causes refreshes the page.
		{"read", "read", http.StatusNoContent, ""},
		// Mark unread closes the pane: an open unread alert marks itself read,
		// which would undo it. Found in manual testing.
		{"unread", "unread", http.StatusNoContent, "/alerts?view=all"},
		{"resolve", "resolve", http.StatusNoContent, "/alerts?view=all&selected=" + testAlertID},
		// A dismissed alert is gone, so its pane is not reopened.
		{"dismiss", "dismiss", http.StatusNoContent, "/alerts?view=all"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			fake := &fakeAlerts{}
			api, uid := alertsAPI(t, db.TierAdmin, fake)
			rec := postFormAs(t, api, uid, "/alerts/"+testAlertID+"/"+c.action, url.Values{"view": {"all"}})
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if got := rec.Header().Get("HX-Redirect"); got != c.wantRedir {
				t.Errorf("HX-Redirect = %q, want %q", got, c.wantRedir)
			}
			if len(fake.calls) != 1 || fake.calls[0] != c.call+":"+testAlertID {
				t.Errorf("service calls = %v", fake.calls)
			}
		})
	}
}

func TestAlertActions_ViewFromTheFormIsValidated(t *testing.T) {
	api, uid := alertsAPI(t, db.TierAdmin, &fakeAlerts{})
	rec := postFormAs(t, api, uid, "/alerts/"+testAlertID+"/dismiss", url.Values{"view": {"all&selected=x\r\nSet-Cookie: a=b"}})
	if got := rec.Header().Get("HX-Redirect"); got != "/alerts?view=unread" {
		t.Errorf("HX-Redirect = %q, want a fixed unread view", got)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("a hostile view value injected a header")
	}
}

func TestAlertActions_ErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", alerts.ErrNotFound, http.StatusNotFound},
		{"not permitted", rbac.ErrNotPermitted, http.StatusForbidden},
		{"internal", errors.New("db down secret-detail"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, uid := alertsAPI(t, db.TierAdmin, &fakeAlerts{actionErr: c.err})
			rec := postFormAs(t, api, uid, "/alerts/"+testAlertID+"/resolve", url.Values{})
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
			if strings.Contains(rec.Body.String(), "secret-detail") {
				t.Error("an internal error leaked to the client")
			}
		})
	}
}

func TestAlertsMarkAllRead(t *testing.T) {
	fake := &fakeAlerts{markAllCount: 4}
	api, uid := alertsAPI(t, db.TierDeveloper, fake)
	rec := postFormAs(t, api, uid, "/alerts/mark-all-read", url.Values{"view": {"unread"}})
	if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/alerts?view=unread" {
		t.Errorf("status %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if len(fake.calls) != 1 || fake.calls[0] != "markall" {
		t.Errorf("service calls = %v", fake.calls)
	}
}

func TestAlertsBulk(t *testing.T) {
	t.Run("read, unread and dismiss act on each chosen alert", func(t *testing.T) {
		for _, action := range []string{"read", "unread", "dismiss"} {
			fake := &fakeAlerts{}
			api, uid := alertsAPI(t, db.TierDeveloper, fake)
			rec := postFormAs(t, api, uid, "/alerts/bulk", url.Values{"action": {action}, "ids": {testAlertID, testAlertID2}, "view": {"all"}})
			if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/alerts?view=all" {
				t.Fatalf("%s: status %d, HX-Redirect %q", action, rec.Code, rec.Header().Get("HX-Redirect"))
			}
			want := []string{action + ":" + testAlertID, action + ":" + testAlertID2}
			if fmt.Sprint(fake.calls) != fmt.Sprint(want) {
				t.Errorf("%s: calls = %v, want %v", action, fake.calls, want)
			}
		}
	})
	t.Run("an alert that vanished is skipped, others still processed", func(t *testing.T) {
		fake := &fakeAlerts{errFor: map[string]error{testAlertID: alerts.ErrNotFound}}
		api, uid := alertsAPI(t, db.TierDeveloper, fake)
		rec := postFormAs(t, api, uid, "/alerts/bulk", url.Values{"action": {"dismiss"}, "ids": {testAlertID, testAlertID2}})
		if rec.Code != http.StatusNoContent || len(fake.calls) != 2 {
			t.Errorf("status %d, calls %v", rec.Code, fake.calls)
		}
	})
	t.Run("a real failure stops the batch", func(t *testing.T) {
		fake := &fakeAlerts{errFor: map[string]error{testAlertID: errors.New("db down")}}
		api, uid := alertsAPI(t, db.TierDeveloper, fake)
		rec := postFormAs(t, api, uid, "/alerts/bulk", url.Values{"action": {"read"}, "ids": {testAlertID, testAlertID2}})
		if rec.Code != http.StatusInternalServerError || len(fake.calls) != 1 {
			t.Errorf("status %d, calls %v", rec.Code, fake.calls)
		}
	})
	t.Run("bad requests", func(t *testing.T) {
		many := make([]string, maxBulkAlerts+1)
		for i := range many {
			many[i] = testAlertID
		}
		for name, form := range map[string]url.Values{
			"no selection":    {"action": {"read"}},
			"too many":        {"action": {"read"}, "ids": many},
			"unknown action":  {"action": {"delete"}, "ids": {testAlertID}},
			"resolve is solo": {"action": {"resolve"}, "ids": {testAlertID}},
			"missing action":  {"ids": {testAlertID}},
		} {
			fake := &fakeAlerts{}
			api, uid := alertsAPI(t, db.TierDeveloper, fake)
			if rec := postFormAs(t, api, uid, "/alerts/bulk", form); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", name, rec.Code)
			}
			if len(fake.calls) != 0 {
				t.Errorf("%s: reached the service: %v", name, fake.calls)
			}
		}
	})
}

func TestSettings_AlertRetentionIsShownAndUpdatableByAnAdmin(t *testing.T) {
	fake := &fakeAlerts{retention: 9}
	api, uid := settingsAPIFor(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/settings").Body.String()
	for _, want := range []string{"<h2>Alerts</h2>", `name="retention_months"`, `value="9"`, `action="/settings/alerts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}

	rec := postFormAs(t, api, uid, "/settings/alerts", url.Values{"retention_months": {"6"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Alert retention updated") {
		t.Errorf("update: status %d body %.200s", rec.Code, rec.Body.String())
	}
	if len(fake.updatedTo) != 1 || fake.updatedTo[0] != 6 {
		t.Errorf("updated to %v, want [6]", fake.updatedTo)
	}
}

func TestSettings_AlertRetentionRejectsBadInputWithAReason(t *testing.T) {
	fake := &fakeAlerts{retentionErr: fmt.Errorf("%w: keep alerts between 1 and 24 months", alerts.ErrInvalidRetention)}
	api, uid := settingsAPIFor(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/settings/alerts", url.Values{"retention_months": {"99"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "between 1 and 24 months") {
		t.Errorf("status %d body %.300s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"", "abc", "1.5"} {
		fake.updatedTo, fake.retentionErr = nil, nil
		rec := postFormAs(t, api, uid, "/settings/alerts", url.Values{"retention_months": {bad}})
		if !strings.Contains(rec.Body.String(), "whole number of months") || len(fake.updatedTo) != 0 {
			t.Errorf("%q: body %.200s updated %v", bad, rec.Body.String(), fake.updatedTo)
		}
	}
}

func TestSettings_AlertRetentionIsForbiddenBelowAdmin(t *testing.T) {
	fake := &fakeAlerts{}
	api, uid := settingsAPIFor(t, db.TierPowerDev, fake)
	rec := postFormAs(t, api, uid, "/settings/alerts", url.Values{"retention_months": {"6"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(fake.updatedTo) != 0 {
		t.Error("a PowerDev changed alert retention")
	}
}

// settingsAPIFor is settingsAPI (container_logs_test.go) with the alerts fake
// installed too.
func settingsAPIFor(t *testing.T, tier db.Tier, fake *fakeAlerts) (*API, string) {
	t.Helper()
	api, uid := settingsAPI(t, tier, &fakeContainerLogs{})
	api.alerts = fake
	return api, uid
}
