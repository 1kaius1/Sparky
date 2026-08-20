// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
)

// engineInventoryLister is the subset of *engineprovision.Service this
// package needs for the Engine inventory page's read-only list.
type engineInventoryLister interface {
	ListNodeEngineInventory(ctx context.Context) ([]*db.NodeEngineInventory, error)
}

// engineInventoryPageData is the Engine inventory page's view model - see
// SCHEMA.md Node engine inventory. Distinct from engineTransfersPageData:
// this answers "what's actually installed right now," not "what
// provisioning runs have happened."
type engineInventoryPageData struct {
	Entries []engineInventoryRow
}

type engineInventoryRow struct {
	NodeName    string
	EngineType  string
	Version     string
	Status      string
	InstallPath string
	Size        string
	PlacedAt    string
}

func (a *API) handleEngineInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	entries, err := a.engineInventory.ListNodeEngineInventory(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list node engine inventory: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for engine inventory: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodeList))
	for _, n := range nodeList {
		nodeNames[n.ID] = n.Name
	}

	rows := make([]engineInventoryRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, engineInventoryRow{
			NodeName:    nodeNames[e.NodeID],
			EngineType:  string(e.EngineType),
			Version:     e.Version,
			Status:      string(e.Status),
			InstallPath: e.InstallPath,
			Size:        formatMB(e.SizeBytes),
			PlacedAt:    e.PlacedAt.Format("2006-01-02 15:04:05 MST"),
		})
	}

	a.render(w, r, "engine_inventory", "Engine inventory", engineInventoryPageData{Entries: rows})
}
