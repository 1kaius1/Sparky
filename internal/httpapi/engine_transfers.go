// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engineprovision"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// engineProvisioner is the subset of *engineprovision.Service this package
// needs for the engine provisioning form.
type engineProvisioner interface {
	ProvisionEngine(ctx context.Context, actor rbac.Actor, params engineprovision.ProvisionEngineParams) (*db.EngineTransfer, error)
}

// engineTransferLister is the subset of *engineprovision.Service this
// package needs for the Engine transfers page's read-only list.
type engineTransferLister interface {
	ListEngineTransfers(ctx context.Context) ([]*db.EngineTransfer, error)
}

// engineTransfersPageData is the Engine transfers page's view model - see
// SCHEMA.md Engine transfers. CanProvision only decides whether the
// "Provision engine" link is shown, same non-security-boundary reasoning as
// nodesPageData.CanRegister - the real check happens inside
// engineprovision.Service.ProvisionEngine.
type engineTransfersPageData struct {
	Transfers    []engineTransferRow
	CanProvision bool
}

type engineTransferRow struct {
	DestNode     string
	EngineType   string
	Version      string
	Status       string
	Progress     string
	RequestedAt  string
	ErrorMessage string
}

func (a *API) handleEngineTransfers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	transfers, err := a.engineTransfers.ListEngineTransfers(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list engine transfers: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for engine transfers: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodeList))
	for _, n := range nodeList {
		nodeNames[n.ID] = n.Name
	}

	rows := make([]engineTransferRow, 0, len(transfers))
	for _, t := range transfers {
		var errMsg string
		if t.ErrorMessage != nil {
			errMsg = *t.ErrorMessage
		}
		rows = append(rows, engineTransferRow{
			DestNode:     nodeNames[t.DestNodeID],
			EngineType:   string(t.EngineType),
			Version:      t.Version,
			Status:       string(t.Status),
			Progress:     formatMB(t.BytesTransferred) + " / " + formatMB(t.BytesTotal),
			RequestedAt:  t.RequestedAt.Format("2006-01-02 15:04:05 MST"),
			ErrorMessage: errMsg,
		})
	}

	// CanProvision only decides whether the "Provision engine" link is
	// shown - it is not the security boundary. The real check happens
	// inside engineprovision.Service.ProvisionEngine, same reasoning as
	// the Nodes page's CanRegister.
	var canProvision bool
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canProvision = rbac.CanManageNodes(actor)
		}
	}

	a.render(w, r, "engine_transfers", "Engine transfers", engineTransfersPageData{Transfers: rows, CanProvision: canProvision})
}

// provisionEnginePageData is the engine provisioning form's view model -
// Error is non-empty, and Form carries back what was submitted, when
// redisplaying the form after a validation failure, same pattern as
// registerNodePageData.
type provisionEnginePageData struct {
	Error string
	Form  provisionEngineFormValues
	Nodes []nodeOption
}

type provisionEngineFormValues struct {
	DestNodeID string
	Version    string
}

func (a *API) handleProvisionEngineForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for provision-engine form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !rbac.CanManageNodes(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	}

	// nodeOptionsForProfileForm is reused as-is - it's just "every node,
	// as an {ID, Name} option list," not actually profile-form-specific.
	options, err := a.nodeOptionsForProfileForm(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for provision-engine form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "provision_engine", "Provision engine", provisionEnginePageData{Nodes: options})
}

func (a *API) handleProvisionEngine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderProvisionEngineError(w, r, "invalid form submission", provisionEngineFormValues{})
		return
	}
	form := provisionEngineFormValues{
		DestNodeID: r.PostFormValue("dest_node_id"),
		Version:    r.PostFormValue("version"),
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for provision engine: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// engine_type is fixed to llamacpp, not read from the form - SCHEMA.md
	// Engine transfers is explicit that only llamacpp rows are realistic
	// through this path today (Python-based engines get a different
	// v0.3.0 mechanism), so the form never offers vllm/aphrodite as
	// choices that don't actually work.
	params := engineprovision.ProvisionEngineParams{
		DestNodeID: form.DestNodeID,
		EngineType: db.ProfileEngineLlamaCPP,
		Version:    form.Version,
	}

	_, err = a.engineProvisioner.ProvisionEngine(ctx, actor, params)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "admin tier required")
		return
	case errors.Is(err, engineprovision.ErrInvalidProvisionRequest), errors.Is(err, engineprovision.ErrDestNodeOffline):
		a.renderProvisionEngineError(w, r, err.Error(), form)
		return
	case err != nil:
		a.logger.Printf("httpapi: provision engine: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/engine-transfers", http.StatusSeeOther)
}

func (a *API) renderProvisionEngineError(w http.ResponseWriter, r *http.Request, errMsg string, form provisionEngineFormValues) {
	options, err := a.nodeOptionsForProfileForm(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes for provision-engine form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "provision_engine", "Provision engine", provisionEnginePageData{Error: errMsg, Form: form, Nodes: options})
}
