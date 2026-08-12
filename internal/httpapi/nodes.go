// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import "net/http"

// nodesPageData is the Nodes page's view model - CLAUDE.md Frontend
// Conventions' Nodes sidebar tier ("Read-only view"); the "Admin edit"
// half of that tier note is a later phase - no write form exists yet.
type nodesPageData struct {
	Nodes []nodeRow
}

type nodeRow struct {
	Name        string
	Hostname    string
	NodeType    string
	AgentStatus string
	GPUMemoryGB float64
	CPUMemoryGB float64
}

func (a *API) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.nodes.ListNodes(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, nodeRow{
			Name:        n.Name,
			Hostname:    n.Hostname,
			NodeType:    string(n.NodeType),
			AgentStatus: string(n.AgentStatus),
			GPUMemoryGB: n.GPUMemoryGB,
			CPUMemoryGB: n.CPUMemoryGB,
		})
	}

	a.render(w, r, "nodes", "Nodes", nodesPageData{Nodes: rows})
}
