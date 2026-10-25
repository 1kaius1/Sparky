// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/containerlogs"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// containerLogViewer is the subset of *containerlogs.Service this package
// needs for the archived-log pages. Both calls check
// rbac.CanViewInstanceLogs themselves.
type containerLogViewer interface {
	List(ctx context.Context, actor rbac.Actor, limit int) ([]*db.ContainerLogArchive, error)
	Read(ctx context.Context, actor rbac.Actor, id string) (*db.ContainerLogArchive, string, error)
	LatestForInstances(ctx context.Context, actor rbac.Actor, instanceIDs []string) (map[string]string, error)
	Live(ctx context.Context, actor rbac.Actor, instanceID string, lines int) (*containerlogs.LiveLog, error)

	// The retention setting on the Settings page (Admin).
	Settings(ctx context.Context, actor rbac.Actor) (*db.ContainerLogSettings, error)
	UpdateRetention(ctx context.Context, actor rbac.Actor, months int) error
}

const (
	// containerLogListLimit is how many archives the list page shows.
	containerLogListLimit = 200

	// containerLogViewLines is how many trailing lines the view page shows;
	// the full text is a download away. Keeps a page of a huge log light.
	containerLogViewLines = 5000
)

// ansiEscape matches terminal colour and cursor sequences, which an engine's
// log can contain and which would otherwise show as noise in a <pre>.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// containerLogRow is one archived log in the list.
type containerLogRow struct {
	ID            string
	CreatedAt     string
	NodeName      string
	ProfileName   string
	ContainerName string
	Reason        string
	State         string
	ExitCode      string
	Signal        bool
	OOMKilled     bool
	Lines         int
	Size          string
	Truncated     bool
}

type containerLogsPageData struct {
	Rows []containerLogRow
}

type containerLogViewData struct {
	Row containerLogRow

	// StartedAt and FinishedAt are empty when unknown.
	StartedAt  string
	FinishedAt string

	// LinesRequested is "all" or a count.
	LinesRequested string

	// Text is the log, ANSI sequences stripped, limited to the last
	// containerLogViewLines lines. Shown reports how many lines that is;
	// Cut is true when earlier lines were left out of the view.
	Text  string
	Shown int
	Cut   bool
}

