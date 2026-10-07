// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// nodeRegistrar is the subset of *nodes.Service this package needs for
// the node registration form - RegisterNodeParams is referenced directly
// (rather than a primitives-only method signature) since that struct is
// nodes.Service.RegisterNode's real parameter type; a narrower signature
// here would mean *nodes.Service no longer structurally satisfies this
// interface at all.
type nodeRegistrar interface {
	RegisterNode(ctx context.Context, actor rbac.Actor, params nodes.RegisterNodeParams) (*db.Node, string, error)
	// The node edit page's methods - same *nodes.Service, same
	// "same value, multiple interfaces" reasoning as registrar/nodes.
	GetNode(ctx context.Context, id string) (*db.Node, error)
	ListInterfaces(ctx context.Context, nodeID string) ([]*db.NodeNetworkInterface, error)
	RescanInterfaces(ctx context.Context, actor rbac.Actor, nodeID string) error
	SetDefaultTransferInterface(ctx context.Context, actor rbac.Actor, nodeID, interfaceName string) error
	SetComment(ctx context.Context, actor rbac.Actor, nodeID, comment string) error
}

// nodesPageData is the Nodes page's view model - CLAUDE.md Frontend
// Conventions' Nodes sidebar tier ("Read-only view"); the "Admin edit"
// half of that tier note is a later phase - no write form exists yet.
type nodesPageData struct {
	Nodes       []nodeRow
	CanRegister bool
	// CanEdit only decides whether each row's Edit link is shown - the
	// real gate is rbac.CanManageNodes inside the edit handlers/service.
	CanEdit bool
	// CanEditComment only decides whether each row's "Edit comment" link is
	// shown - the real gate is rbac.CanEditNodeComment inside the comment
	// edit handlers/service. Deliberately separate from CanEdit: PowerDev
	// may see this link without seeing the full Edit link.
	CanEditComment bool
}

type nodeRow struct {
	ID             string
	Name           string
	Hostname       string
	RuntimeBackend string
	AgentStatus    string
	GPUMemoryGB    float64
	CPUMemoryGB    float64
	Comment        string
}

func (a *API) handleNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]nodeRow, 0, len(nodeList))
	for _, n := range nodeList {
		row := nodeRow{
			ID:             n.ID,
			Name:           n.Name,
			Hostname:       n.Hostname,
			RuntimeBackend: string(n.RuntimeBackend),
			AgentStatus:    string(n.AgentStatus),
			GPUMemoryGB:    n.GPUMemoryGB,
			CPUMemoryGB:    n.CPUMemoryGB,
		}
		if n.Comment != nil {
			row.Comment = truncateComment(*n.Comment)
		}
		rows = append(rows, row)
	}

	// CanRegister only decides whether the "Register node" button/form is
	// shown - it is not the security boundary. The real check happens
	// inside nodes.Service.RegisterNode via rbac.CanManageNodes on every
	// submission, same reasoning as the Users page's per-row
	// ReachableTiers. Listing nodes itself stays unguarded, per this
	// handler's own long-standing doc comment above nodesPageData.
	var canRegister, canEditComment bool
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canRegister = rbac.CanManageNodes(actor)
			canEditComment = rbac.CanEditNodeComment(actor)
		}
	}

	a.render(w, r, "nodes", "Nodes", nodesPageData{Nodes: rows, CanRegister: canRegister, CanEdit: canRegister, CanEditComment: canEditComment})
}

// registerNodePageData is the node registration form's view model -
// Error is non-empty, and Form carries back what was submitted, when
// redisplaying the form after a validation failure so the Admin doesn't
// have to retype everything.
type registerNodePageData struct {
	Error string
	Form  registerNodeFormValues
}

type registerNodeFormValues struct {
	Name           string
	Hostname       string
	IPAddress      string
	RuntimeBackend string
	GPUMemoryGB    string
	CPUMemoryGB    string
}

// nodeRegisteredPageData is the post-registration confirmation page's
// view model - BearerToken is plaintext and shown here only once, per
// nodes.Service.RegisterNode's own doc comment.
type nodeRegisteredPageData struct {
	NodeName    string
	BearerToken string
}

func (a *API) handleRegisterNodeForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for register-node form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !rbac.CanManageNodes(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	}

	a.render(w, r, "register_node", "Register node", registerNodePageData{})
}

