// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"

	"github.com/1kaius1/Sparky/internal/db"
)

// metricsLister is the subset of *metrics.Service this package needs -
// unguarded by RBAC, same reasoning as nodeLister/profileLister/
// instanceLister/transferLister (CLAUDE.md Frontend Conventions, Metrics'
// sidebar tier "Read-only").
type metricsLister interface {
	ListLatestByNode(ctx context.Context) ([]*db.Metric, error)
	ListRecent(ctx context.Context) ([]*db.Metric, error)
	ListLatestGPUByNode(ctx context.Context) ([]*db.GPUMetric, error)
	ListRecentGPU(ctx context.Context) ([]*db.GPUMetric, error)
}

// metricsPageData is the Metrics page's view model.
type metricsPageData struct {
	Rows          []metricsGPURow
	ChartDataJSON template.JS
}

// metricsGPURow is one row of the Metrics page's summary table - one per
// (node, GPU index), since GPU readings are now genuinely per-device (see
// SCHEMA.md GPU metrics). CPUUtilizationPct/SystemMemory are node-level
// values (a node has one CPU/RAM pool regardless of GPU count), repeated on
// every one of that node's GPU rows - the simplest correct rendering,
// avoiding HTML rowspan complexity for what's still a small table at this
// project's scale.
type metricsGPURow struct {
	NodeName          string
	GPUIndex          int
	GPUUtilizationPct string
	GPUMemory         string
	CPUUtilizationPct string
	SystemMemory      string
	RunningModel      string
	Port              string
	RecordedAt        string
}

// chartPoint/chartSeries are the JSON shape web/static/js/metrics.js
// expects - see that file. Point values are pre-formatted server-side
// (RecordedAt as a fixed-format string) rather than left as raw
// timestamps, so the chart's category axis and this page's own table use
// the same rendering, and no date-parsing library needs vendoring
// alongside Chart.js.
type chartPoint struct {
	X string  `json:"x"`
	Y float64 `json:"y"`
}

type chartSeries struct {
	Label  string       `json:"label"`
	Points []chartPoint `json:"points"`
}

// metricsChartData is the four-panel chart data shape - GPU utilization
// and GPU memory are one series per (node, GPU index); CPU and system
// memory are one series per node (not GPU-indexed - a node has one CPU/RAM
// pool regardless of GPU count). Shared by handleMetrics' inline page
// render and handleMetricsChartData's live-update JSON endpoint.
type metricsChartData struct {
	GPUUtilization []chartSeries `json:"gpuUtilization"`
	GPUMemory      []chartSeries `json:"gpuMemory"`
	CPU            []chartSeries `json:"cpu"`
	SystemMemory   []chartSeries `json:"systemMemory"`
}

func formatMetricMB(used, total float64) string {
	return fmt.Sprintf("%.0f MB / %.0f MB", used, total)
}

// percentOf returns used/total as a percentage, guarding against a zero
// total (a malformed or not-yet-populated reading) rather than dividing by
// zero.
func percentOf(used, total float64) float64 {
	if total == 0 {
		return 0
	}
	return used / total * 100
}

// buildMetricsChartData groups recent readings into the four chart panels.
// recentNode/recentGPU come back most-recently-recorded first (db.MetricsRepository/
// db.GPUMetricsRepository's own ordering) - reversed here so each series
// renders chronologically left-to-right on the chart, same reasoning as
// this function's pre-refactor inline predecessor.
func buildMetricsChartData(recentNode []*db.Metric, recentGPU []*db.GPUMetric, nodeNames map[string]string) metricsChartData {
	type gpuKey struct {
		nodeID string
		index  int
	}
	utilByGPU := make(map[gpuKey][]chartPoint)
	memByGPU := make(map[gpuKey][]chartPoint)
	var gpuOrder []gpuKey
	for i := len(recentGPU) - 1; i >= 0; i-- {
		m := recentGPU[i]
		key := gpuKey{m.NodeID, m.GPUIndex}
		if _, ok := utilByGPU[key]; !ok {
			gpuOrder = append(gpuOrder, key)
		}
		x := m.RecordedAt.Format("15:04:05")
		utilByGPU[key] = append(utilByGPU[key], chartPoint{X: x, Y: m.UtilizationPct})
		memByGPU[key] = append(memByGPU[key], chartPoint{X: x, Y: percentOf(m.MemoryUsedMB, m.MemoryTotalMB)})
	}
	gpuUtilization := make([]chartSeries, 0, len(gpuOrder))
	gpuMemory := make([]chartSeries, 0, len(gpuOrder))
	for _, key := range gpuOrder {
		label := fmt.Sprintf("%s GPU %d", nodeNames[key.nodeID], key.index)
		gpuUtilization = append(gpuUtilization, chartSeries{Label: label, Points: utilByGPU[key]})
		gpuMemory = append(gpuMemory, chartSeries{Label: label, Points: memByGPU[key]})
	}
	sort.Slice(gpuUtilization, func(i, j int) bool { return gpuUtilization[i].Label < gpuUtilization[j].Label })
	sort.Slice(gpuMemory, func(i, j int) bool { return gpuMemory[i].Label < gpuMemory[j].Label })

	cpuByNode := make(map[string][]chartPoint)
	memNodeByNode := make(map[string][]chartPoint)
	var nodeOrder []string
	for i := len(recentNode) - 1; i >= 0; i-- {
		m := recentNode[i]
		if _, ok := cpuByNode[m.NodeID]; !ok {
			nodeOrder = append(nodeOrder, m.NodeID)
		}
		x := m.RecordedAt.Format("15:04:05")
		cpuByNode[m.NodeID] = append(cpuByNode[m.NodeID], chartPoint{X: x, Y: m.CPUUtilizationPct})
		memNodeByNode[m.NodeID] = append(memNodeByNode[m.NodeID], chartPoint{X: x, Y: percentOf(m.SystemMemoryUsedMB, m.SystemMemoryTotalMB)})
	}
	cpu := make([]chartSeries, 0, len(nodeOrder))
	systemMemory := make([]chartSeries, 0, len(nodeOrder))
	for _, nodeID := range nodeOrder {
		label := nodeNames[nodeID]
		cpu = append(cpu, chartSeries{Label: label, Points: cpuByNode[nodeID]})
		systemMemory = append(systemMemory, chartSeries{Label: label, Points: memNodeByNode[nodeID]})
	}
	sort.Slice(cpu, func(i, j int) bool { return cpu[i].Label < cpu[j].Label })
	sort.Slice(systemMemory, func(i, j int) bool { return systemMemory[i].Label < systemMemory[j].Label })

	return metricsChartData{GPUUtilization: gpuUtilization, GPUMemory: gpuMemory, CPU: cpu, SystemMemory: systemMemory}
}