// containerLogReasonLabel makes a stored reason readable.
func containerLogReasonLabel(reason string) string {
	switch reason {
	case "unload":
		return "unloaded"
	case "failed_launch":
		return "failed launch"
	case "replaced":
		return "replaced by a new launch"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}

func containerLogRowOf(a *db.ContainerLogArchive) containerLogRow {
	exit := ""
	signalled := false
	if a.ExitCode != nil {
		exit = fmt.Sprintf("%d", *a.ExitCode)
		// A bare-metal process killed by a signal has no exit code; the
		// agent reports -1 for it.
		signalled = *a.ExitCode == -1
	}
	return containerLogRow{
		ID:            a.ID,
		CreatedAt:     a.CreatedAt.Format("2006-01-02 15:04:05 MST"),
		NodeName:      a.NodeName,
		ProfileName:   a.ProfileName,
		ContainerName: a.ContainerName,
		Reason:        containerLogReasonLabel(a.Reason),
		State:         a.State,
		ExitCode:      exit,
		Signal:        signalled,
		OOMKilled:     a.OOMKilled,
		Lines:         a.LinesKept,
		Size:          formatLogSize(a.SizeBytes),
		Truncated:     a.Truncated,
	}
}

// formatLogSize renders a log's size. Logs are typically kilobytes, so unlike
// formatBytes (whose smallest unit is MB, for model files) it shows B and KiB.
func formatLogSize(b int64) string {
	const kib, mib = 1024, 1024 * 1024
	switch {
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/mib)
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/kib)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// tailLines returns the last n lines of s and whether anything before them
// was left out.
func tailLines(s string, n int) (string, bool) {
	trimmed := strings.TrimSuffix(s, "\n")
	idx := len(trimmed)
	for i := 0; i < n; i++ {
		j := strings.LastIndexByte(trimmed[:idx], '\n')
		if j < 0 {
			return s, false
		}
		idx = j
	}
	return s[idx+1:], true
}

// containerLogActor resolves the viewer, answering the request itself on
// failure. ok is false when the handler should stop.
func (a *API) containerLogActor(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for container logs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	return actor, true
}

func (a *API) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.containerLogActor(w, r)
	if !ok {
		return
	}
	archives, err := a.containerLogs.List(r.Context(), actor, containerLogListLimit)
	if errors.Is(err, rbac.ErrNotPermitted) {
		a.renderForbidden(w, r, actor.Tier)
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: list container logs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := containerLogsPageData{Rows: make([]containerLogRow, 0, len(archives))}
	for _, arc := range archives {
		data.Rows = append(data.Rows, containerLogRowOf(arc))
	}
	a.render(w, r, "container_logs", "Container logs", data)
}

func (a *API) handleContainerLog(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.containerLogActor(w, r)
	if !ok {
		return
	}
	arc, text, err := a.containerLogs.Read(r.Context(), actor, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, containerlogs.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "container log not found")
		return
	case err != nil:
		a.logger.Printf("httpapi: read container log: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	shown, cut := tailLines(ansiEscape.ReplaceAllString(text, ""), containerLogViewLines)
	data := containerLogViewData{
		Row:            containerLogRowOf(arc),
		LinesRequested: "all",
		Text:           shown,
		Shown:          strings.Count(shown, "\n"),
		Cut:            cut,
	}
	if !strings.HasSuffix(shown, "\n") && shown != "" {
		data.Shown++
	}
	if arc.LinesRequested != nil {
		data.LinesRequested = fmt.Sprintf("%d", *arc.LinesRequested)
	}
	if arc.StartedAt != nil {
		data.StartedAt = arc.StartedAt.Format("2006-01-02 15:04:05 MST")
	}
	if arc.FinishedAt != nil {
		data.FinishedAt = arc.FinishedAt.Format("2006-01-02 15:04:05 MST")
	}
	a.render(w, r, "container_log", "Container log", data)
}

func (a *API) handleContainerLogDownload(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.containerLogActor(w, r)
	if !ok {
		return
	}
	arc, text, err := a.containerLogs.Read(r.Context(), actor, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "developer tier required")
		return
	case errors.Is(err, containerlogs.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "container log not found")
		return
	case err != nil:
		a.logger.Printf("httpapi: download container log: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The file name is built from fixed parts and the archive's own id, never
	// from the container name an agent reported.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="container-log-%s-%s.txt"`, arc.CreatedAt.UTC().Format("20060102-150405"), arc.ID[:8]))
	_, _ = w.Write([]byte(text))
}

// liveLineOptions are the line counts the live log page offers.
var liveLineOptions = []int{100, 500, 1000, 2000, 5000}

// instanceLogsPageData is the live log page's view model. Exactly one of Text
// and Error is meaningful: Error is a reason shown in a banner when the node
// could not be read, and ArchiveLogID, when set, is the instance's newest
// saved log - the way to see what the container said when a live read is no
// longer possible.
type instanceLogsPageData struct {
	InstanceID   string
	ProfileName  string
	Lines        int
	LineOptions  []int
	State        string
	ExitCode     string
	Text         string
	Shown        int
	Error        string
	ArchiveLogID string
}

// handleInstanceLogs is GET /instances/{id}/logs?lines=N: an instance's
// current output, read from its node on demand. Nothing is stored. The tier
// check is inside containerlogs.Service.
func (a *API) handleInstanceLogs(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.containerLogActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	lines := containerlogs.DefaultLiveLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	if lines > containerlogs.MaxLiveLines {
		lines = containerlogs.MaxLiveLines
	}

	data := instanceLogsPageData{InstanceID: id, Lines: lines, LineOptions: liveLineOptions}
	live, err := a.containerLogs.Live(ctx, actor, id, lines)
	status := http.StatusOK
	var agentErr *containerlogs.AgentError
	switch {
	case err == nil:
		text, _ := tailLines(ansiEscape.ReplaceAllString(live.Text, ""), containerLogViewLines)
		data.ProfileName = live.ProfileName
		data.State = live.Meta.State
		if live.Meta.ExitCode != nil {
			data.ExitCode = fmt.Sprintf("%d", *live.Meta.ExitCode)
		}
		data.Text = text
		data.Shown = strings.Count(text, "\n")
		if text != "" && !strings.HasSuffix(text, "\n") {
			data.Shown++
		}
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, containerlogs.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "instance not found")
		return
	case errors.Is(err, containerlogs.ErrBusy):
		status = http.StatusTooManyRequests
		data.Error = err.Error()
	case errors.Is(err, containerlogs.ErrNodeOffline), errors.Is(err, containerlogs.ErrAgentSilent):
		data.Error = err.Error()
	case errors.As(err, &agentErr):
		data.Error = agentErr.Message
	default:
		a.logger.Printf("httpapi: live container log for instance %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// When the live read failed, point at the saved log, if there is one.
	if data.Error != "" {
		if ids, err := a.containerLogs.LatestForInstances(ctx, actor, []string{id}); err != nil {
			a.logger.Printf("httpapi: look up saved container log for instance %s: %v", id, err)
		} else {
			data.ArchiveLogID = ids[id]
		}
	}
	w.WriteHeader(status)
	a.render(w, r, "instance_logs", "Instance log", data)
}
