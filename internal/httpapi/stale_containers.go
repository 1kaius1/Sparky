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
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
	"github.com/1kaius1/Sparky/internal/stalecontainers"
)

// staleContainerManager is the subset of *stalecontainers.Service this package
// needs: the Settings page's monitoring section and the Container cleanup
// page. Every method is gated to Admin inside the service.
type staleContainerManager interface {
	Settings(ctx context.Context, actor rbac.Actor) (*stalecontainers.SettingsView, error)
	UpdateSettings(ctx context.Context, actor rbac.Actor, in stalecontainers.SettingsInput) error
	Preview(ctx context.Context, actor rbac.Actor) (*stalecontainers.PreviewResult, error)
	RemoveOne(ctx context.Context, actor rbac.Actor, nodeID, instanceID string, lines int) (stalecontainers.RemoveResult, error)
}

// staleSettingsView is the monitoring section of the Settings page.
type staleSettingsView struct {
	Mode         string
	ScheduleKind string // "weekly" or "every_days": which half of the schedule form is shown selected
	Weekdays     []weekdayOption
	Times        string
	EveryDays    int
	Description  string // the saved schedule as a sentence, "" if none
	Timezone     string
	NextRun      string
	MinAgeHours  int
	ArchiveLines int
	ArchiveAll   bool
	LastRun      *staleLastRunView
	UpdatedBy    string
	UpdatedAt    string
}

type weekdayOption struct {
	Value   int
	Name    string
	Checked bool
}

type staleLastRunView struct {
	Mode, StartedAt, Finished   string
	NodesScanned, NodesSkipped  int
	Stale, Recent, Orphans      int
	Removed, Gone, Kept, Failed int
	IsPrune                     bool
}

// staleSettingsViewOf builds the Settings section from the service's view.
func (a *API) staleSettingsViewOf(ctx context.Context, v *stalecontainers.SettingsView) *staleSettingsView {
	cfg := v.Settings
	out := &staleSettingsView{
		Mode:         string(cfg.Mode),
		ScheduleKind: string(schedule.KindWeekly),
		EveryDays:    1,
		Timezone:     v.Timezone,
		MinAgeHours:  cfg.MinAgeHours,
		ArchiveLines: cfg.ArchiveLines,
		ArchiveAll:   cfg.ArchiveLines == 0,
		UpdatedBy:    a.resolveUserName(ctx, cfg.UpdatedBy),
		UpdatedAt:    cfg.UpdatedAt.Format("2006-01-02 15:04:05 MST"),
	}
	if out.ArchiveAll {
		out.ArchiveLines = 2000 // what the number box shows while "all" is ticked
	}
	chosen := map[int]bool{}
	if v.Spec != nil {
		out.Description = v.Spec.Describe()
		out.ScheduleKind = string(v.Spec.Kind)
		for _, d := range v.Spec.Weekdays {
			chosen[d] = true
		}
		out.Times = strings.Join(v.Spec.Times, ", ")
		if v.Spec.Kind == schedule.KindEveryDays {
			out.EveryDays = v.Spec.EveryDays
		}
	}
	for d := 0; d <= 6; d++ {
		out.Weekdays = append(out.Weekdays, weekdayOption{Value: d, Name: schedule.WeekdayName(d), Checked: chosen[d]})
	}
	if v.NextRun != nil {
		out.NextRun = v.NextRun.Format("2006-01-02 15:04 MST")
		if loc, err := time.LoadLocation(v.Timezone); err == nil {
			out.NextRun = v.NextRun.In(loc).Format("2006-01-02 15:04 MST")
		}
	}
	if r := v.LastRun; r != nil {
		out.LastRun = &staleLastRunView{
			Mode: r.Mode, StartedAt: r.StartedAt.Format("2006-01-02 15:04:05 MST"), Finished: r.FinishedAt.Format("15:04:05"),
			NodesScanned: r.NodesScanned, NodesSkipped: r.NodesSkipped, Stale: r.Stale, Recent: r.Recent, Orphans: r.Orphans,
			Removed: r.Removed, Gone: r.Gone, Kept: r.Kept, Failed: r.Failed, IsPrune: r.Mode == string(db.StaleContainerModePrune),
		}
	}
	return out
}

