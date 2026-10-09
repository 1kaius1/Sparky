// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
