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
}

// metricsPageData is the Metrics page's view model.
type metricsPageData struct {
	Nodes           []metricsNodeRow
	ChartSeriesJSON template.JS
}

type metricsNodeRow struct {
	NodeName          string
	GPUUtilizationPct string
	GPUMemory         string
	CPUUtilizationPct string
	SystemMemory      string
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
	NodeName string       `json:"nodeName"`
	Points   []chartPoint `json:"points"`
}

func formatMetricMB(used, total float64) string {
	return fmt.Sprintf("%.0f MB / %.0f MB", used, total)
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	latest, err := a.metrics.ListLatestByNode(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list latest metrics by node: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recent, err := a.metrics.ListRecent(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list recent metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}

	rows := make([]metricsNodeRow, 0, len(latest))
	for _, m := range latest {
		rows = append(rows, metricsNodeRow{
			NodeName:          nodeNames[m.NodeID],
			GPUUtilizationPct: fmt.Sprintf("%.1f%%", m.GPUUtilizationPct),
			GPUMemory:         formatMetricMB(m.GPUMemoryUsedMB, m.GPUMemoryTotalMB),
			CPUUtilizationPct: fmt.Sprintf("%.1f%%", m.CPUUtilizationPct),
			SystemMemory:      formatMetricMB(m.SystemMemoryUsedMB, m.SystemMemoryTotalMB),
			RecordedAt:        m.RecordedAt.Format("2006-01-02 15:04:05 MST"),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].NodeName < rows[j].NodeName })

	// recent is most-recently-recorded first (db.MetricsRepository.Recent's
	// own ordering) - reversed here so each series renders chronologically
	// left-to-right on the chart.
	seriesByNode := make(map[string][]chartPoint)
	var nodeOrder []string
	for i := len(recent) - 1; i >= 0; i-- {
		m := recent[i]
		if _, ok := seriesByNode[m.NodeID]; !ok {
			nodeOrder = append(nodeOrder, m.NodeID)
		}
		seriesByNode[m.NodeID] = append(seriesByNode[m.NodeID], chartPoint{
			X: m.RecordedAt.Format("15:04:05"),
			Y: m.GPUUtilizationPct,
		})
	}
	series := make([]chartSeries, 0, len(nodeOrder))
	for _, nodeID := range nodeOrder {
		series = append(series, chartSeries{NodeName: nodeNames[nodeID], Points: seriesByNode[nodeID]})
	}
	sort.Slice(series, func(i, j int) bool { return series[i].NodeName < series[j].NodeName })

	// json.Marshal HTML-escapes angle brackets and ampersands by default
	// (documented behavior, not opted into here) - that's what makes it
	// safe to embed this directly inside a <script> block via
	// template.JS below without a separate sanitization step. NodeName is
	// Admin-authored free text (SCHEMA.md Nodes.name), not
	// attacker-controlled in the untrusted sense, but this holds
	// regardless of who set it.
	encoded, err := json.Marshal(series)
	if err != nil {
		a.logger.Printf("httpapi: encode metrics chart series: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "metrics", "Metrics", metricsPageData{Nodes: rows, ChartSeriesJSON: template.JS(encoded)})
}