var timeOfDaySplit = regexp.MustCompile(`[\s,;]+`)

// staleSettingsInputFrom reads the monitoring form. It only parses: every rule
// about what is acceptable is the service's.
func staleSettingsInputFrom(r *http.Request) (stalecontainers.SettingsInput, string) {
	in := stalecontainers.SettingsInput{
		Mode:         db.StaleContainerMode(r.PostFormValue("stale_mode")),
		ConfirmPrune: r.PostFormValue("confirm_prune") != "",
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("min_age_hours")))
	if err != nil {
		return in, "the minimum age must be a whole number of hours"
	}
	in.MinAgeHours = n

	if r.PostFormValue("archive_all") != "" {
		in.ArchiveLines = 0
	} else {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("archive_lines")))
		if err != nil {
			return in, "log lines to keep must be a whole number (or tick All)"
		}
		in.ArchiveLines = n
	}

	switch schedule.Kind(r.PostFormValue("schedule_kind")) {
	case schedule.KindEveryDays:
		in.Schedule.Kind = schedule.KindEveryDays
		raw := strings.TrimSpace(r.PostFormValue("every_days"))
		if raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return in, "every N days needs N as a whole number"
			}
			in.Schedule.EveryDays = n
		}
		in.HasSchedule = raw != ""
	default:
		in.Schedule.Kind = schedule.KindWeekly
		for _, v := range r.PostForm["weekdays"] {
			d, err := strconv.Atoi(v)
			if err != nil {
				return in, "a weekday was not understood"
			}
			in.Schedule.Weekdays = append(in.Schedule.Weekdays, d)
		}
		for _, t := range timeOfDaySplit.Split(strings.TrimSpace(r.PostFormValue("times")), -1) {
			if t != "" {
				in.Schedule.Times = append(in.Schedule.Times, t)
			}
		}
		in.HasSchedule = len(in.Schedule.Weekdays) > 0 || len(in.Schedule.Times) > 0
	}
	// Switching the monitoring on always submits the schedule, so the service
	// can say what is missing from it; switching it off with the schedule
	// fields empty keeps whatever was saved.
	if in.Mode != db.StaleContainerModeNone {
		in.HasSchedule = true
	}
	return in, ""
}

// handleUpdateStaleContainers saves the stale container monitoring settings -
// see stalecontainers.Service.UpdateSettings. A refused change redisplays the
// page with the reason, never a 500.
func (a *API) handleUpdateStaleContainers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for stale container settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{StaleError: "invalid form submission"})
		return
	}
	in, problem := staleSettingsInputFrom(r)
	if problem != "" {
		a.renderSettingsPage(w, r, actor, settingsPageData{StaleError: problem})
		return
	}

	err = a.staleContainers.UpdateSettings(ctx, actor, in)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, stalecontainers.ErrInvalidSettings), errors.Is(err, stalecontainers.ErrPruneNotConfirmed):
		a.renderSettingsPage(w, r, actor, settingsPageData{StaleError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: update stale container settings: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{StaleError: "failed to update the settings"})
		return
	}
	a.renderSettingsPage(w, r, actor, settingsPageData{StaleSuccess: true})
}

// actorOrFail resolves who is signed in for a handler, writing the error
// response itself when it cannot (what names the handler in the log). It
// exists so the three cleanup handlers do not each repeat the same
// identity-to-actor boilerplate; the RBAC decision itself stays in the service.
func (a *API) actorOrFail(w http.ResponseWriter, r *http.Request, what string) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for %s: %v", what, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	return actor, true
}

// ---- the Container cleanup page ----

type lineOption struct {
	Value    int
	Label    string
	Selected bool
}

