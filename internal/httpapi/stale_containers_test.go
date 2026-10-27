// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
	"github.com/1kaius1/Sparky/internal/session"
	"github.com/1kaius1/Sparky/internal/stalecontainers"
)

const (
	cleanupNode = "aaaaaaaa-0000-4000-8000-00000000000a"
	cleanupInst = "10000000-0000-4000-8000-000000000001"
)

// fakeStale implements staleContainerManager with the same tier rule the real
// service applies, so these tests exercise the handlers' handling of
// rbac.ErrNotPermitted and the service's typed errors, not a rule of their own.
type fakeStale struct {
	view        *stalecontainers.SettingsView
	updateErr   error
	updated     []stalecontainers.SettingsInput
	preview     *stalecontainers.PreviewResult
	previewErr  error
	removeRes   stalecontainers.RemoveResult
	removeErr   error
	removeCalls []string
}

func (f *fakeStale) Settings(_ context.Context, actor rbac.Actor) (*stalecontainers.SettingsView, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if f.view != nil {
		return f.view, nil
	}
	return &stalecontainers.SettingsView{
		Settings: &db.StaleContainerSettings{Mode: db.StaleContainerModeNone, MinAgeHours: 24, ArchiveLines: 2000, UpdatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
		Timezone: "UTC",
	}, nil
}

func (f *fakeStale) UpdateSettings(_ context.Context, actor rbac.Actor, in stalecontainers.SettingsInput) error {
	if !rbac.CanManageStaleContainers(actor) {
		return rbac.ErrNotPermitted
	}
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, in)
	return nil
}

func (f *fakeStale) Preview(_ context.Context, actor rbac.Actor) (*stalecontainers.PreviewResult, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if f.previewErr != nil {
		return nil, f.previewErr
	}
	if f.preview != nil {
		return f.preview, nil
	}
	return &stalecontainers.PreviewResult{MinAge: 24 * time.Hour, ArchiveLines: 2000}, nil
}

func (f *fakeStale) RemoveOne(_ context.Context, actor rbac.Actor, nodeID, instanceID string, lines int) (stalecontainers.RemoveResult, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return stalecontainers.RemoveResult{}, rbac.ErrNotPermitted
	}
	f.removeCalls = append(f.removeCalls, fmt.Sprintf("%s/%s/%d", nodeID, instanceID, lines))
	return f.removeRes, f.removeErr
}

func staleAPI(t *testing.T, tier db.Tier, fake *fakeStale) (*API, string) {
	t.Helper()
	api, uid := settingsAPI(t, tier, &fakeContainerLogs{})
	api.staleContainers = fake
	return api, uid
}

func staleSettingsForm(extra url.Values) url.Values {
	v := url.Values{
		"stale_mode": {"report"}, "schedule_kind": {"weekly"}, "weekdays": {"1", "3"}, "times": {"02:00, 14:30"},
		"every_days": {"2"}, "min_age_hours": {"48"}, "archive_lines": {"500"},
	}
	for k, vals := range extra {
		v[k] = vals
	}
	return v
}

