// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/inventory"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// maxScanFormBytes bounds the scan and import POST bodies. An import form
// carries one checkbox and one text field per scanned model (at most
// 500 per node), well inside this.
const maxScanFormBytes = 1 << 20

// maxImportItems bounds how many selected models one import request
// accepts, a backstop independent of the body limit. A request over it is
// refused outright, never truncated.
const maxImportItems = 1000

// scanPageData is the scan page's view model: the nodes to pick from.
type scanPageData struct {
	Nodes []scanNodeOption
}

type scanNodeOption struct {
	ID     string
	Name   string
	Online bool
}

// scanResultsData is the scan_results partial's view model. PollURL is set
// only while some node is still pending, which is what keeps the partial
// polling itself; once the scan is complete the partial is static and
// carries the import form.
type scanResultsData struct {
	// Error is a whole-request problem (expired scan, nothing selected).
	Error    string
	ScanID   string
	PollURL  string
	Nodes    []scanResultNode
	Failures []string
}

type scanResultNode struct {
	Name        string
	Status      string
	StatusClass string
	Error       string
	Known       int
	Truncated   bool
	Rows        []scanResultRow
	// HasSelectable is true when at least one row can be ticked (is not
	// blocked); the table's Select all control is only offered then.
	HasSelectable bool
}

type scanResultRow struct {
	// Index makes the sel_N / aq_N form field names unique across nodes.
	Index int
	// Value identifies the scanned model for the import handler (node, ref,
	// quantization, format) - the handler re-derives everything else from
	// the stored scan.
	Value        string
	ModelRef     string
	Quantization string
	Format       string
	Size         string
	FileName     string
	Incomplete   bool
	Removed      bool
	Blocked      string
	// CanEditQuant is true for gguf, whose quantization label is a guess
	// from the file name an Admin may correct.
	CanEditQuant bool
}

func encodeScanEntry(nodeID, modelRef, quantization string, format db.ModelFormat) string {
	return url.Values{"node": {nodeID}, "ref": {modelRef}, "q": {quantization}, "f": {string(format)}}.Encode()
}

func decodeScanEntry(s string) (nodeID, modelRef, quantization string, format db.ModelFormat, ok bool) {
	v, err := url.ParseQuery(s)
	if err != nil {
		return "", "", "", "", false
	}
	f := db.ModelFormat(v.Get("f"))
	if v.Get("node") == "" || v.Get("ref") == "" || (f != db.ModelFormatSafetensors && f != db.ModelFormatGGUF) {
		return "", "", "", "", false
	}
	return v.Get("node"), v.Get("ref"), v.Get("q"), f, true
}

// requireImporter resolves the actor and enforces inventory.Service's
// import gate, writing the error response itself. The service repeats the
// check on every real operation - this keeps non-Admins off the page and
// its helper endpoints.
func (a *API) requireImporter(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for model scan: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	if !a.inventory.CanImport(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin required")
		return rbac.Actor{}, false
	}
	return actor, true
}

// handleScanModelsPage is GET /inventory/scan - the node picker.
func (a *API) handleScanModelsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireImporter(w, r); !ok {
		return
	}
	nodeList, err := a.nodes.ListNodes(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes for model scan: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := scanPageData{Nodes: make([]scanNodeOption, 0, len(nodeList))}
	for _, n := range nodeList {
		data.Nodes = append(data.Nodes, scanNodeOption{ID: n.ID, Name: n.Name, Online: n.AgentStatus == db.AgentStatusOnline})
	}
	a.render(w, r, "scan_models", "Scan for unknown models", data)
}

