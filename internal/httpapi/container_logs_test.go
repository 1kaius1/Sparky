// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/containerlogs"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const testLogID = "cccccccc-cccc-cccc-cccc-cccccccccccc"

// fakeContainerLogs implements containerLogViewer with the same tier rule
// the real service applies, so these tests exercise the handlers' handling
// of ErrNotPermitted rather than a rule of their own.
type fakeContainerLogs struct {
	archives []*db.ContainerLogArchive
	text     string
	readErr  error
	listErr  error

	// latest answers LatestForInstances (instance id -> archive id);
	// latestAsked records the ids it was asked about.
	latest      map[string]string
	latestAsked []string

	// live answers Live; liveErr, if set, is returned instead. liveAsked
	// records the arguments.
	live      *containerlogs.LiveLog
	liveErr   error
	liveAsked []string

	// retention is the container log retention setting; retentionErr and
	// updatedTo support the Settings page tests.
	retention    int
	retentionErr error
	updatedTo    []int
}

func (f *fakeContainerLogs) Live(_ context.Context, actor rbac.Actor, instanceID string, lines int) (*containerlogs.LiveLog, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	f.liveAsked = append(f.liveAsked, fmt.Sprintf("%s/%d", instanceID, lines))
	return f.live, f.liveErr
}

func (f *fakeContainerLogs) Settings(_ context.Context, actor rbac.Actor) (*db.ContainerLogSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	m := f.retention
	if m == 0 {
		m = 12
	}
	return &db.ContainerLogSettings{RetentionMonths: m, UpdatedAt: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeContainerLogs) UpdateRetention(_ context.Context, actor rbac.Actor, months int) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if f.retentionErr != nil {
		return f.retentionErr
	}
	f.updatedTo = append(f.updatedTo, months)
	return nil
}

func (f *fakeContainerLogs) LatestForInstances(_ context.Context, actor rbac.Actor, ids []string) (map[string]string, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	f.latestAsked = append(f.latestAsked, ids...)
	return f.latest, nil
}

func (f *fakeContainerLogs) List(_ context.Context, actor rbac.Actor, _ int) ([]*db.ContainerLogArchive, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	return f.archives, f.listErr
}

func (f *fakeContainerLogs) Read(_ context.Context, actor rbac.Actor, id string) (*db.ContainerLogArchive, string, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, "", rbac.ErrNotPermitted
	}
	if f.readErr != nil {
		return nil, "", f.readErr
	}
	for _, a := range f.archives {
		if a.ID == id {
			return a, f.text, nil
		}
	}
	return nil, "", containerlogs.ErrNotFound
}

func sampleArchive() *db.ContainerLogArchive {
	code := 137
	lines := 2000
	started := time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC)
	return &db.ContainerLogArchive{
		ID: testLogID, NodeName: "spark-1", ProfileName: "tiny", ContainerName: "sparky-tiny-20261009-010203",
		Reason: "unload", State: "exited", ExitCode: &code, OOMKilled: true, StartedAt: &started, LinesRequested: &lines,
		LinesKept: 3, SizeBytes: 2048, CreatedAt: time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC),
	}
}

func logsAPI(t *testing.T, tier db.Tier, fake *fakeContainerLogs) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{})
	api.containerLogs = fake
	return api, "u-1"
}

func getAs(t *testing.T, api *API, userID, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, path, userID))
	return rec
}

func TestContainerLogsList_ShowsArchivesToADeveloper(t *testing.T) {
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{sampleArchive()}})
	rec := getAs(t, api, uid, "/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"spark-1", "tiny", "sparky-tiny-20261009-010203", "unloaded", "out of memory", `href="/logs/` + testLogID + `"`, "2.0 KiB", "Container logs"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page is missing %q", want)
		}
	}
}

func TestContainerLogsList_EmptyStateAndSidebarLink(t *testing.T) {
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{})
	body := getAs(t, api, uid, "/logs").Body.String()
	if !strings.Contains(body, "No container logs saved yet") {
		t.Error("empty state missing")
	}
	if !strings.Contains(body, `href="/logs"`) {
		t.Error("a Developer should see the Container logs sidebar link")
	}
}

