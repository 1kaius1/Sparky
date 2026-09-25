// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/inventory"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// inventoryLister is the subset of *inventory.Service this package needs
// for the Inventory page's read-only Simple/Advanced views.
type inventoryLister interface {
	ListGrouped(ctx context.Context) ([]inventory.Group, error)
	ListGroupedSimple(ctx context.Context) ([]inventory.SimpleRow, error)
	ListByNode(ctx context.Context, nodeID string) ([]*db.NodeModelInventory, error)
	CanDelete(ctx context.Context, actor rbac.Actor) (bool, error)
	Delete(ctx context.Context, actor rbac.Actor, nodeID, modelRef, quantization string, format db.ModelFormat) error
}

// inventoryPageData is the Inventory page's view model - see PLANNING.md's
// Models redesign decision 14. View is "simple" or "advanced"; only the
// matching slice below is populated, mirroring transfersPageData's own
// single-purpose-per-request shape.
type inventoryPageData struct {
	View string
	// CanDelete only decides whether the Delete action is shown, not a
	// security boundary - inventory.Service.Delete re-checks.
	CanDelete bool
	// CanTransfer only decides whether the Download / Replicate links are
	// shown - the real gate is the transfer form's own capability check.
	CanTransfer    bool
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
	// ReplicateURL opens the transfer form with this entry as a peer
	// source - this row's node is the source, this group the model.
	ReplicateURL string
	NodeID       string
	NodeName     string
	Status       string
	Size         string
	PlacedAt     string
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
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canDelete, err := a.inventory.CanDelete(ctx, actor)
			if err != nil {
				a.logger.Printf("httpapi: check delete-model permission for inventory: %v", err)
			}
			data.CanDelete = canDelete
			canTransfer, err := a.transferInitiatorSvc.CanInitiateTransfer(ctx, actor)
			if err != nil {
				a.logger.Printf("httpapi: check initiate-transfer permission for inventory: %v", err)
			}
			data.CanTransfer = canTransfer
		}
	}

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
					ReplicateURL: "/inventory/transfer/new?" + url.Values{"source_node_id": {e.NodeID}, "entry": {encodeEntry(g.ModelRef, g.Quantization, g.Format)}}.Encode(),
					NodeID:       e.NodeID,
					NodeName:     nodeNames[e.NodeID],
					Status:       string(e.Status),
					Size:         formatMB(e.SizeBytes),
					PlacedAt:     e.PlacedAt.Format("2006-01-02 15:04:05 MST"),
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

// handleDeleteInventoryEntry is POST /inventory/delete - asks the node
// holding a model copy to remove it. Fields are form values rather than
// path segments since model_ref contains slashes. The RBAC decision lives
// in inventory.Service.Delete, same as every other write path; same
// hx-post/HX-Redirect shape as handleUnloadInstance. The removal itself is
// asynchronous - the entry disappears once the agent confirms.
func (a *API) handleDeleteInventoryEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for delete inventory entry: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}
	nodeID := r.PostForm.Get("node_id")
	modelRef := r.PostForm.Get("model_ref")
	format := db.ModelFormat(r.PostForm.Get("format"))
	if nodeID == "" || modelRef == "" || (format != db.ModelFormatSafetensors && format != db.ModelFormatGGUF) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "node_id, model_ref, and a valid format are required")
		return
	}

	err = a.inventory.Delete(ctx, actor, nodeID, modelRef, r.PostForm.Get("quantization"), format)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "model store management required")
		return
	case errors.Is(err, db.ErrNodeModelInventoryNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "inventory entry not found")
		return
	case errors.Is(err, inventory.ErrModelInUse):
		writeError(w, r, http.StatusConflict, "MODEL_IN_USE", err.Error())
		return
	case errors.Is(err, inventory.ErrNodeOffline):
		writeError(w, r, http.StatusConflict, "NODE_OFFLINE", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: delete inventory entry %s on node %s: %v", modelRef, nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/inventory")
	w.WriteHeader(http.StatusNoContent)
}
