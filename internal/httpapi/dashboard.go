// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"

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
	TotalNodes      int
	OnlineNodes     int
	TotalInstances  int
	RunningCount    int
	RecentInstances []dashboardInstanceRow
}

type dashboardInstanceRow struct {
	Status    db.RunningInstanceStatus
	NodeName  string
	StartedAt string
}

// dashboardRecentInstanceLimit caps how many rows the overview table
// shows - a full, filterable fleet-wide history browser belongs to a
// later phase, not this summary page.
const dashboardRecentInstanceLimit = 10

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

	nodeNames := make(map[string]string, len(nodes))
	onlineNodes := 0
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
		if n.AgentStatus == db.AgentStatusOnline {
			onlineNodes++
		}
	}

	runningCount := 0
	rows := make([]dashboardInstanceRow, 0, min(len(instances), dashboardRecentInstanceLimit))
	for _, inst := range instances {
		if inst.Status == db.RunningInstanceStatusRunning {
			runningCount++
		}
		if len(rows) < dashboardRecentInstanceLimit {
			rows = append(rows, dashboardInstanceRow{
				Status:    inst.Status,
				NodeName:  nodeNames[inst.PrimaryNodeID],
				StartedAt: inst.StartedAt.Format("2006-01-02 15:04:05 MST"),
			})
		}
	}

	a.render(w, r, "dashboard", "Dashboard", dashboardData{
		TotalNodes:      len(nodes),
		OnlineNodes:     onlineNodes,
		TotalInstances:  len(instances),
		RunningCount:    runningCount,
		RecentInstances: rows,
	})
}