func (a *API) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderRegisterNodeError(w, r, "invalid form submission", registerNodeFormValues{})
		return
	}
	form := registerNodeFormValues{
		Name:           r.PostFormValue("name"),
		Hostname:       r.PostFormValue("hostname"),
		IPAddress:      r.PostFormValue("ip_address"),
		RuntimeBackend: r.PostFormValue("runtime_backend"),
		GPUMemoryGB:    r.PostFormValue("gpu_memory_gb"),
		CPUMemoryGB:    r.PostFormValue("cpu_memory_gb"),
	}

	gpuMemoryGB, err := strconv.ParseFloat(form.GPUMemoryGB, 64)
	if err != nil {
		a.renderRegisterNodeError(w, r, "gpu_memory_gb must be a number", form)
		return
	}
	cpuMemoryGB, err := strconv.ParseFloat(form.CPUMemoryGB, 64)
	if err != nil {
		a.renderRegisterNodeError(w, r, "cpu_memory_gb must be a number", form)
		return
	}

	params := nodes.RegisterNodeParams{
		Name:           form.Name,
		Hostname:       form.Hostname,
		IPAddress:      form.IPAddress,
		RuntimeBackend: db.RuntimeBackend(form.RuntimeBackend),
		GPUMemoryGB:    gpuMemoryGB,
		CPUMemoryGB:    cpuMemoryGB,
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for register node: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	node, bearerToken, err := a.registrar.RegisterNode(ctx, actor, params)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, nodes.ErrInvalidNode):
		a.renderRegisterNodeError(w, r, err.Error(), form)
		return
	case err != nil:
		a.logger.Printf("httpapi: register node: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "node_registered", "Node registered", nodeRegisteredPageData{NodeName: node.Name, BearerToken: bearerToken})
}

func (a *API) renderRegisterNodeError(w http.ResponseWriter, r *http.Request, errMsg string, form registerNodeFormValues) {
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "register_node", "Register node", registerNodePageData{Error: errMsg, Form: form})
}

// nodeEditPageData is the node edit page's view model. SSH keys are public
// keys only - the private half never leaves the node. DefaultInterface ""
// means "Fastest".
type nodeEditPageData struct {
	NodeID           string
	NodeName         string
	SSHPublicKey     string
	SSHHostPublicKey string
	Interfaces       []nodeInterfaceRow
	DefaultInterface string
	Error            string
}

type nodeInterfaceRow struct {
	Name      string
	IPAddress string
	Speed     string
}

// requireNodeAdmin resolves the actor and enforces rbac.CanManageNodes for
// the node edit routes' pages, writing the error response itself.
func (a *API) requireNodeAdmin(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for node edit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	if !rbac.CanManageNodes(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return rbac.Actor{}, false
	}
	return actor, true
}

func (a *API) buildNodeEditData(ctx context.Context, nodeID string) (nodeEditPageData, error) {
	node, err := a.registrar.GetNode(ctx, nodeID)
	if err != nil {
		return nodeEditPageData{}, err
	}
	ifaces, err := a.registrar.ListInterfaces(ctx, nodeID)
	if err != nil {
		return nodeEditPageData{}, err
	}
	data := nodeEditPageData{NodeID: node.ID, NodeName: node.Name}
	if node.SSHPublicKey != nil {
		data.SSHPublicKey = *node.SSHPublicKey
	}
	if node.SSHHostPublicKey != nil {
		data.SSHHostPublicKey = *node.SSHHostPublicKey
	}
	if node.DefaultTransferInterface != nil {
		data.DefaultInterface = *node.DefaultTransferInterface
	}
	for _, i := range ifaces {
		speed := "unknown"
		if i.LinkSpeedMbps != nil {
			speed = strconv.Itoa(*i.LinkSpeedMbps) + " Mbps"
		}
		data.Interfaces = append(data.Interfaces, nodeInterfaceRow{Name: i.InterfaceName, IPAddress: i.IPAddress, Speed: speed})
	}
	return data, nil
}

// handleEditNodeForm is GET /nodes/{id}/edit - Admin-only (same
// rbac.CanManageNodes gate as registration): read-only SSH identity, the
// node's reported interfaces with a Rescan action, and the one editable
// field, its default transfer interface.
func (a *API) handleEditNodeForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireNodeAdmin(w, r); !ok {
		return
	}
	nodeID := chi.URLParam(r, "id")
	data, err := a.buildNodeEditData(r.Context(), nodeID)
	if errors.Is(err, db.ErrNodeNotFound) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: build node edit page for %s: %v", nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "node_edit", "Edit node", data)
}

