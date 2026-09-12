// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// nodeLister is the subset of *nodes.Service this package needs, narrow
// enough to fake in tests without a real Postgres instance.
type nodeLister interface {
	ListNodes(ctx context.Context) ([]*db.Node, error)
}

// profileLister is the subset of *profiles.Service this package needs.
type profileLister interface {
	ListProfiles(ctx context.Context) ([]*db.Profile, error)
}

// instanceLister is the subset of *lifecycle.Service this package needs.
type instanceLister interface {
	ListInstances(ctx context.Context) ([]*db.RunningInstance, error)
}

// dashboardData is the Dashboard page's view model - a fleet-level
// summary, per CLAUDE.md Frontend Conventions' Dashboard sidebar tier
// ("Read-only"). Viewing it is never audited - see ARCHITECTURE.md Audit
// Log ("dashboard polling, listing resources" are explicitly excluded).
type dashboardData struct {
	TotalNodes       int
	OnlineNodes      int
	TotalInstances   int
	RunningCount     int
	RunningInstances []dashboardRunningRow
	// LiveDataJSON seeds web/static/js/dashboard.js's load strips on the
	// initial render, exactly the way metrics.html seeds chartData - same
	// shape as GET /dashboard/live-data's body, so the strips are populated
	// on first paint instead of blank until the first ~5s telemetry tick.
	LiveDataJSON template.JS
}

// dashboardRunningRow is one row of the Dashboard's "Running instances"
// table - what is loaded right now, joined with its profile for the model
// name and engine type. Only instances in a live status (running /
// starting / stopping) appear; a stopped or failed instance is history,
// not something "running".
type dashboardRunningRow struct {
	InstanceID string
	Model      string
	Engine     string
	NodeName   string
	StartedAt  string
	Uptime     string
	Status     string
	// Health is empty unless the instance is actually running - a
	// still-starting instance hasn't reached its first periodic health
	// check, and surfacing db.InstanceHealthUnknown there would read as a
	// real verdict rather than "not applicable yet" (same rule as the
	// Model profiles page's row, see internal/httpapi/model_profiles.go).
	Health string
	Port   string
}

// dashboardLiveData is the JSON shape shared by handleDashboardLiveData's
// endpoint and dashboardData.LiveDataJSON's initial seed - one entry per
// live running instance, each carrying the last dashboardLoadBars
// utilization and memory-percent readings for its correlated GPU
// telemetry.
type dashboardLiveData struct {
	Instances []dashboardInstanceLoad `json:"instances"`
}

type dashboardInstanceLoad struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Health string `json:"health"`
	Uptime string `json:"uptime"`
	// GPUUtil/GPUMem are percentages (0-100), oldest first, at most
	// dashboardLoadBars entries. Memory is used/total*100, not absolute MB
	// - the dashboard strips are a coarse "how loaded" indicator; absolute
	// values and full history are the Metrics page's job.
	GPUUtil []float64 `json:"gpuUtil"`
	GPUMem  []float64 `json:"gpuMem"`
}

// dashboardLoadBars is how many recent readings each load strip shows -
// about two minutes of history at the default 5s telemetry poll interval.
// Deliberately coarse: this is an at-a-glance "how hard is it working"
// strip, not the Metrics page's hour-long chart.
const dashboardLoadBars = 24

// isLiveInstanceStatus reports whether an instance in this status belongs
// on the Dashboard's "Running instances" table - the transient/active
// states, not stopped or failed.
func isLiveInstanceStatus(s db.RunningInstanceStatus) bool {
	return s == db.RunningInstanceStatusRunning ||
		s == db.RunningInstanceStatusStarting ||
		s == db.RunningInstanceStatusStopping
}

// humanizeUptime renders a coarse, dashboard-grade elapsed time - whole
// seconds under a minute, whole minutes under an hour, "Nh Nm" above that.
// Not meant to be precise to the second on an old instance; the exact
// timeline lives on the Metrics page.
func humanizeUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func round1(f float64) float64 {
	return math.Round(f*10) / 10
}

