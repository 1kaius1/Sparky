// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
)

// transferLister is the subset of *transfers.Service this package needs.
type transferLister interface {
	ListTransfers(ctx context.Context) ([]*db.ModelTransfer, error)
}

// transfersPageData is the Model transfers page's view model - CLAUDE.md
// Frontend Conventions' Model transfers sidebar tier ("Read-only view /
// Admin+grant initiate"). Labeled "Model transfers" in the sidebar/page
// title (not just "Transfers") since the Engine transfers page sits right
// below it. CanInitiate only decides whether the "New transfer" link is
// shown, same non-security-boundary reasoning as
// engineTransfersPageData.CanProvision - the real check happens inside
// transfers.Service.InitiateTransfer.
type transfersPageData struct {
	Transfers   []transferRow
	CanInitiate bool
}

type transferRow struct {
	ModelRef     string
	DestNode     string
	SourceType   string
	Status       string
	Progress     string
	RequestedAt  string
	ErrorMessage string
}

// formatMB renders a byte count as megabytes with one decimal place - the
// same "plain, minimal" formatting nodes.html already uses for
// GPUMemoryGB/CPUMemoryGB, not a general-purpose byte-formatting helper.
func formatMB(bytes int64) string {
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

func (a *API) handleTransfers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	transfers, err := a.transfers.ListTransfers(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list transfers: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for transfers: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Name
	}

	rows := make([]transferRow, 0, len(transfers))
	for _, t := range transfers {
		var errMsg string
		if t.ErrorMessage != nil {
			errMsg = *t.ErrorMessage
		}
		rows = append(rows, transferRow{
			ModelRef:     t.ModelRef,
			DestNode:     nodeNames[t.DestNodeID],
			SourceType:   string(t.SourceType),
			Status:       string(t.Status),
			Progress:     formatMB(t.BytesTransferred) + " / " + formatMB(t.BytesTotal),
			RequestedAt:  t.RequestedAt.Format("2006-01-02 15:04:05 MST"),
			ErrorMessage: errMsg,
		})
	}

	// CanInitiate only decides whether the "New transfer" link is shown -
	// it is not the security boundary. The real check happens inside
	// transfers.Service.InitiateTransfer, same reasoning as the Engine
	// transfers page's CanProvision.
	var canInitiate bool
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canInitiate, err = a.transferInitiatorSvc.CanInitiateTransfer(ctx, actor)
			if err != nil {
				a.logger.Printf("httpapi: check initiate-transfer permission for transfers list: %v", err)
				canInitiate = false
			}
		}
	}

	a.render(w, r, "transfers", "Model transfers", transfersPageData{Transfers: rows, CanInitiate: canInitiate})
}
