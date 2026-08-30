// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/transfers"
)

// transferInitiator is the subset of *transfers.Service this package needs
// for the model download initiation form - separate from transferLister
// (the Model transfers page's read-only list) the same way engineProvisioner
// is kept separate from engineTransferLister, even though both are backed
// by the same concrete service in cmd/sparky-server/main.go.
type transferInitiator interface {
	CanInitiateTransfer(ctx context.Context, actor rbac.Actor) (bool, error)
	InitiateTransfer(ctx context.Context, actor rbac.Actor, params transfers.InitiateTransferParams) (*db.ModelTransfer, error)
}

// initiateTransferPageData is the model download form's view model - Error
// is non-empty, and Form carries back what was submitted, when redisplaying
// the form after a validation failure, same pattern as
// provisionEnginePageData.
type initiateTransferPageData struct {
	Error string
	Form  initiateTransferFormValues
	Nodes []nodeOption
}

type initiateTransferFormValues struct {
	DestNodeID   string
	ModelRef     string
	Quantization string
}

func (a *API) handleInitiateTransferForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for initiate-transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	permitted, err := a.transferInitiatorSvc.CanInitiateTransfer(ctx, actor)
	if err != nil {
		a.logger.Printf("httpapi: check initiate-transfer permission: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !permitted {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return
	}

	// nodeOptionsForProfileForm is reused as-is - it's just "every node, as
	// an {ID, Name} option list," not actually profile-form-specific.
	options, err := a.nodeOptionsForProfileForm(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for initiate-transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "initiate_transfer", "New model transfer", initiateTransferPageData{Nodes: options})
}

func (a *API) handleInitiateTransfer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderInitiateTransferError(w, r, "invalid form submission", initiateTransferFormValues{})
		return
	}
	form := initiateTransferFormValues{
		DestNodeID:   r.PostFormValue("dest_node_id"),
		ModelRef:     r.PostFormValue("model_ref"),
		Quantization: r.PostFormValue("quantization"),
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for initiate transfer: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// source_type is not read from the form - v0.1.0 only initiates
	// internet-sourced (Hugging Face) downloads (SCHEMA.md Model
	// transfers), so InitiateTransferParams has no field to set here yet.
	params := transfers.InitiateTransferParams{
		DestNodeID:   form.DestNodeID,
		ModelRef:     form.ModelRef,
		Quantization: form.Quantization,
	}

	_, err = a.transferInitiatorSvc.InitiateTransfer(ctx, actor, params)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "manage_model_store capability required")
		return
	case errors.Is(err, transfers.ErrInvalidTransfer), errors.Is(err, transfers.ErrDestNodeOffline):
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
		a.logger.Printf("httpapi: list nodes for initiate-transfer form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "initiate_transfer", "New model transfer", initiateTransferPageData{Error: errMsg, Form: form, Nodes: options})
}