// handleStartScan is POST /inventory/scan. Expected failures are rendered
// into the results partial with a 200, since htmx does not swap 4xx bodies.
func (a *API) handleStartScan(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireImporter(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}
	scanID, err := a.inventory.StartScan(r.Context(), actor, r.PostForm["node_id"])
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin required")
		return
	case errors.Is(err, inventory.ErrNoScanNodes), errors.Is(err, inventory.ErrInvalidScanNode), errors.Is(err, inventory.ErrTooManyScans):
		a.renderPartial(w, "scan_results", scanResultsData{Error: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: start model scan: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.renderScan(w, r, actor, scanID, nil)
}

// handleScanResult is GET /inventory/scan/{id} - polled by the results
// partial until every node has answered.
func (a *API) handleScanResult(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireImporter(w, r)
	if !ok {
		return
	}
	a.renderScan(w, r, actor, chi.URLParam(r, "id"), nil)
}

// handleImportModels is POST /inventory/import. Only the scan id and which
// rows were ticked (plus an optional quantization correction) come from the
// form; inventory.Service.Import re-derives everything else from the
// stored scan. Full success redirects to the Inventory page; anything else
// re-renders the results with what went wrong.
func (a *API) handleImportModels(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireImporter(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}
	scanID := r.PostForm.Get("scan_id")

	var indexes []int
	for key := range r.PostForm {
		suffix, found := strings.CutPrefix(key, "sel_")
		if !found {
			continue
		}
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 0 {
			continue
		}
		indexes = append(indexes, n)
	}
	sort.Ints(indexes)
	// Refuse rather than quietly import only the first maxImportItems: with
	// the Select all control a user can legitimately tick more than that, and
	// would otherwise believe every one of them was imported.
	if len(indexes) > maxImportItems {
		a.renderScan(w, r, actor, scanID, []string{fmt.Sprintf("%d models are selected; import at most %d at a time. Nothing was imported.", len(indexes), maxImportItems)})
		return
	}
	items := make([]inventory.ImportItem, 0, len(indexes))
	for _, n := range indexes {
		nodeID, ref, quant, format, ok := decodeScanEntry(r.PostForm.Get("sel_" + strconv.Itoa(n)))
		if !ok {
			continue
		}
		items = append(items, inventory.ImportItem{
			NodeID: nodeID, ModelRef: ref, Quantization: quant, Format: format,
			AsQuantization: strings.TrimSpace(r.PostForm.Get("aq_" + strconv.Itoa(n))),
		})
	}
	if len(items) == 0 {
		a.renderScan(w, r, actor, scanID, []string{"Select at least one model to import."})
		return
	}

	_, failures, err := a.inventory.Import(r.Context(), actor, scanID, items)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin required")
		return
	case errors.Is(err, inventory.ErrScanNotFound):
		a.renderPartial(w, "scan_results", scanResultsData{Error: "This scan has expired - run a new scan."})
		return
	case err != nil:
		a.logger.Printf("httpapi: import models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if len(failures) == 0 {
		w.Header().Set("HX-Redirect", "/inventory")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	names := a.nodeNameMap(r.Context())
	messages := make([]string, 0, len(failures))
	for _, f := range failures {
		messages = append(messages, fmt.Sprintf("%s on %s: %s", f.Item.ModelRef, nodeLabel(names, f.Item.NodeID), a.importFailureText(f.Err)))
	}
	a.renderScan(w, r, actor, scanID, messages)
}

// importFailureText gives the operator a reason for the failures they can
// act on, and a generic line (with the detail logged) for the rest.
func (a *API) importFailureText(err error) string {
	for _, known := range []error{
		inventory.ErrNotInScan, inventory.ErrAlreadyKnown, inventory.ErrInvalidQuantization,
		inventory.ErrSharedDirectory, inventory.ErrAmbiguousQuantization,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	a.logger.Printf("httpapi: import model: %v", err)
	return "could not be imported (see server log)"
}

func (a *API) nodeNameMap(ctx context.Context) map[string]string {
	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for model scan: %v", err)
		return nil
	}
	names := make(map[string]string, len(nodeList))
	for _, n := range nodeList {
		names[n.ID] = n.Name
	}
	return names
}

func nodeLabel(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

// renderScan renders the scan_results partial for a scan id.
func (a *API) renderScan(w http.ResponseWriter, r *http.Request, actor rbac.Actor, scanID string, failures []string) {
	view, err := a.inventory.ScanResult(actor, scanID)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin required")
		return
	case errors.Is(err, inventory.ErrScanNotFound):
		a.renderPartial(w, "scan_results", scanResultsData{Error: "This scan has expired - run a new scan."})
		return
	case err != nil:
		a.logger.Printf("httpapi: read model scan: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	names := a.nodeNameMap(r.Context())
	data := scanResultsData{ScanID: view.ID, Failures: failures}
	if !view.Complete {
		data.PollURL = "/inventory/scan/" + view.ID
	}
	index := 0
	for _, n := range view.Nodes {
		node := scanResultNode{
			Name: nodeLabel(names, n.NodeID), Status: n.Status, StatusClass: scanStatusClass(n.Status),
			Error: n.Error, Known: n.KnownCount, Truncated: n.Truncated,
		}
		for _, c := range n.Candidates {
			node.Rows = append(node.Rows, scanResultRow{
				Index:        index,
				Value:        encodeScanEntry(n.NodeID, c.ModelRef, c.Quantization, c.Format),
				ModelRef:     c.ModelRef,
				Quantization: c.Quantization,
				Format:       string(c.Format),
				Size:         formatBytes(c.SizeBytes),
				FileName:     c.FileName,
				Incomplete:   c.PossiblyIncomplete,
				Removed:      c.PreviouslyRemoved,
				Blocked:      c.BlockedReason,
				CanEditQuant: c.Format == db.ModelFormatGGUF,
			})
			if c.BlockedReason == "" {
				node.HasSelectable = true
			}
			index++
		}
		data.Nodes = append(data.Nodes, node)
	}
	a.renderPartial(w, "scan_results", data)
}

// scanStatusClass maps a scan state onto the existing status colour classes.
func scanStatusClass(status string) string {
	switch status {
	case inventory.ScanDone:
		return "completed"
	case inventory.ScanOffline:
		return "offline"
	case inventory.ScanFailed:
		return "failed"
	default:
		return "starting"
	}
}
