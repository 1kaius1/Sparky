// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/modelsource"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/transfers"
)

// transferInitiator is the subset of *transfers.Service this package needs
// for the transfer initiation form - separate from transferLister (the
// Model transfers page's read-only list) the same way engineProvisioner is
// kept separate from engineTransferLister, even though both are backed by
// the same concrete service in cmd/sparky-server/main.go.
type transferInitiator interface {
	CanInitiateTransfer(ctx context.Context, actor rbac.Actor) (bool, error)
	InitiateTransfer(ctx context.Context, actor rbac.Actor, params transfers.InitiateTransferParams) (*db.ModelTransfer, error)
	RetryTransfer(ctx context.Context, actor rbac.Actor, transferID string) (*db.ModelTransfer, error)
	CancelTransfer(ctx context.Context, actor rbac.Actor, transferID string) error
	CheckConnectivity(ctx context.Context, actor rbac.Actor, destNodeID, sourceNodeID, sourceInterface string) (string, error)
	GetConnectivityResult(checkID string) (*transfers.ConnectivityResult, bool)
}

// sizeEstimator is the subset of *modelsource.Estimator the form's size
// preview needs.
type sizeEstimator interface {
	EstimateSize(ctx context.Context, modelRef, quantization string) (int64, error)
}

const (
	sourceInternet = "internet"
	sourcePeer     = "peer_node"

	// maxCheckPolls bounds how long the "Check Destination" partial keeps
	// polling for the agent's answer (one poll per second).
	maxCheckPolls = 12
)

// initiateTransferPageData is the transfer form's view model - Error is
// non-empty, and Form carries back what was submitted, when redisplaying
// the form after a validation failure, same pattern as
// provisionEnginePageData.
type initiateTransferPageData struct {
	Error string
	Form  initiateTransferFormValues
	Nodes []nodeOption
}

type initiateTransferFormValues struct {
	SourceType   string
	DestNodeID   string
	ModelRef     string
	Quantization string

	// Peer-only fields. Entry is a source inventory entry encoded by
	// encodeEntry - one <select> value carrying (model_ref, quantization,
	// format), since model_ref and quantization are free text and cannot be
	// packed into a delimiter-joined string safely.
	SourceNodeID    string
	Entry           string
	SourceInterface string
}

// encodeEntry / decodeEntry pack and unpack an inventory entry identity
// into a single form value.
func encodeEntry(modelRef, quantization string, format db.ModelFormat) string {
	return url.Values{"ref": {modelRef}, "q": {quantization}, "f": {string(format)}}.Encode()
}

func decodeEntry(s string) (modelRef, quantization string, format db.ModelFormat, ok bool) {
	v, err := url.ParseQuery(s)
	if err != nil {
		return "", "", "", false
	}
	f := db.ModelFormat(v.Get("f"))
	if v.Get("ref") == "" || (f != db.ModelFormatSafetensors && f != db.ModelFormatGGUF) {
		return "", "", "", false
	}
	return v.Get("ref"), v.Get("q"), f, true
}