// runningInfo is one node's currently-running model name and port, if any -
// built from a.instances/a.profiles, both already-wired API fields, no new
// constructor plumbing needed.
type runningInfo struct {
	model string
	port  string
}

func buildRunningByNode(instances []*db.RunningInstance, profiles []*db.Profile) map[string]runningInfo {
	profileNames := make(map[string]string, len(profiles))
	for _, p := range profiles {
		profileNames[p.ID] = p.Name
	}

	running := make(map[string]runningInfo)
	for _, inst := range instances {
		if inst.Status != db.RunningInstanceStatusRunning {
			continue
		}
		port := "-"
		if inst.ActualPort != nil {
			port = fmt.Sprintf("%d", *inst.ActualPort)
		}
		running[inst.PrimaryNodeID] = runningInfo{model: profileNames[inst.ProfileID], port: port}
	}
	return running
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	latestNode, err := a.metrics.ListLatestByNode(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list latest metrics by node: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	latestGPU, err := a.metrics.ListLatestGPUByNode(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list latest gpu metrics by node and gpu: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recentNode, err := a.metrics.ListRecent(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recentGPU, err := a.metrics.ListRecentGPU(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent gpu metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	instances, err := a.instances.ListInstances(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list instances for metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profiles, err := a.profiles.ListProfiles(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list profiles for metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}
	nodeMetrics := make(map[string]*db.Metric, len(latestNode))
	for _, m := range latestNode {
		nodeMetrics[m.NodeID] = m
	}
	runningByNode := buildRunningByNode(instances, profiles)

	rows := make([]metricsGPURow, 0, len(latestGPU))
	for _, g := range latestGPU {
		row := metricsGPURow{
			NodeName:          nodeNames[g.NodeID],
			GPUIndex:          g.GPUIndex,
			GPUUtilizationPct: fmt.Sprintf("%.1f%%", g.UtilizationPct),
			GPUMemory:         formatMetricMB(g.MemoryUsedMB, g.MemoryTotalMB),
			RunningModel:      "-",
			Port:              "-",
			RecordedAt:        g.RecordedAt.Format("2006-01-02 15:04:05 MST"),
		}
		if nm, ok := nodeMetrics[g.NodeID]; ok {
			row.CPUUtilizationPct = fmt.Sprintf("%.1f%%", nm.CPUUtilizationPct)
			row.SystemMemory = formatMetricMB(nm.SystemMemoryUsedMB, nm.SystemMemoryTotalMB)
		}
		if ri, ok := runningByNode[g.NodeID]; ok {
			row.RunningModel = ri.model
			row.Port = ri.port
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].NodeName != rows[j].NodeName {
			return rows[i].NodeName < rows[j].NodeName
		}
		return rows[i].GPUIndex < rows[j].GPUIndex
	})

	chartData := buildMetricsChartData(recentNode, recentGPU, nodeNames)

	// json.Marshal HTML-escapes angle brackets and ampersands by default
	// (documented behavior, not opted into here) - that's what makes it
	// safe to embed this directly inside a <script> block via
	// template.JS below without a separate sanitization step. NodeName is
	// Admin-authored free text (SCHEMA.md Nodes.name), not
	// attacker-controlled in the untrusted sense, but this holds
	// regardless of who set it.
	encoded, err := json.Marshal(chartData)
	if err != nil {
		a.logger.Printf("httpapi: encode metrics chart data: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "metrics", "Metrics", metricsPageData{Rows: rows, ChartDataJSON: template.JS(encoded)})
}

// handleMetricsChartData is the Metrics page's live-update fetch target -
// see web/static/js/metrics.js's sparkyMetricsLiveUpdate. A telemetry tick
// fetches this instead of triggering the full htmx page refetch every
// other SSE event still uses (web/static/js/sse.js), so the chart updates
// in place without visibly redrawing. Same Read-only/no-audit posture as
// /metrics itself - reads are never audited (ARCHITECTURE.md Audit Log).
func (a *API) handleMetricsChartData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	recentNode, err := a.metrics.ListRecent(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recentGPU, err := a.metrics.ListRecentGPU(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent gpu metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for metrics chart data: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}

	data := buildMetricsChartData(recentNode, recentGPU, nodeNames)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		a.logger.Printf("httpapi: encode metrics chart data response: %v", err)
	}
}