// handleUpdateNode is POST /nodes/{id}/edit - sets the default transfer
// interface ("" = Fastest). The RBAC gate and audit live in
// nodes.Service.SetDefaultTransferInterface.
func (a *API) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireNodeAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	nodeID := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}

	err := a.registrar.SetDefaultTransferInterface(ctx, actor, nodeID, r.PostFormValue("default_transfer_interface"))
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, db.ErrNodeNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	case errors.Is(err, nodes.ErrUnknownInterface):
		data, buildErr := a.buildNodeEditData(ctx, nodeID)
		if buildErr != nil {
			a.logger.Printf("httpapi: rebuild node edit page for %s: %v", nodeID, buildErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "node_edit", "Edit node", data)
		return
	case err != nil:
		a.logger.Printf("httpapi: set default transfer interface for node %s: %v", nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

// nodeCommentListPreviewLen is how many characters of a node's comment are
// shown in the Nodes list table - the full text is only on the dedicated
// comment edit page.
const nodeCommentListPreviewLen = 60

// truncateComment shortens comment for the Nodes list table display.
func truncateComment(comment string) string {
	if len(comment) <= nodeCommentListPreviewLen {
		return comment
	}
	return comment[:nodeCommentListPreviewLen] + "..."
}

// nodeCommentPageData is the node comment edit page's view model.
// CommentUpdatedByName is "" when the comment has never been set, or when
// the break-glass SuperAdmin made the last change (see resolveUserName) -
// the template tells the two apart via CommentUpdatedAt.
type nodeCommentPageData struct {
	NodeID               string
	NodeName             string
	Comment              string
	CommentUpdatedByName string
	CommentUpdatedAt     string
	Error                string
}

// requireNodeCommentEditor resolves the actor and enforces
// rbac.CanEditNodeComment for the node comment edit routes' pages, writing
// the error response itself - same shape as requireNodeAdmin, but the
// broader Admin-or-PowerDev gate that guards only the comment field.
func (a *API) requireNodeCommentEditor(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for node comment edit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	if !rbac.CanEditNodeComment(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin or powerdev tier required")
		return rbac.Actor{}, false
	}
	return actor, true
}

func (a *API) buildNodeCommentData(ctx context.Context, nodeID string) (nodeCommentPageData, error) {
	node, err := a.registrar.GetNode(ctx, nodeID)
	if err != nil {
		return nodeCommentPageData{}, err
	}
	data := nodeCommentPageData{NodeID: node.ID, NodeName: node.Name}
	if node.Comment != nil {
		data.Comment = *node.Comment
	}
	data.CommentUpdatedByName = a.resolveUserName(ctx, node.CommentUpdatedBy)
	if node.CommentUpdatedAt != nil {
		data.CommentUpdatedAt = node.CommentUpdatedAt.Format("2006-01-02 15:04 MST")
	}
	return data, nil
}

// handleEditNodeCommentForm is GET /nodes/{id}/comment - Admin or PowerDev
// (rbac.CanEditNodeComment): a free-text note on which team/model is using
// this node, independent from the rest of the node edit page's Admin-only
// settings.
func (a *API) handleEditNodeCommentForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireNodeCommentEditor(w, r); !ok {
		return
	}
	nodeID := chi.URLParam(r, "id")
	data, err := a.buildNodeCommentData(r.Context(), nodeID)
	if errors.Is(err, db.ErrNodeNotFound) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	if err != nil {
		a.logger.Printf("httpapi: build node comment page for %s: %v", nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "node_comment_edit", "Edit node comment", data)
}

// handleUpdateNodeComment is POST /nodes/{id}/comment - sets or clears the
// node's comment field. The RBAC gate and audit live in
// nodes.Service.SetComment.
func (a *API) handleUpdateNodeComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireNodeCommentEditor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	nodeID := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}

	err := a.registrar.SetComment(ctx, actor, nodeID, r.PostFormValue("comment"))
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin or powerdev tier required")
		return
	case errors.Is(err, db.ErrNodeNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	case errors.Is(err, nodes.ErrCommentTooLong):
		data, buildErr := a.buildNodeCommentData(ctx, nodeID)
		if buildErr != nil {
			a.logger.Printf("httpapi: rebuild node comment page for %s: %v", nodeID, buildErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "node_comment_edit", "Edit node comment", data)
		return
	case err != nil:
		a.logger.Printf("httpapi: set comment for node %s: %v", nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

// handleRescanInterfaces is POST /nodes/{id}/rescan-interfaces - asks the
// node's agent to re-report its interfaces; the page refreshes when the
// report arrives (SSE report_interfaces).
func (a *API) handleRescanInterfaces(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireNodeAdmin(w, r)
	if !ok {
		return
	}
	nodeID := chi.URLParam(r, "id")
	err := a.registrar.RescanInterfaces(r.Context(), actor, nodeID)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, nodes.ErrNodeNotConnected):
		writeError(w, r, http.StatusConflict, "NODE_OFFLINE", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: rescan interfaces for node %s: %v", nodeID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
