// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/1kaius1/Sparky/internal/inventory"
)

// inventoryLister is the subset of *inventory.Service this package needs
// for the Inventory page's read-only Simple/Advanced views.
type inventoryLister interface {
	ListGrouped(ctx context.Context) ([]inventory.Group, error)
	ListGroupedSimple(ctx context.Context) ([]inventory.SimpleRow, error)
}

// inventoryPageData is the Inventory page's view model - see PLANNING.md's
// Models redesign decision 14. View is "simple" or "advanced"; only the
// matching slice below is populated, mirroring transfersPageData's own
// single-purpose-per-request shape.
type inventoryPageData struct {
	View           string
	AdvancedGroups []inventoryGroupRow
	SimpleRows     []inventorySimpleRow
}

// inventoryGroupRow is one (model_ref, quantization, format) combination's
// entries across every node - the Advanced view's row unit.
type inventoryGroupRow struct {
	ModelRef     string
	Quantization string
	Format       string
	Entries      []inventoryEntryRow
}

type inventoryEntryRow struct {
	NodeName string
	Status   string
	Size     string
	PlacedAt string
}

// inventorySimpleRow is one model_ref's totals across every quantization/
// format/node it exists in - the Simple view's row unit.
type inventorySimpleRow struct {
	ModelRef      string
	Quantizations string
	Size          string
}

func (a *API) handleInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	view := r.URL.Query().Get("view")
	if view != "simple" {
		view = "advanced"
	}

	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for inventory: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodeNames := make(map[string]string, len(nodeList))
	for _, n := range nodeList {
		nodeNames[n.ID] = n.Name
	}

	data := inventoryPageData{View: view}

	if view == "simple" {
		rows, err := a.inventory.ListGroupedSimple(ctx)
		if err != nil {
			a.logger.Printf("httpapi: list grouped-simple inventory: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data.SimpleRows = make([]inventorySimpleRow, 0, len(rows))
		for _, row := range rows {
			data.SimpleRows = append(data.SimpleRows, inventorySimpleRow{
				ModelRef:      row.ModelRef,
				Quantizations: strings.Join(row.Quantizations, ", "),
				Size:          formatMB(row.TotalSizeBytes),
			})
		}
	} else {
		groups, err := a.inventory.ListGrouped(ctx)
		if err != nil {
			a.logger.Printf("httpapi: list grouped inventory: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data.AdvancedGroups = make([]inventoryGroupRow, 0, len(groups))
		for _, g := range groups {
			entries := make([]inventoryEntryRow, 0, len(g.Entries))
			for _, e := range g.Entries {
				entries = append(entries, inventoryEntryRow{
					NodeName: nodeNames[e.NodeID],
					Status:   string(e.Status),
					Size:     formatMB(e.SizeBytes),
					PlacedAt: e.PlacedAt.Format("2006-01-02 15:04:05 MST"),
				})
			}
			data.AdvancedGroups = append(data.AdvancedGroups, inventoryGroupRow{
				ModelRef:     g.ModelRef,
				Quantization: g.Quantization,
				Format:       string(g.Format),
				Entries:      entries,
			})
		}
	}

	a.render(w, r, "inventory", "Inventory", data)
}