// cleanupLineOptions are the choices for how many log lines to keep, with the
// configured default selected (and added if it is not one of the usual).
func cleanupLineOptions(def int) []lineOption {
	values := []int{50, 200, 500, 2000, 5000, 0}
	found := false
	for _, v := range values {
		if v == def {
			found = true
		}
	}
	if !found {
		values = append([]int{def}, values...)
	}
	out := make([]lineOption, len(values))
	for i, v := range values {
		label := strconv.Itoa(v) + " lines"
		if v == 0 {
			label = "All"
		}
		out[i] = lineOption{Value: v, Label: label, Selected: v == def}
	}
	return out
}

type cleanupPageData struct {
	MinAgeHours int
	Mode        string
}

// handleContainerCleanup renders the Container cleanup page shell. The scan
// itself (which asks every node and can take a while) loads into it
// afterwards, so the page appears at once.
func (a *API) handleContainerCleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "container cleanup page")
	if !ok {
		return
	}
	view, err := a.staleContainers.Settings(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err != nil:
		a.logger.Printf("httpapi: get stale container settings for the cleanup page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "container_cleanup", "Container cleanup", cleanupPageData{MinAgeHours: view.Settings.MinAgeHours, Mode: string(view.Settings.Mode)})
}

type staleRowView struct {
	NodeID, InstanceID        string
	Container, Profile, State string
	Exit                      string
	Stopped                   string
	Reason                    string
	ProfileKnown              bool
	InstanceStatus            string
}

type staleNodeView struct {
	Name, NodeID string
	Status       string
	StatusClass  string
	Error        string
	Truncated    bool
	Stale        []staleRowView
	Recent       []staleRowView
	Orphans      []staleRowView
	Live, Other  int
}

type staleScanData struct {
	Error       string
	MinAgeHours int
	Lines       []lineOption
	Nodes       []staleNodeView
	AnyStale    bool
}

// nodeScanStatusClass picks the existing status colour for a node's part of a
// scan: green for answered, grey for not connected, red for failed.
func nodeScanStatusClass(s stalecontainers.NodeStatus) string {
	switch s {
	case stalecontainers.ScanDone:
		return "online"
	case stalecontainers.ScanOffline:
		return "offline"
	default:
		return "failed"
	}
}

func staleRowOf(nodeID string, it stalecontainers.Item, now time.Time) staleRowView {
	row := staleRowView{
		NodeID: nodeID, InstanceID: it.InstanceID, Container: it.ContainerName, Profile: it.ProfileName, State: it.State,
		Reason: it.Reason, InstanceStatus: it.InstanceStatus,
	}
	if it.ExitCode != nil {
		row.Exit = "exit code " + strconv.Itoa(*it.ExitCode)
	}
	if it.OOMKilled {
		if row.Exit != "" {
			row.Exit += ", "
		}
		row.Exit += "out of memory"
	}
	if !it.StoppedAt.IsZero() {
		row.Stopped = it.StoppedAt.Format("2006-01-02 15:04 MST") + " (" + ageWords(now.Sub(it.StoppedAt)) + " ago)"
	}
	return row
}

// ageWords writes an age as "5 days", "3 hours" or "20 minutes".
func ageWords(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= time.Hour:
		return "1 hour"
	case d >= time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	default:
		return "under a minute"
	}
}