func TestContainerLogs_ReadOnlyIsForbiddenEverywhere(t *testing.T) {
	api, uid := logsAPI(t, db.TierReadOnly, &fakeContainerLogs{archives: []*db.ContainerLogArchive{sampleArchive()}, text: "secret prompt text\n"})
	for _, path := range []string{"/logs", "/logs/" + testLogID, "/logs/" + testLogID + "/download"} {
		rec := getAs(t, api, uid, path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s status = %d, want 403", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret prompt text") {
			t.Errorf("GET %s leaked the log to a read-only user", path)
		}
	}
	// And a read-only user is not shown the sidebar link on any page.
	if body := getAs(t, api, uid, "/dashboard").Body.String(); strings.Contains(body, `href="/logs"`) {
		t.Error("the Container logs link is shown to a read-only user")
	}
}

func TestContainerLogView_ShowsDetailsAndEscapesTheText(t *testing.T) {
	text := "loading <script>alert(1)</script>\n\x1b[31mred error\x1b[0m & more\n"
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{sampleArchive()}, text: text})
	rec := getAs(t, api, uid, "/logs/"+testLogID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("log text was not HTML-escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped log text missing")
	}
	if strings.Contains(body, "\x1b") || strings.Contains(body, "[31m") {
		t.Error("ANSI escape sequences were not stripped from the view")
	}
	for _, want := range []string{"red error &amp; more", "killed for running out of memory", "exit code 137", "2000", "spark-1", `/logs/` + testLogID + `/download`} {
		if !strings.Contains(body, want) {
			t.Errorf("view page is missing %q", want)
		}
	}
}

func TestContainerLogView_LongLogShowsOnlyTheTailAndSaysSo(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < containerLogViewLines+50; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{sampleArchive()}, text: sb.String()})
	body := getAs(t, api, uid, "/logs/"+testLogID).Body.String()
	if strings.Contains(body, "line-0\n") || !strings.Contains(body, fmt.Sprintf("line-%d\n", containerLogViewLines+49)) {
		t.Error("view should keep the newest lines and drop the oldest")
	}
	if !strings.Contains(body, fmt.Sprintf("Showing the last %d lines", containerLogViewLines)) {
		t.Error("the page does not say it is showing only the tail")
	}
}

func TestContainerLogView_NotFound(t *testing.T) {
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{})
	if rec := getAs(t, api, uid, "/logs/"+testLogID); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec := getAs(t, api, uid, "/logs/"+testLogID+"/download"); rec.Code != http.StatusNotFound {
		t.Errorf("download status = %d, want 404", rec.Code)
	}
}

func TestContainerLogDownload_IsPlainTextAttachmentWithASafeFileName(t *testing.T) {
	arc := sampleArchive()
	arc.ContainerName = `evil"; filename="x.exe`
	text := "<b>raw</b>\n\x1b[31mkept raw in the download\x1b[0m\n"
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{arc}, text: text})
	rec := getAs(t, api, uid, "/logs/"+testLogID+"/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || strings.Contains(cd, "evil") || strings.Contains(cd, "exe") || !strings.Contains(cd, "cccccccc") {
		t.Errorf("Content-Disposition = %q, want an attachment named from the archive id only", cd)
	}
	if rec.Body.String() != text {
		t.Errorf("download body = %q, want the exact text", rec.Body.String())
	}
}

func TestContainerLogs_ServiceFailureIs500WithoutDetails(t *testing.T) {
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{listErr: fmt.Errorf("pq: password authentication failed")})
	rec := getAs(t, api, uid, "/logs")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Errorf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestContainerLogs_RequireASession(t *testing.T) {
	api, _ := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{sampleArchive()}})
	for _, path := range []string{"/logs", "/logs/" + testLogID, "/logs/" + testLogID + "/download"} {
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s without a session returned 200", path)
		}
	}
}

func TestTailLines(t *testing.T) {
	cases := []struct {
		in      string
		n       int
		want    string
		wantCut bool
	}{
		{"a\nb\nc\n", 2, "b\nc\n", true},
		{"a\nb\nc\n", 3, "a\nb\nc\n", false},
		{"a\nb\nc", 2, "b\nc", true},
		{"a\n", 5, "a\n", false},
		{"", 5, "", false},
	}
	for _, tc := range cases {
		got, cut := tailLines(tc.in, tc.n)
		if got != tc.want || cut != tc.wantCut {
			t.Errorf("tailLines(%q, %d) = %q, %v; want %q, %v", tc.in, tc.n, got, cut, tc.want, tc.wantCut)
		}
	}
}

func TestFormatLogSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 2048: "2.0 KiB", 1 << 20: "1.0 MiB", 5 << 20: "5.0 MiB"} {
		if got := formatLogSize(in); got != want {
			t.Errorf("formatLogSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerLogs_ExitCodeMinusOneIsShownAsKilledBySignal(t *testing.T) {
	arc := sampleArchive()
	code := -1
	arc.ExitCode, arc.OOMKilled = &code, false
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{archives: []*db.ContainerLogArchive{arc}, text: "x\n"})
	for _, path := range []string{"/logs", "/logs/" + testLogID} {
		body := getAs(t, api, uid, path).Body.String()
		if !strings.Contains(body, "killed by a signal") || strings.Contains(body, "exit code -1") {
			t.Errorf("%s: exit -1 not shown as killed by a signal", path)
		}
	}
}

func postFormAs(t *testing.T, api *API, userID, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedFormRequest(t, path, userID, form))
	return rec
}

func TestInstanceLogs_ShowsTheLiveTextEscapedAndWithLineChoices(t *testing.T) {
	fake := &fakeContainerLogs{live: &containerlogs.LiveLog{
		InstanceID: testLogID, ProfileName: "tiny-q4",
		Meta: agentproto.ContainerLogMeta{State: "running"},
		Text: "up and <b>serving</b>\n\x1b[31mred\x1b[0m line\n",
	}}
	api, uid := logsAPI(t, db.TierDeveloper, fake)
	rec := getAs(t, api, uid, "/instances/"+testLogID+"/logs?lines=1000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"tiny-q4", "&lt;b&gt;serving&lt;/b&gt;", "red line", "State: running", "Showing the last 2 lines", "<strong>1000</strong>", `lines=500`, "Refresh"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(body, "<b>serving</b>") || strings.Contains(body, "\x1b") {
		t.Error("log text was not escaped or ANSI was not stripped")
	}
	if len(fake.liveAsked) != 1 || fake.liveAsked[0] != testLogID+"/1000" {
		t.Errorf("Live asked %v, want the instance and 1000 lines", fake.liveAsked)
	}
}

func TestInstanceLogs_LinesParameterIsBoundedAndDefaulted(t *testing.T) {
	for query, want := range map[string]string{"": "/500", "?lines=abc": "/500", "?lines=-4": "/500", "?lines=99999": "/5000", "?lines=100": "/100"} {
		fake := &fakeContainerLogs{live: &containerlogs.LiveLog{Text: "x\n"}}
		api, uid := logsAPI(t, db.TierDeveloper, fake)
		getAs(t, api, uid, "/instances/"+testLogID+"/logs"+query)
		if len(fake.liveAsked) != 1 || !strings.HasSuffix(fake.liveAsked[0], want) {
			t.Errorf("query %q: Live asked %v, want it to end %s", query, fake.liveAsked, want)
		}
	}
}

func TestInstanceLogs_FailuresShowAReasonAndPointToTheSavedLog(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		text   string
	}{
		"node offline": {containerlogs.ErrNodeOffline, http.StatusOK, "not connected"},
		"silent node":  {containerlogs.ErrAgentSilent, http.StatusOK, "did not answer"},
		"agent reason": {&containerlogs.AgentError{Message: "no container on the node any more"}, http.StatusOK, "no container on the node any more"},
		"busy":         {containerlogs.ErrBusy, http.StatusTooManyRequests, "too many log requests"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeContainerLogs{liveErr: tc.err, latest: map[string]string{testLogID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}}
			api, uid := logsAPI(t, db.TierDeveloper, fake)
			rec := getAs(t, api, uid, "/instances/"+testLogID+"/logs")
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.text) || !strings.Contains(body, "form-error") {
				t.Errorf("no reason shown (%q): %s", tc.text, body)
			}
			if !strings.Contains(body, `href="/logs/dddddddd-dddd-dddd-dddd-dddddddddddd"`) {
				t.Error("no link to the saved log")
			}
			if strings.Contains(body, `class="log-text"`) {
				t.Error("an empty log box is shown alongside the error")
			}
		})
	}
}

