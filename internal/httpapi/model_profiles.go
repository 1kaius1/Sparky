// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import "net/http"

// profilesPageData is the Model profiles page's view model - CLAUDE.md
// Frontend Conventions' Model profiles sidebar tier ("Read-only view");
// the "Developer launch"/"PowerDev create" halves of that tier note are
// later phases - no launch/create form exists yet.
type profilesPageData struct {
	Profiles []profileRow
}

type profileRow struct {
	Name       string
	ModelRef   string
	EngineType string
	TargetNode string
	Port       int
}

func (a *API) handleModelProfiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	profiles, err := a.profiles.ListProfiles(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list model profiles: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for model profiles: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}

	rows := make([]profileRow, 0, len(profiles))
	for _, p := range profiles {
		var targetNode string
		if p.TargetNodeID != nil {
			targetNode = nodeNames[*p.TargetNodeID]
		}
		rows = append(rows, profileRow{
			Name:       p.Name,
			ModelRef:   p.ModelRef,
			EngineType: string(p.EngineType),
			TargetNode: targetNode,
			Port:       p.Port,
		})
	}

	a.render(w, r, "profiles", "Model profiles", profilesPageData{Profiles: rows})
}
