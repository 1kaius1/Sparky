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

// transfersPageData is the Transfers page's view model - CLAUDE.md Frontend
// Conventions' Transfers sidebar tier ("Read-only view / Admin+grant
// initiate"); the "Admin+grant initiate" half is a later phase - no
// initiate form exists yet.
type transfersPageData struct {
	Transfers []transferRow
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

	a.render(w, r, "transfers", "Transfers", transfersPageData{Transfers: rows})
}