func TestInstanceLogs_NotFoundForbiddenAndUnexpectedErrors(t *testing.T) {
	api, uid := logsAPI(t, db.TierDeveloper, &fakeContainerLogs{liveErr: containerlogs.ErrNotFound})
	if rec := getAs(t, api, uid, "/instances/"+testLogID+"/logs"); rec.Code != http.StatusNotFound {
		t.Errorf("not found: status %d", rec.Code)
	}
	api, uid = logsAPI(t, db.TierReadOnly, &fakeContainerLogs{live: &containerlogs.LiveLog{Text: "secret\n"}})
	rec := getAs(t, api, uid, "/instances/"+testLogID+"/logs")
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("read-only: status %d body %q", rec.Code, rec.Body.String())
	}
	api, uid = logsAPI(t, db.TierDeveloper, &fakeContainerLogs{liveErr: fmt.Errorf("pq: password authentication failed")})
	rec = getAs(t, api, uid, "/instances/"+testLogID+"/logs")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Errorf("unexpected error: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestLogsLinks_ShownOnProfilesAndDashboardForDevelopersOnly(t *testing.T) {
	profiles := &fakeProfileLister{profiles: []*db.Profile{{ID: "profile-1", Name: "live-profile", ModelRef: "org/m"}}}
	instances := &fakeInstanceLister{instances: []*db.RunningInstance{{ID: "inst-live", ProfileID: "profile-1", Status: db.RunningInstanceStatusRunning, PrimaryNodeID: "n1"}}}
	for tier, want := range map[db.Tier]bool{db.TierDeveloper: true, db.TierReadOnly: false} {
		users := newFakeUserLister()
		users.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
		api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, profiles, instances, &fakeTransferLister{}, users, &fakeAuditLister{})
		for _, page := range []string{"/profiles", "/dashboard"} {
			body := getAs(t, api, "u-1", page).Body.String()
			if got := strings.Contains(body, `href="/instances/inst-live/logs"`); got != want {
				t.Errorf("tier %s on %s: Logs link shown = %v, want %v", tier, page, got, want)
			}
		}
	}
}

// settingsAPI is an API whose Settings page has real rows behind it, with fake
// as its container log service.
func settingsAPI(t *testing.T, tier db.Tier, fake *fakeContainerLogs) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)
	api.containerLogs = fake
	return api, "u-1"
}

func TestSettings_ContainerLogRetentionIsShownAndUpdatableByAnAdmin(t *testing.T) {
	fake := &fakeContainerLogs{retention: 9}
	api, uid := settingsAPI(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/settings").Body.String()
	for _, want := range []string{"Container logs", `name="retention_months"`, `value="9"`, `action="/settings/container-logs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}

	rec := postFormAs(t, api, uid, "/settings/container-logs", url.Values{"retention_months": {"6"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Container log retention updated") {
		t.Errorf("update: status %d body %.200s", rec.Code, rec.Body.String())
	}
	if len(fake.updatedTo) != 1 || fake.updatedTo[0] != 6 {
		t.Errorf("updated to %v, want [6]", fake.updatedTo)
	}
}

func TestSettings_ContainerLogRetentionRejectsBadInputWithAReason(t *testing.T) {
	fake := &fakeContainerLogs{retentionErr: fmt.Errorf("%w: keep logs between 1 and 24 months", containerlogs.ErrInvalidRetention)}
	api, uid := settingsAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/settings/container-logs", url.Values{"retention_months": {"99"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "between 1 and 24 months") {
		t.Errorf("status %d body %.300s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"", "abc", "1.5"} {
		fake.updatedTo = nil
		fake.retentionErr = nil
		rec := postFormAs(t, api, uid, "/settings/container-logs", url.Values{"retention_months": {bad}})
		if !strings.Contains(rec.Body.String(), "whole number of months") || len(fake.updatedTo) != 0 {
			t.Errorf("%q: body %.200s updated %v", bad, rec.Body.String(), fake.updatedTo)
		}
	}
}

func TestSettings_ContainerLogRetentionIsForbiddenBelowAdmin(t *testing.T) {
	fake := &fakeContainerLogs{}
	api, uid := settingsAPI(t, db.TierPowerDev, fake)
	if rec := postFormAs(t, api, uid, "/settings/container-logs", url.Values{"retention_months": {"6"}}); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(fake.updatedTo) != 0 {
		t.Error("a non-admin changed the retention")
	}
}