// requireInitiator resolves the actor and enforces the manage_model_store
// capability, writing the error response itself. The check is the same one
// the service repeats on the real write - this only keeps unauthorized
// viewers off the form and its helper endpoints.
func (a *API) requireInitiator(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	permitted, err := a.transferInitiatorSvc.CanInitiateTransfer(ctx, actor)
	if err != nil {
		a.logger.Printf("httpapi: check initiate-transfer permission: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	if !permitted {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return rbac.Actor{}, false
	}
	return actor, true
}

// handleInitiateTransferForm is GET /inventory/transfer/new. Query
// parameters prefill it - the Inventory page's per-row Replicate link
// passes source_node_id and entry, which selects the peer-to-peer mode.
func (a *API) handleInitiateTransferForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInitiator(w, r); !ok {
		return
	}
	options, err := a.nodeOptionsForProfileForm(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes for transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	form := initiateTransferFormValues{
		SourceType:      sourceInternet,
		DestNodeID:      q.Get("dest_node_id"),
		SourceNodeID:    q.Get("source_node_id"),
		Entry:           q.Get("entry"),
		SourceInterface: q.Get("source_interface"),
	}
	if form.SourceNodeID != "" || form.Entry != "" {
		form.SourceType = sourcePeer
	}
	a.render(w, r, "initiate_transfer", "New model transfer", initiateTransferPageData{Form: form, Nodes: options})
}

// handleInitiateTransfer is POST /inventory/transfer/new. The RBAC decision
// lives in transfers.Service.InitiateTransfer; the Check Destination gate
// on the peer form is a client-side convenience, not enforced here - it
// proves only that a TCP path was open at check time, and the real
// authenticated pull is what decides success.
func (a *API) handleInitiateTransfer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderInitiateTransferError(w, r, "invalid form submission", initiateTransferFormValues{SourceType: sourceInternet})
		return
	}
	form := initiateTransferFormValues{
		SourceType:      r.PostFormValue("source_type"),
		DestNodeID:      r.PostFormValue("dest_node_id"),
		ModelRef:        r.PostFormValue("model_ref"),
		Quantization:    r.PostFormValue("quantization"),
		SourceNodeID:    r.PostFormValue("source_node_id"),
		Entry:           r.PostFormValue("entry"),
		SourceInterface: r.PostFormValue("source_interface"),
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for initiate transfer: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var params transfers.InitiateTransferParams
	switch form.SourceType {
	case sourcePeer:
		ref, quant, format, ok := decodeEntry(form.Entry)
		if !ok {
			a.renderInitiateTransferError(w, r, "choose a model on the source node", form)
			return
		}
		params = transfers.InitiateTransferParams{
			DestNodeID: form.DestNodeID, ModelRef: ref, Quantization: quant,
			SourceType: db.TransferSourcePeerNode, SourceNodeID: form.SourceNodeID,
			Format: format, SourceInterface: form.SourceInterface,
		}
	case sourceInternet, "":
		form.SourceType = sourceInternet
		params = transfers.InitiateTransferParams{DestNodeID: form.DestNodeID, ModelRef: form.ModelRef, Quantization: form.Quantization}
	default:
		a.renderInitiateTransferError(w, r, "unknown source type", initiateTransferFormValues{SourceType: sourceInternet})
		return
	}

	_, err = a.transferInitiatorSvc.InitiateTransfer(ctx, actor, params)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return
	case errors.Is(err, transfers.ErrInvalidTransfer), errors.Is(err, transfers.ErrDestNodeOffline),
		errors.Is(err, transfers.ErrSourceNodeOffline), errors.Is(err, transfers.ErrSourceNotPresent),
		errors.Is(err, transfers.ErrPeerNotReady):
		a.renderInitiateTransferError(w, r, err.Error(), form)
		return
	case err != nil:
		a.logger.Printf("httpapi: initiate transfer: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/transfers", http.StatusSeeOther)
}

func (a *API) renderInitiateTransferError(w http.ResponseWriter, r *http.Request, errMsg string, form initiateTransferFormValues) {
	options, err := a.nodeOptionsForProfileForm(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes for transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "initiate_transfer", "New model transfer", initiateTransferPageData{Error: errMsg, Form: form, Nodes: options})
}

// estimateSizeData is the size-preview partial's view model.
type estimateSizeData struct{ Text string }

// formatBytes renders a byte count in the largest sensible unit.
func formatBytes(b int64) string {
	const gb = 1024 * 1024 * 1024
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	default:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	}
}

// handleEstimateSize is GET /inventory/transfer/estimate-size - the form's
// "Estimated size" preview. Best-effort: every failure degrades to a quiet
// "size unknown" and never blocks the transfer. Behind the same capability
// as the form, since it makes an outbound request to a third party.
func (a *API) handleEstimateSize(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInitiator(w, r); !ok {
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("model_ref"))
	if ref == "" {
		a.renderPartial(w, "estimate_size", estimateSizeData{})
		return
	}
	size, err := a.sizeEstimator.EstimateSize(r.Context(), ref, r.URL.Query().Get("quantization"))
	switch {
	case err == nil:
		a.renderPartial(w, "estimate_size", estimateSizeData{Text: "Estimated size: ~" + formatBytes(size)})
	case errors.Is(err, modelsource.ErrUnsupported):
		a.renderPartial(w, "estimate_size", estimateSizeData{Text: "Size estimate not available for this model reference."})
	default:
		a.renderPartial(w, "estimate_size", estimateSizeData{Text: "Size unknown."})
	}
}