// handleContainerCleanupScan scans every container node now and returns the
// results as an htmx fragment: what is stale (with a remove control each),
// what is too recent, what is protected.
func (a *API) handleContainerCleanupScan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "container cleanup scan")
	if !ok {
		return
	}
	res, err := a.staleContainers.Preview(ctx, actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err != nil:
		a.logger.Printf("httpapi: container cleanup scan: %v", err)
		a.renderPartial(w, "stale_scan", staleScanData{Error: "The scan failed. Try again."})
		return
	}

	now := time.Now()
	data := staleScanData{MinAgeHours: int(res.MinAge / time.Hour), Lines: cleanupLineOptions(res.ArchiveLines)}
	for _, ns := range res.Nodes {
		nv := staleNodeView{Name: ns.NodeName, NodeID: ns.NodeID, Status: string(ns.Status), StatusClass: nodeScanStatusClass(ns.Status), Error: ns.Error, Truncated: ns.Truncated}
		for _, it := range ns.Items {
			switch it.Verdict {
			case stalecontainers.VerdictStale:
				nv.Stale = append(nv.Stale, staleRowOf(ns.NodeID, it, now))
			case stalecontainers.VerdictRecent:
				nv.Recent = append(nv.Recent, staleRowOf(ns.NodeID, it, now))
			case stalecontainers.VerdictOrphanLive:
				nv.Orphans = append(nv.Orphans, staleRowOf(ns.NodeID, it, now))
			case stalecontainers.VerdictLive:
				nv.Live++
			default:
				nv.Other++
			}
		}
		if len(nv.Stale) > 0 {
			data.AnyStale = true
		}
		data.Nodes = append(data.Nodes, nv)
	}
	a.renderPartial(w, "stale_scan", data)
}

type staleRemoveData struct {
	InstanceID string
	Container  string
	Profile    string
	Class      string // "success", "error" or "pending" - which CSS class the message wears
	Message    string
}

// handleContainerCleanupRemove removes one stale container - the per-row
// button - and answers with the row's replacement. Every outcome, including a
// refusal, is a 200 fragment that explains itself in place: htmx does not swap
// an error status, and the person clicking needs to see what happened.
func (a *API) handleContainerCleanupRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := a.actorOrFail(w, r, "container cleanup removal")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form submission", http.StatusBadRequest)
		return
	}
	nodeID, instanceID := r.PostFormValue("node_id"), r.PostFormValue("instance_id")
	lines, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("lines")))
	if err != nil {
		http.Error(w, "log lines must be a whole number", http.StatusBadRequest)
		return
	}

	res, err := a.staleContainers.RemoveOne(ctx, actor, nodeID, instanceID, lines)
	out := staleRemoveData{InstanceID: instanceID, Container: res.ContainerName, Profile: res.ProfileName}
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, stalecontainers.ErrInvalidRequest):
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	case errors.Is(err, stalecontainers.ErrNotStale):
		out.Class, out.Message = "error", "Not removed. "+strings.TrimPrefix(err.Error(), stalecontainers.ErrNotStale.Error()+": ")
	case errors.Is(err, stalecontainers.ErrNodeUnavailable):
		out.Class, out.Message = "error", "Not removed. "+strings.TrimPrefix(err.Error(), stalecontainers.ErrNodeUnavailable.Error()+": ")
	case errors.Is(err, stalecontainers.ErrAuditAfterRemoval):
		// The removal happened and cannot be undone; say so, and say the record is missing.
		a.logger.Printf("httpapi: %v", err)
		out.Class, out.Message = "error", "The container was removed and its log saved, but writing the audit log entry failed. Tell whoever runs this server."
	case err != nil:
		a.logger.Printf("httpapi: remove stale container: %v", err)
		out.Class, out.Message = "error", "Not removed: something went wrong. Try again."
	default:
		out.Class, out.Message = removeOutcomeMessage(res)
	}
	a.renderPartial(w, "stale_row_result", out)
}

// removeOutcomeMessage turns an agent outcome into a sentence and a style.
func removeOutcomeMessage(res stalecontainers.RemoveResult) (class, message string) {
	switch res.Outcome {
	case agentproto.RemoveOutcomeRemoved:
		return "success", "Removed. Its log was saved first (see Container logs)."
	case agentproto.RemoveOutcomeGone:
		return "success", "Already gone."
	case agentproto.RemoveOutcomeKept:
		return "error", "Kept: the log could not be saved, so the container was left in place. " + res.Detail
	case agentproto.RemoveOutcomeRefused:
		return "error", "Not removed: the node refused. " + res.Detail
	default:
		return "error", "Not removed: " + res.Detail
	}
}