func TestSettings_StaleMonitoringIsShown(t *testing.T) {
	next := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	fake := &fakeStale{view: &stalecontainers.SettingsView{
		Settings: &db.StaleContainerSettings{Mode: db.StaleContainerModeReport, MinAgeHours: 36, ArchiveLines: 750, UpdatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
		Spec:     &schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00", "14:30"}},
		Timezone: "UTC", NextRun: &next,
		LastRun: &stalecontainers.RunSummary{Mode: "report", StartedAt: time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 26, 2, 0, 9, 0, time.UTC), NodesScanned: 2, NodesSkipped: 1, Stale: 4, Recent: 1, Orphans: 1},
	}}
	api, uid := staleAPI(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/settings").Body.String()
	for _, want := range []string{
		"Stale container monitoring", `action="/settings/stale-containers"`,
		`name="stale_mode" value="report" checked`, `name="stale_mode" value="none">`,
		"Mondays and Wednesdays at 02:00 and 14:30", "2026-10-28 02:00 UTC",
		`name="schedule_kind" value="weekly" checked`, `name="weekdays" value="1" checked`, `name="weekdays" value="3" checked`, `name="weekdays" value="2">`,
		`value="02:00, 14:30"`, `name="min_age_hours"`, `value="36"`, `value="750"`,
		"2 nodes scanned", "1 not reachable", "4 stale", "1 too recent", "1 running with no record",
		`name="confirm_prune"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}
	if strings.Contains(body, "removed,") {
		t.Error("a Report run shows removal counts")
	}
}

func TestSettings_StaleMonitoringFormIsParsedForTheService(t *testing.T) {
	cases := map[string]struct {
		form url.Values
		want stalecontainers.SettingsInput
	}{
		"weekly report": {staleSettingsForm(nil), stalecontainers.SettingsInput{
			Mode: db.StaleContainerModeReport, HasSchedule: true, MinAgeHours: 48, ArchiveLines: 500,
			Schedule: schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00", "14:30"}},
		}},
		"every N days prune, confirmed, keeping all": {staleSettingsForm(url.Values{
			"stale_mode": {"prune"}, "schedule_kind": {"every_days"}, "confirm_prune": {"yes"}, "archive_all": {"yes"},
		}), stalecontainers.SettingsInput{
			Mode: db.StaleContainerModePrune, HasSchedule: true, ConfirmPrune: true, MinAgeHours: 48, ArchiveLines: 0,
			Schedule: schedule.Spec{Kind: schedule.KindEveryDays, EveryDays: 2},
		}},
		"times separated by spaces and semicolons": {staleSettingsForm(url.Values{"times": {" 01:00;  03:30  05:00 "}}), stalecontainers.SettingsInput{
			Mode: db.StaleContainerModeReport, HasSchedule: true, MinAgeHours: 48, ArchiveLines: 500,
			Schedule: schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{1, 3}, Times: []string{"01:00", "03:30", "05:00"}},
		}},
		"switched off with nothing in the schedule fields keeps the saved one": {url.Values{
			"stale_mode": {"none"}, "schedule_kind": {"weekly"}, "min_age_hours": {"24"}, "archive_lines": {"2000"},
		}, stalecontainers.SettingsInput{
			Mode: db.StaleContainerModeNone, HasSchedule: false, MinAgeHours: 24, ArchiveLines: 2000,
			Schedule: schedule.Spec{Kind: schedule.KindWeekly},
		}},
		"switched off but a schedule was filled in": {staleSettingsForm(url.Values{"stale_mode": {"none"}}), stalecontainers.SettingsInput{
			Mode: db.StaleContainerModeNone, HasSchedule: true, MinAgeHours: 48, ArchiveLines: 500,
			Schedule: schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00", "14:30"}},
		}},
	}
	for name, c := range cases {
		fake := &fakeStale{}
		api, uid := staleAPI(t, db.TierAdmin, fake)
		rec := postFormAs(t, api, uid, "/settings/stale-containers", c.form)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Stale container monitoring updated") {
			t.Errorf("%s: status %d body %.200s", name, rec.Code, rec.Body.String())
			continue
		}
		if len(fake.updated) != 1 {
			t.Errorf("%s: %d updates", name, len(fake.updated))
			continue
		}
		got := fake.updated[0]
		if got.Mode != c.want.Mode || got.HasSchedule != c.want.HasSchedule || got.ConfirmPrune != c.want.ConfirmPrune ||
			got.MinAgeHours != c.want.MinAgeHours || got.ArchiveLines != c.want.ArchiveLines ||
			got.Schedule.Kind != c.want.Schedule.Kind || got.Schedule.EveryDays != c.want.Schedule.EveryDays ||
			fmt.Sprint(got.Schedule.Weekdays) != fmt.Sprint(c.want.Schedule.Weekdays) || fmt.Sprint(got.Schedule.Times) != fmt.Sprint(c.want.Schedule.Times) {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got, c.want)
		}
	}
}

func TestSettings_StaleMonitoringRefusalsAreShownNotHidden(t *testing.T) {
	cases := map[string]struct {
		form url.Values
		err  error
		want string
	}{
		"a minimum age that is not a number":     {staleSettingsForm(url.Values{"min_age_hours": {"soon"}}), nil, "minimum age must be a whole number"},
		"log lines that are not a number":        {staleSettingsForm(url.Values{"archive_lines": {"lots"}}), nil, "log lines to keep must be a whole number"},
		"a weekday that is not a number":         {staleSettingsForm(url.Values{"weekdays": {"monday"}}), nil, "weekday was not understood"},
		"N that is not a number":                 {staleSettingsForm(url.Values{"schedule_kind": {"every_days"}, "every_days": {"two"}}), nil, "N as a whole number"},
		"the service refusing the settings":      {staleSettingsForm(nil), fmt.Errorf("%w: choose at least one weekday", stalecontainers.ErrInvalidSettings), "choose at least one weekday"},
		"Prune switched on without confirmation": {staleSettingsForm(url.Values{"stale_mode": {"prune"}}), stalecontainers.ErrPruneNotConfirmed, "confirm that Prune removes containers"},
	}
	for name, c := range cases {
		fake := &fakeStale{updateErr: c.err}
		api, uid := staleAPI(t, db.TierAdmin, fake)
		rec := postFormAs(t, api, uid, "/settings/stale-containers", c.form)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) || strings.Contains(rec.Body.String(), "Stale container monitoring updated") {
			t.Errorf("%s: status %d, body lacks %q", name, rec.Code, c.want)
		}
		if c.err == nil && len(fake.updated) != 0 {
			t.Errorf("%s: an unparseable form still reached the service", name)
		}
	}

	// An internal failure is a generic message; the cause stays in the log.
	fake := &fakeStale{updateErr: errors.New("update stale container settings: pq: connection refused at 10.0.0.5")}
	api, uid := staleAPI(t, db.TierAdmin, fake)
	body := postFormAs(t, api, uid, "/settings/stale-containers", staleSettingsForm(nil)).Body.String()
	if !strings.Contains(body, "failed to update the settings") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal failure body leaks or lacks the generic message")
	}
}

func TestSettings_StaleMonitoringIsForbiddenBelowAdminAndNeedsCSRF(t *testing.T) {
	fake := &fakeStale{}
	api, uid := staleAPI(t, db.TierPowerDev, fake)
	if rec := postFormAs(t, api, uid, "/settings/stale-containers", staleSettingsForm(nil)); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(fake.updated) != 0 {
		t.Error("a non-admin changed the monitoring")
	}

	fake = &fakeStale{}
	api, uid = staleAPI(t, db.TierAdmin, fake)
	cookieValue, err := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/stale-containers", strings.NewReader(staleSettingsForm(nil).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(fake.updated) != 0 {
		t.Errorf("without a CSRF token: status %d, %d updates; want refused with no change", rec.Code, len(fake.updated))
	}
}

// ---- the Container cleanup page ----

func TestContainerCleanup_PageIsAdminOnlyAndLoadsTheScanAfterwards(t *testing.T) {
	api, uid := staleAPI(t, db.TierAdmin, &fakeStale{})
	rec := getAs(t, api, uid, "/containers/cleanup")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Container cleanup") || !strings.Contains(body, `hx-get="/containers/cleanup/scan"`) ||
		!strings.Contains(body, `hx-trigger="load"`) || !strings.Contains(body, "24 hours") {
		t.Errorf("status %d, body lacks the shell: %.300s", rec.Code, body)
	}
	// The scan is not run to render the page itself.
	if strings.Contains(body, "Asking the nodes") == false {
		t.Error("the page does not say it is waiting for the scan")
	}

	for _, tier := range []db.Tier{db.TierPowerDev, db.TierDeveloper, db.TierReadOnly} {
		api, uid := staleAPI(t, tier, &fakeStale{})
		for _, path := range []string{"/containers/cleanup", "/containers/cleanup/scan"} {
			if rec := getAs(t, api, uid, path); rec.Code != http.StatusForbidden {
				t.Errorf("tier %s GET %s = %d, want 403", tier, path, rec.Code)
			}
		}
		if rec := postFormAs(t, api, uid, "/containers/cleanup/remove", url.Values{"node_id": {cleanupNode}, "instance_id": {cleanupInst}, "lines": {"10"}}); rec.Code != http.StatusForbidden {
			t.Errorf("tier %s POST remove = %d, want 403", tier, rec.Code)
		}
	}
}

func TestContainerCleanup_SidebarEntryIsAdminOnly(t *testing.T) {
	for tier, want := range map[db.Tier]bool{db.TierAdmin: true, db.TierPowerDev: false, db.TierDeveloper: false} {
		api, uid := staleAPI(t, tier, &fakeStale{})
		body := getAs(t, api, uid, "/dashboard").Body.String()
		if got := strings.Contains(body, `href="/containers/cleanup"`); got != want {
			t.Errorf("tier %s: sidebar link shown = %v, want %v", tier, got, want)
		}
	}
}

func scanResult() *stalecontainers.PreviewResult {
	code := 137
	stopped := time.Now().Add(-5 * 24 * time.Hour)
	recent := time.Now().Add(-3 * time.Hour)
	mk := func(id, name, state string, v stalecontainers.Verdict, at time.Time) stalecontainers.Item {
		return stalecontainers.Item{
			Container:   stalecontainers.Container{InstanceID: id, ContainerName: name, State: state, ExitCode: &code, OOMKilled: id == cleanupInst},
			Evaluation:  stalecontainers.Evaluation{Verdict: v, StoppedAt: at, Reason: "because"},
			ProfileName: "qwen-8b",
		}
	}
	return &stalecontainers.PreviewResult{MinAge: 24 * time.Hour, ArchiveLines: 750, Nodes: []stalecontainers.NodeScan{
		{NodeID: cleanupNode, NodeName: "spark-1", Status: stalecontainers.ScanDone, Truncated: true, Items: []stalecontainers.Item{
			mk(cleanupInst, "<script>alert(1)</script>", "exited", stalecontainers.VerdictStale, stopped),
			mk("10000000-0000-4000-8000-000000000002", "sparky-recent", "exited", stalecontainers.VerdictRecent, recent),
			mk("10000000-0000-4000-8000-000000000003", "sparky-live", "running", stalecontainers.VerdictLive, time.Time{}),
			mk("10000000-0000-4000-8000-000000000004", "sparky-orphan", "running", stalecontainers.VerdictOrphanLive, time.Time{}),
			mk("10000000-0000-4000-8000-000000000005", "sparky-odd", "removing", stalecontainers.VerdictOther, time.Time{}),
		}},
		{NodeID: "aaaaaaaa-0000-4000-8000-00000000000b", NodeName: "spark-2", Status: stalecontainers.ScanOffline, Error: "the node's agent is not connected"},
		{NodeID: "aaaaaaaa-0000-4000-8000-00000000000c", NodeName: "spark-3", Status: stalecontainers.ScanFailed, Error: "the agent did not answer within 30s"},
	}}
}

func TestContainerCleanup_ScanFragmentShowsEachGroupAndEscapesWhatNodesSend(t *testing.T) {
	api, uid := staleAPI(t, db.TierAdmin, &fakeStale{preview: scanResult()})
	rec := getAs(t, api, uid, "/containers/cleanup/scan")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{
		"spark-1", "status-online", "spark-2", "status-offline", "spark-3", "status-failed",
		"the node&#39;s agent is not connected", "did not answer within 30s",
		"Stale (1)", "exit code 137, out of memory", "qwen-8b", "(5 days ago)",
		`hx-post="/containers/cleanup/remove"`, `name="node_id" value="` + cleanupNode + `"`, `name="instance_id" value="` + cleanupInst + `"`,
		`<option value="750" selected>750 lines</option>`, `<option value="0">All</option>`, `<option value="2000">2000 lines</option>`,
		"Stopped too recently to be stale (1)", "sparky-recent",
		"Running, but Sparky has no record of them (1)", "sparky-orphan",
		"1 in use or belonging to a running instance", "1 in a state this page does not act on",
		"holds more containers than it can list at once",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scan fragment is missing %q", want)
		}
	}
	// A node sends the container name; it must never reach the page unescaped.
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("a container name was not escaped")
	}
	// Only the stale container gets a remove control.
	if n := strings.Count(body, `hx-post="/containers/cleanup/remove"`); n != 1 {
		t.Errorf("%d remove forms, want exactly one (for the one stale container)", n)
	}
}

func TestContainerCleanup_ScanFragmentWhenNothingIsStaleOrTheScanFails(t *testing.T) {
	api, uid := staleAPI(t, db.TierAdmin, &fakeStale{preview: &stalecontainers.PreviewResult{MinAge: time.Hour, ArchiveLines: 2000, Nodes: []stalecontainers.NodeScan{
		{NodeID: cleanupNode, NodeName: "spark-1", Status: stalecontainers.ScanDone},
	}}})
	if body := getAs(t, api, uid, "/containers/cleanup/scan").Body.String(); !strings.Contains(body, "Nothing is stale on this node") || strings.Contains(body, "hx-post") {
		t.Errorf("empty node: %.300s", body)
	}
	api, uid = staleAPI(t, db.TierAdmin, &fakeStale{preview: &stalecontainers.PreviewResult{}})
	if body := getAs(t, api, uid, "/containers/cleanup/scan").Body.String(); !strings.Contains(body, "No node runs containers") {
		t.Errorf("no nodes: %.300s", body)
	}
	api, uid = staleAPI(t, db.TierAdmin, &fakeStale{previewErr: errors.New("list nodes: pq: boom at 10.0.0.5")})
	body := getAs(t, api, uid, "/containers/cleanup/scan").Body.String()
	if !strings.Contains(body, "The scan failed") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("failed scan: %.300s", body)
	}
}

func TestContainerCleanup_RemoveSendsTheChoiceAndShowsEveryOutcomeInPlace(t *testing.T) {
	form := url.Values{"node_id": {cleanupNode}, "instance_id": {cleanupInst}, "lines": {"500"}}
	cases := map[string]struct {
		res  stalecontainers.RemoveResult
		err  error
		want string
		bad  string
	}{
		"removed":     {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeRemoved, ContainerName: "sparky-x", ProfileName: "qwen-8b"}, nil, "Removed. Its log was saved first", "form-error"},
		"gone":        {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeGone}, nil, "Already gone", "form-error"},
		"kept":        {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeKept, Detail: "the log could not be archived"}, nil, "Kept: the log could not be saved", "form-success"},
		"refused":     {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeRefused, Detail: "the container is running"}, nil, "the node refused. the container is running", "form-success"},
		"failed":      {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeFailed, Detail: "the agent did not answer"}, nil, "Not removed: the agent did not answer", "form-success"},
		"not stale":   {stalecontainers.RemoveResult{}, fmt.Errorf("%w: its instance is running", stalecontainers.ErrNotStale), "Not removed. its instance is running", "form-success"},
		"unavailable": {stalecontainers.RemoveResult{}, fmt.Errorf("%w: spark-1: the node's agent is not connected", stalecontainers.ErrNodeUnavailable), "agent is not connected", "form-success"},
		"audit":       {stalecontainers.RemoveResult{Outcome: agentproto.RemoveOutcomeRemoved}, fmt.Errorf("%w: x", stalecontainers.ErrAuditAfterRemoval), "writing the audit log entry failed", "form-success"},
		"other error": {stalecontainers.RemoveResult{}, errors.New("get stale container settings: pq: boom at 10.0.0.5"), "something went wrong", "10.0.0.5"},
	}
	for name, c := range cases {
		fake := &fakeStale{removeRes: c.res, removeErr: c.err}
		api, uid := staleAPI(t, db.TierAdmin, fake)
		rec := postFormAs(t, api, uid, "/containers/cleanup/remove", form)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, c.want) || strings.Contains(body, c.bad) {
			t.Errorf("%s: status %d, body %.300s; want %q and not %q", name, rec.Code, body, c.want, c.bad)
		}
		if !strings.Contains(body, `<tr id="stale-`+cleanupInst+`">`) {
			t.Errorf("%s: the replacement row does not keep the row's id", name)
		}
		if len(fake.removeCalls) != 1 || fake.removeCalls[0] != cleanupNode+"/"+cleanupInst+"/500" {
			t.Errorf("%s: RemoveOne called with %v", name, fake.removeCalls)
		}
	}
}

func TestContainerCleanup_RemoveRefusesMalformedRequests(t *testing.T) {
	fake := &fakeStale{removeErr: fmt.Errorf("%w: malformed id", stalecontainers.ErrInvalidRequest)}
	api, uid := staleAPI(t, db.TierAdmin, fake)
	if rec := postFormAs(t, api, uid, "/containers/cleanup/remove", url.Values{"node_id": {"x"}, "instance_id": {"y"}, "lines": {"10"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed ids: status %d, want 400", rec.Code)
	}
	fake.removeCalls = nil
	for _, lines := range []string{"", "abc", "1.5"} {
		rec := postFormAs(t, api, uid, "/containers/cleanup/remove", url.Values{"node_id": {cleanupNode}, "instance_id": {cleanupInst}, "lines": {lines}})
		if rec.Code != http.StatusBadRequest || len(fake.removeCalls) != 0 {
			t.Errorf("lines %q: status %d, %d calls; want 400 and no call", lines, rec.Code, len(fake.removeCalls))
		}
	}
}

func TestContainerCleanup_RemoveNeedsACSRFToken(t *testing.T) {
	fake := &fakeStale{}
	api, uid := staleAPI(t, db.TierAdmin, fake)
	cookieValue, _ := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	form := url.Values{"node_id": {cleanupNode}, "instance_id": {cleanupInst}, "lines": {"10"}}
	req := httptest.NewRequest(http.MethodPost, "/containers/cleanup/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(fake.removeCalls) != 0 {
		t.Errorf("status %d, %d calls; want a refused request that removed nothing", rec.Code, len(fake.removeCalls))
	}
}

func TestAlertObjectLink_StaleContainersGoToTheCleanupPage(t *testing.T) {
	typ := "stale_containers"
	if url, label := alertObjectLink(&typ); url != "/containers/cleanup" || label == "" {
		t.Errorf("stale_containers link = %q %q", url, label)
	}
	inst := "running_instance"
	if url, _ := alertObjectLink(&inst); url != "/dashboard" {
		t.Errorf("running_instance link = %q", url)
	}
	unknown := "mystery"
	if url, label := alertObjectLink(&unknown); url != "" || label != "" {
		t.Errorf("unknown type linked to %q %q", url, label)
	}
	if url, _ := alertObjectLink(nil); url != "" {
		t.Error("a nil object type linked somewhere")
	}
}

func TestContainerLogReasonLabel_KnowsStaleCleanup(t *testing.T) {
	for reason, want := range map[string]string{
		"stale_cleanup": "removed as stale", "unload": "unloaded", "failed_launch": "failed launch",
		"replaced": "replaced by a new launch", "something_new": "something new",
	} {
		if got := containerLogReasonLabel(reason); got != want {
			t.Errorf("containerLogReasonLabel(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestCleanupLineOptions(t *testing.T) {
	opts := cleanupLineOptions(2000)
	if len(opts) != 6 || !opts[3].Selected || opts[3].Value != 2000 || opts[5].Value != 0 || opts[5].Label != "All" {
		t.Errorf("default 2000: %+v", opts)
	}
	opts = cleanupLineOptions(750)
	if len(opts) != 7 || opts[0].Value != 750 || !opts[0].Selected {
		t.Errorf("an unusual default is added and selected: %+v", opts)
	}
	opts = cleanupLineOptions(0)
	if !opts[5].Selected || opts[5].Value != 0 {
		t.Errorf("all: %+v", opts)
	}
}