// peerOptionsData is the peer fieldset's dynamic part: the source node's
// present inventory entries and reported interfaces.
type peerOptionsData struct {
	NoSource        bool
	SourceNodeID    string
	Entries         []peerEntryOption
	Interfaces      []peerInterfaceOption
	DefaultLabel    string
	SelectedIface   string
	CanRescan       bool
	NoEntriesNotice bool
}

type peerEntryOption struct {
	Value    string
	Label    string
	Selected bool
}

type peerInterfaceOption struct {
	Name     string
	Label    string
	Selected bool
}

// handlePeerOptions is GET /inventory/transfer/peer-options - rendered into
// the form whenever the source node changes (or a Rescan finishes). entry
// and source_interface carry the user's current choices so a refresh keeps
// them; prefill_* are the values the page was opened with, used until the
// user has chosen something themselves.
func (a *API) handlePeerOptions(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireInitiator(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	sourceID := q.Get("source_node_id")
	if sourceID == "" {
		a.renderPartial(w, "peer_options", peerOptionsData{NoSource: true})
		return
	}
	entry := q.Get("entry")
	if entry == "" {
		entry = q.Get("prefill_entry")
	}
	iface := q.Get("source_interface")
	if _, chosen := q["source_interface"]; !chosen {
		iface = q.Get("prefill_source_interface")
	}

	node, err := a.registrar.GetNode(ctx, sourceID)
	if errors.Is(err, db.ErrNodeNotFound) {
		a.renderPartial(w, "peer_options", peerOptionsData{NoSource: true})
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: look up source node for peer options: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entries, err := a.inventory.ListByNode(ctx, sourceID)
	if err != nil {
		a.logger.Printf("httpapi: list source inventory for peer options: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ifaces, err := a.registrar.ListInterfaces(ctx, sourceID)
	if err != nil {
		a.logger.Printf("httpapi: list source interfaces for peer options: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := peerOptionsData{SourceNodeID: sourceID, SelectedIface: iface, CanRescan: rbac.CanManageNodes(actor)}
	for _, e := range entries {
		if e.Status != db.InventoryStatusPresent {
			continue
		}
		value := encodeEntry(e.ModelRef, e.Quantization, e.Format)
		label := fmt.Sprintf("%s - %s (%s), %s", e.ModelRef, e.Quantization, e.Format, formatMB(e.SizeBytes))
		data.Entries = append(data.Entries, peerEntryOption{Value: value, Label: label, Selected: value == entry})
	}
	data.NoEntriesNotice = len(data.Entries) == 0

	data.DefaultLabel = "Fastest (highest reported link speed)"
	if node.DefaultTransferInterface != nil {
		data.DefaultLabel = "Node default (" + *node.DefaultTransferInterface + ")"
	}
	for _, i := range ifaces {
		speed := "speed unknown"
		if i.LinkSpeedMbps != nil {
			speed = strconv.Itoa(*i.LinkSpeedMbps) + " Mbps"
		}
		data.Interfaces = append(data.Interfaces, peerInterfaceOption{
			Name: i.InterfaceName, Label: fmt.Sprintf("%s (%s, %s)", i.InterfaceName, i.IPAddress, speed), Selected: i.InterfaceName == iface,
		})
	}
	a.renderPartial(w, "peer_options", data)
}

// connectivityData is the Check Destination status partial's view model.
// State is "pending" (keep polling), "passed", "failed", or "error"; the
// form's script reads it from data-check-state to enable the submit
// control.
type connectivityData struct {
	State     string
	Message   string
	PollURL   string
	LatencyMs int64
}

// handleCheckConnectivity is POST /inventory/transfer/check-connectivity.
// Expected failures (offline node, not-ready node) are rendered as a status
// partial with HTTP 200, since htmx does not swap 4xx responses and the
// operator needs to see why.
func (a *API) handleCheckConnectivity(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireInitiator(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderPartial(w, "connectivity", connectivityData{State: "error", Message: "invalid request"})
		return
	}
	id, err := a.transferInitiatorSvc.CheckConnectivity(r.Context(), actor, r.PostFormValue("dest_node_id"), r.PostFormValue("source_node_id"), r.PostFormValue("source_interface"))
	switch {
	case err == nil:
		a.renderPartial(w, "connectivity", connectivityData{State: "pending", Message: "Checking...", PollURL: checkPollURL(id, 1)})
	case errors.Is(err, transfers.ErrInvalidTransfer), errors.Is(err, transfers.ErrDestNodeOffline), errors.Is(err, transfers.ErrPeerNotReady):
		a.renderPartial(w, "connectivity", connectivityData{State: "error", Message: err.Error()})
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
	default:
		a.logger.Printf("httpapi: check connectivity: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func checkPollURL(id string, n int) string {
	return "/inventory/transfer/check-result?id=" + url.QueryEscape(id) + "&n=" + strconv.Itoa(n)
}

// handleCheckResult is GET /inventory/transfer/check-result - polled by the
// pending status partial until the destination agent has answered.
func (a *API) handleCheckResult(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireInitiator(w, r); !ok {
		return
	}
	id := r.URL.Query().Get("id")
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	res, known := a.transferInitiatorSvc.GetConnectivityResult(id)
	switch {
	case !known:
		a.renderPartial(w, "connectivity", connectivityData{State: "error", Message: "The check expired - run it again."})
	case res == nil && n >= maxCheckPolls:
		a.renderPartial(w, "connectivity", connectivityData{State: "failed", Message: "No answer from the destination node - it may be busy or offline."})
	case res == nil:
		a.renderPartial(w, "connectivity", connectivityData{State: "pending", Message: "Checking...", PollURL: checkPollURL(id, n+1)})
	case res.Reachable:
		a.renderPartial(w, "connectivity", connectivityData{State: "passed", Message: "Destination can reach the source.", LatencyMs: res.LatencyMs})
	default:
		msg := "Destination cannot reach the source."
		if res.Reason != "" {
			msg += " " + res.Reason
		}
		a.renderPartial(w, "connectivity", connectivityData{State: "failed", Message: msg})
	}
}

// handleRetryTransfer is POST /transfers/{id}/retry - re-runs a failed
// transfer as a new one. The RBAC decision and the "only failed" rule live
// in transfers.Service.RetryTransfer; same hx-post/HX-Redirect shape as
// handleLoadInstance, with the failure reason surfacing as the toast.
func (a *API) handleRetryTransfer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for retry transfer: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	id := chi.URLParam(r, "id")
	_, err = a.transferInitiatorSvc.RetryTransfer(ctx, actor, id)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return
	case errors.Is(err, db.ErrModelTransferNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "model transfer not found")
		return
	case errors.Is(err, transfers.ErrNotRetryable), errors.Is(err, transfers.ErrInvalidTransfer),
		errors.Is(err, transfers.ErrDestNodeOffline), errors.Is(err, transfers.ErrSourceNodeOffline),
		errors.Is(err, transfers.ErrSourceNotPresent), errors.Is(err, transfers.ErrPeerNotReady):
		writeError(w, r, http.StatusConflict, "CANNOT_RETRY", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: retry transfer %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/transfers")
	w.WriteHeader(http.StatusNoContent)
}

// handleCancelTransfer is POST /transfers/{id}/cancel - stops a queued or
// running transfer. RBAC and the only-unfinished rule live in
// transfers.Service.CancelTransfer; same hx-post/HX-Redirect shape as
// handleRetryTransfer.
func (a *API) handleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for cancel transfer: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	id := chi.URLParam(r, "id")
	err = a.transferInitiatorSvc.CancelTransfer(ctx, actor, id)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return
	case errors.Is(err, db.ErrModelTransferNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "model transfer not found")
		return
	case errors.Is(err, transfers.ErrNotCancelable):
		writeError(w, r, http.StatusConflict, "ALREADY_FINISHED", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: cancel transfer %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/transfers")
	w.WriteHeader(http.StatusNoContent)
}
