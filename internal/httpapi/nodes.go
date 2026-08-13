// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

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
}

// nodesPageData is the Nodes page's view model - CLAUDE.md Frontend
// Conventions' Nodes sidebar tier ("Read-only view"); the "Admin edit"
// half of that tier note is a later phase - no write form exists yet.
type nodesPageData struct {
	Nodes       []nodeRow
	CanRegister bool
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
	ctx := r.Context()

	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]nodeRow, 0, len(nodeList))
	for _, n := range nodeList {
		rows = append(rows, nodeRow{
			Name:        n.Name,
			Hostname:    n.Hostname,
			NodeType:    string(n.NodeType),
			AgentStatus: string(n.AgentStatus),
			GPUMemoryGB: n.GPUMemoryGB,
			CPUMemoryGB: n.CPUMemoryGB,
		})
	}

	// CanRegister only decides whether the "Register node" button/form is
	// shown - it is not the security boundary. The real check happens
	// inside nodes.Service.RegisterNode via rbac.CanManageNodes on every
	// submission, same reasoning as the Users page's per-row
	// ReachableTiers. Listing nodes itself stays unguarded, per this
	// handler's own long-standing doc comment above nodesPageData.
	var canRegister bool
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canRegister = rbac.CanManageNodes(actor)
		}
	}

	a.render(w, r, "nodes", "Nodes", nodesPageData{Nodes: rows, CanRegister: canRegister})
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
	Name             string
	Hostname         string
	IPAddress        string
	NodeType         string
	ContainerRuntime string
	GPUMemoryGB      string
	CPUMemoryGB      string
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
		Name:             r.PostFormValue("name"),
		Hostname:         r.PostFormValue("hostname"),
		IPAddress:        r.PostFormValue("ip_address"),
		NodeType:         r.PostFormValue("node_type"),
		ContainerRuntime: r.PostFormValue("container_runtime"),
		GPUMemoryGB:      r.PostFormValue("gpu_memory_gb"),
		CPUMemoryGB:      r.PostFormValue("cpu_memory_gb"),
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

	var containerRuntime *db.ContainerRuntime
	if form.ContainerRuntime != "" {
		cr := db.ContainerRuntime(form.ContainerRuntime)
		containerRuntime = &cr
	}

	params := nodes.RegisterNodeParams{
		Name:             form.Name,
		Hostname:         form.Hostname,
		IPAddress:        form.IPAddress,
		NodeType:         db.NodeType(form.NodeType),
		ContainerRuntime: containerRuntime,
		GPUMemoryGB:      gpuMemoryGB,
		CPUMemoryGB:      cpuMemoryGB,
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