// buildDashboardLiveData reduces recent per-GPU telemetry to the
// per-instance load strips the Dashboard shows. recentGPU is expected
// chronological (oldest first) - db.GPUMetricsRepository.Recent's own
// ordering - so appends preserve order and the last dashboardLoadBars
// entries are the newest.
//
// GPU readings are correlated to a running instance by the agent-side
// running_instance_id stamped at collection time (see
// internal/metrics.Service.HandleTelemetry). That correlation attributes a
// tick to at most one active instance per node, so on a node running more
// than one instance the strips would reflect the node total on each row -
// an accepted limitation, documented in PLANNING.md; the fleet runs one
// instance per node today. A node reporting more than one GPU index for
// the same instance is averaged per reading (a no-op on single-GPU
// hardware).
func buildDashboardLiveData(instances []*db.RunningInstance, recentGPU []*db.GPUMetric) dashboardLiveData {
	type reading struct {
		utilSum, memSum float64
		n               int
	}
	byInstance := make(map[string]map[time.Time]*reading)
	order := make(map[string][]time.Time)
	for _, g := range recentGPU {
		if g.RunningInstanceID == nil {
			continue
		}
		id := *g.RunningInstanceID
		perTime, ok := byInstance[id]
		if !ok {
			perTime = make(map[time.Time]*reading)
			byInstance[id] = perTime
		}
		r, ok := perTime[g.RecordedAt]
		if !ok {
			r = &reading{}
			perTime[g.RecordedAt] = r
			order[id] = append(order[id], g.RecordedAt)
		}
		r.utilSum += g.UtilizationPct
		if g.MemoryTotalMB > 0 {
			r.memSum += g.MemoryUsedMB / g.MemoryTotalMB * 100
		}
		r.n++
	}

	out := dashboardLiveData{Instances: make([]dashboardInstanceLoad, 0, len(instances))}
	for _, inst := range instances {
		if !isLiveInstanceStatus(inst.Status) {
			continue
		}
		load := dashboardInstanceLoad{
			ID:     inst.ID,
			Status: string(inst.Status),
			Health: string(inst.HealthStatus),
			Uptime: humanizeUptime(time.Since(inst.StartedAt)),
		}
		times := order[inst.ID]
		if n := len(times); n > dashboardLoadBars {
			times = times[n-dashboardLoadBars:]
		}
		for _, ts := range times {
			r := byInstance[inst.ID][ts]
			load.GPUUtil = append(load.GPUUtil, round1(r.utilSum/float64(r.n)))
			load.GPUMem = append(load.GPUMem, round1(r.memSum/float64(r.n)))
		}
		out.Instances = append(out.Instances, load)
	}
	return out
}

func (a *API) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for dashboard: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	instances, err := a.instances.ListInstances(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list running instances for dashboard: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profiles, err := a.profiles.ListProfiles(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list profiles for dashboard: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recentGPU, err := a.metrics.ListRecentGPU(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent gpu metrics for dashboard: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	onlineNodes := 0
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
		if n.AgentStatus == db.AgentStatusOnline {
			onlineNodes++
		}
	}
	profileByID := make(map[string]*db.Profile, len(profiles))
	for _, p := range profiles {
		profileByID[p.ID] = p
	}

	runningCount := 0
	rows := make([]dashboardRunningRow, 0, len(instances))
	for _, inst := range instances {
		if inst.Status == db.RunningInstanceStatusRunning {
			runningCount++
		}
		if !isLiveInstanceStatus(inst.Status) {
			continue
		}
		row := dashboardRunningRow{
			InstanceID: inst.ID,
			NodeName:   nodeNames[inst.PrimaryNodeID],
			StartedAt:  inst.StartedAt.Format("2006-01-02 15:04:05 MST"),
			Uptime:     humanizeUptime(time.Since(inst.StartedAt)),
			Status:     string(inst.Status),
			Port:       "-",
		}
		if inst.Status == db.RunningInstanceStatusRunning {
			row.Health = string(inst.HealthStatus)
		}
		if p, ok := profileByID[inst.ProfileID]; ok {
			row.Model = p.ModelRef
			row.Engine = string(p.EngineType)
		}
		if inst.ActualPort != nil {
			row.Port = fmt.Sprintf("%d", *inst.ActualPort)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].NodeName != rows[j].NodeName {
			return rows[i].NodeName < rows[j].NodeName
		}
		return rows[i].StartedAt < rows[j].StartedAt
	})

	encoded, err := json.Marshal(buildDashboardLiveData(instances, recentGPU))
	if err != nil {
		a.logger.Printf("httpapi: encode dashboard live data seed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "dashboard", "Dashboard", dashboardData{
		TotalNodes:       len(nodes),
		OnlineNodes:      onlineNodes,
		TotalInstances:   len(instances),
		RunningCount:     runningCount,
		RunningInstances: rows,
		LiveDataJSON:     template.JS(encoded),
	})
}

// handleDashboardLiveData is the Dashboard's load-strip live-update fetch
// target - see web/static/js/dashboard.js's sparkyDashboardLiveUpdate. A
// telemetry tick fetches this instead of triggering a full page refetch, so
// the strips advance in place without visibly redrawing the table. Same
// Read-only/no-audit posture as /dashboard itself - reads are never audited
// (ARCHITECTURE.md Audit Log) - and the same shape as the Metrics page's
// /metrics/chart-data.
func (a *API) handleDashboardLiveData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	instances, err := a.instances.ListInstances(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list running instances for dashboard live data: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recentGPU, err := a.metrics.ListRecentGPU(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent gpu metrics for dashboard live data: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(buildDashboardLiveData(instances, recentGPU)); err != nil {
		a.logger.Printf("httpapi: encode dashboard live data response: %v", err)
	}
}
