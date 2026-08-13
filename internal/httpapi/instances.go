// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/lifecycle"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// instanceLauncher is the subset of *lifecycle.Service this package needs
// for the Model profiles page's Load/Unload controls (PLANNING.md
// Dashboard UI Phase 11, the fourth and last write/action form this
// milestone scoped) - lifecycle.LoadParams is referenced directly, same
// "references the domain package's own parameter type" shape as
// profileEditor/nodeRegistrar. LoadInstance/UnloadInstance were already
// fully built and tested (Running instances, PLANNING.md) before this
// phase - this interface only gives them their first HTTP caller, same
// "already built, just needed a caller" shape as Phases 8/9/10's own
// discoveries.
type instanceLauncher interface {
	LoadInstance(ctx context.Context, actor rbac.Actor, params lifecycle.LoadParams) (*db.RunningInstance, error)
	UnloadInstance(ctx context.Context, actor rbac.Actor, instanceID string) (*db.RunningInstance, error)
}

// handleLoadInstance is POST /profiles/{id}/load - loads a running
// instance of the given profile. The RBAC decision
// (rbac.CanLaunchInstances) is not re-checked here - it lives entirely
// inside lifecycle.Service.LoadInstance itself, matching every other write
// path in this codebase (e.g. handleElevateUser's own doc comment). Same
// hx-post/hx-swap="none"/HX-Redirect pattern as handleElevateUser - a
// one-click action with no fields to preserve on failure, so a failed
// submission surfaces only as the JSON error response, not a re-rendered
// form.
func (a *API) handleLoadInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for load instance: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	profileID := chi.URLParam(r, "id")

	_, err = a.launcher.LoadInstance(ctx, actor, lifecycle.LoadParams{ProfileID: profileID})
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "developer tier required")
		return
	case errors.Is(err, db.ErrProfileNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "model profile not found")
		return
	case errors.Is(err, lifecycle.ErrAlreadyRunning):
		writeError(w, r, http.StatusConflict, "ALREADY_RUNNING", err.Error())
		return
	case errors.Is(err, lifecycle.ErrTargetNodeOffline):
		writeError(w, r, http.StatusConflict, "TARGET_NODE_OFFLINE", err.Error())
		return
	case errors.Is(err, lifecycle.ErrInvalidLoad):
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: load instance for profile %s: %v", profileID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/profiles")
	w.WriteHeader(http.StatusNoContent)
}

// handleUnloadInstance is POST /instances/{id}/unload - stops a running
// instance. Same RBAC-lives-in-the-service, hx-post/HX-Redirect pattern as
// handleLoadInstance above.
func (a *API) handleUnloadInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for unload instance: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	instanceID := chi.URLParam(r, "id")

	_, err = a.launcher.UnloadInstance(ctx, actor, instanceID)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "developer tier required")
		return
	case errors.Is(err, db.ErrRunningInstanceNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "running instance not found")
		return
	case errors.Is(err, lifecycle.ErrInstanceNotRunning):
		writeError(w, r, http.StatusConflict, "NOT_RUNNING", err.Error())
		return
	case errors.Is(err, lifecycle.ErrTargetNodeOffline):
		writeError(w, r, http.StatusConflict, "TARGET_NODE_OFFLINE", err.Error())
		return
	case err != nil:
		a.logger.Printf("httpapi: unload instance %s: %v", instanceID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/profiles")
	w.WriteHeader(http.StatusNoContent)
}
