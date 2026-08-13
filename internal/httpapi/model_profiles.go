// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/profiles"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// profileEditor is the subset of *profiles.Service this package needs
// for the create/edit form - CreateParams/UpdateParams are referenced
// directly, same reasoning as nodeRegistrar's use of
// nodes.RegisterNodeParams (Dashboard UI Phase 9): the concrete
// *profiles.Service methods take these struct types, so a
// primitives-only interface signature wouldn't be structurally
// satisfied by them at all. GetProfile backs the edit form's own
// prefill read - unguarded by RBAC like ListProfiles, per that method's
// own doc comment.
type profileEditor interface {
	CreateProfile(ctx context.Context, actor rbac.Actor, params profiles.CreateParams) (*db.Profile, error)
	UpdateProfile(ctx context.Context, actor rbac.Actor, params profiles.UpdateParams) (*db.Profile, error)
	GetProfile(ctx context.Context, id string) (*db.Profile, error)
}

// profileEngineTypeOptions is every engine type the create/edit form
// offers - including "aphrodite", which has no adapter registered until
// v0.3.0 (db.ProfileEngineType's own doc comment) and so will always
// fail server-side validation today. Not filtered out of the dropdown:
// it is a real value of the enum, and letting the same validation path
// every other invalid submission goes through reject it (rather than
// hiding it from the UI) avoids a second, form-specific notion of which
// engine types are "really" valid.
var profileEngineTypeOptions = []string{string(db.ProfileEngineVLLM), string(db.ProfileEngineAphrodite), string(db.ProfileEngineLlamaCPP)}

// profilesPageData is the Model profiles page's view model - CLAUDE.md
// Frontend Conventions' Model profiles sidebar tier ("Read-only view /
// Developer launch / PowerDev create"). CanManage gates the "New profile"
// link and each row's "Edit" link; CanLaunch gates the Load/Unload
// controls (Dashboard UI Phase 11) - both resolved the same
// non-security-boundary way as the Nodes page's own CanRegister (Phase 9):
// the real enforcement is rbac.CanManageProfiles/CanLaunchInstances,
// checked again inside the Service methods themselves on every
// submission.
type profilesPageData struct {
	Profiles  []profileRow
	CanManage bool
	CanLaunch bool
}

// nonTerminalInstanceStatuses are the running_instances statuses that mean
// a profile already has an active instance - see SCHEMA.md Running
// instances. A stopped/failed instance leaves its profile eligible to
// load again, so those two are deliberately excluded.
var nonTerminalInstanceStatuses = map[db.RunningInstanceStatus]bool{
	db.RunningInstanceStatusStarting: true,
	db.RunningInstanceStatusRunning:  true,
	db.RunningInstanceStatusStopping: true,
}

type profileRow struct {
	ID               string
	Name             string
	ModelRef         string
	EngineType       string
	TargetNode       string
	Port             int
	InstanceStatus   string
	ActiveInstanceID string
}

func (a *API) handleModelProfiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	profileList, err := a.profiles.ListProfiles(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list model profiles: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for model profiles: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	instanceList, err := a.instances.ListInstances(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list running instances for model profiles: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeNames := make(map[string]string, len(nodeList))
	for _, n := range nodeList {
		nodeNames[n.ID] = n.Name
	}

	// A profile can have at most one non-terminal running instance at a
	// time - lifecycle.Service.LoadInstance refuses a second concurrent
	// load for the same profile (ErrAlreadyRunning) - so the last
	// non-terminal instance seen per profile in ListInstances' result is
	// unambiguous, not an arbitrary pick among several.
	activeByProfile := make(map[string]*db.RunningInstance, len(instanceList))
	for _, inst := range instanceList {
		if nonTerminalInstanceStatuses[inst.Status] {
			activeByProfile[inst.ProfileID] = inst
		}
	}

	rows := make([]profileRow, 0, len(profileList))
	for _, p := range profileList {
		var targetNode string
		if p.TargetNodeID != nil {
			targetNode = nodeNames[*p.TargetNodeID]
		}
		row := profileRow{
			ID:         p.ID,
			Name:       p.Name,
			ModelRef:   p.ModelRef,
			EngineType: string(p.EngineType),
			TargetNode: targetNode,
			Port:       p.Port,
		}
		if active, ok := activeByProfile[p.ID]; ok {
			row.InstanceStatus = string(active.Status)
			row.ActiveInstanceID = active.ID
		}
		rows = append(rows, row)
	}

	var canManage, canLaunch bool
	if identity, ok := IdentityFromContext(ctx); ok {
		if actor, err := a.actorFromIdentity(ctx, identity); err == nil {
			canManage = rbac.CanManageProfiles(actor)
			canLaunch = rbac.CanLaunchInstances(actor)
		}
	}

	a.render(w, r, "profiles", "Model profiles", profilesPageData{Profiles: rows, CanManage: canManage, CanLaunch: canLaunch})
}

// profileFormPageData is the create/edit form's view model - IsEdit and
// ProfileID pick the submission URL and title; Error/Form redisplay a
// failed submission with the reason and every previously-typed value
// preserved, same pattern as the node registration form (Dashboard UI
// Phase 9).
type profileFormPageData struct {
	Error       string
	Form        profileFormValues
	Nodes       []nodeOption
	EngineTypes []string
	IsEdit      bool
	ProfileID   string
}

type nodeOption struct {
	ID   string
	Name string
}

type profileFormValues struct {
	Name             string
	ModelRef         string
	EngineType       string
	EngineParamsJSON string
	RequiredMemoryGB string
	TargetNodeID     string
	Port             string
}

func (a *API) nodeOptionsForProfileForm(ctx context.Context) ([]nodeOption, error) {
	nodeList, err := a.nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	opts := make([]nodeOption, 0, len(nodeList))
	for _, n := range nodeList {
		opts = append(opts, nodeOption{ID: n.ID, Name: n.Name})
	}
	return opts, nil
}

func profileFormValuesFromProfile(p *db.Profile) profileFormValues {
	var targetNodeID string
	if p.TargetNodeID != nil {
		targetNodeID = *p.TargetNodeID
	}
	var requiredMemoryGB string
	if p.RequiredMemoryGB != nil {
		requiredMemoryGB = strconv.FormatFloat(*p.RequiredMemoryGB, 'f', -1, 64)
	}
	return profileFormValues{
		Name:             p.Name,
		ModelRef:         p.ModelRef,
		EngineType:       string(p.EngineType),
		EngineParamsJSON: string(p.EngineParams),
		RequiredMemoryGB: requiredMemoryGB,
		TargetNodeID:     targetNodeID,
		Port:             strconv.Itoa(p.Port),
	}
}

// fieldsFromForm converts submitted form values into profiles.Fields,
// parsing the numeric fields (the only checks that belong here - real
// business validation, including EngineParams' own engine-specific
// shape, is profiles.Fields.validate/Service.resolve's job, not this
// handler's). A blank engine_params defaults to "{}" rather than forcing
// every submission to type it out - all of internal/engines' adapters
// treat an empty params object as valid (every field is optional).
func fieldsFromForm(form profileFormValues) (profiles.Fields, error) {
	port, err := strconv.Atoi(form.Port)
	if err != nil {
		return profiles.Fields{}, errors.New("port must be a whole number")
	}

	var requiredMemoryGB *float64
	if strings.TrimSpace(form.RequiredMemoryGB) != "" {
		v, err := strconv.ParseFloat(form.RequiredMemoryGB, 64)
		if err != nil {
			return profiles.Fields{}, errors.New("required_memory_gb must be a number")
		}
		requiredMemoryGB = &v
	}

	engineParamsRaw := form.EngineParamsJSON
	if strings.TrimSpace(engineParamsRaw) == "" {
		engineParamsRaw = "{}"
	}

	return profiles.Fields{
		Name:             form.Name,
		ModelRef:         form.ModelRef,
		EngineType:       db.ProfileEngineType(form.EngineType),
		EngineParams:     json.RawMessage(engineParamsRaw),
		RequiredMemoryGB: requiredMemoryGB,
		TargetNodeID:     form.TargetNodeID,
		Port:             port,
	}, nil
}

func (a *API) renderProfileFormError(w http.ResponseWriter, r *http.Request, isEdit bool, profileID, errMsg string, form profileFormValues) {
	nodeOpts, err := a.nodeOptionsForProfileForm(r.Context())
	if err != nil {
		a.logger.Printf("httpapi: list nodes for profile form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	title := "New model profile"
	if isEdit {
		title = "Edit model profile"
	}
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, r, "profile_form", title, profileFormPageData{
		Error: errMsg, Form: form, Nodes: nodeOpts, EngineTypes: profileEngineTypeOptions, IsEdit: isEdit, ProfileID: profileID,
	})
}

func (a *API) handleNewProfileForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for new-profile form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !rbac.CanManageProfiles(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "power_dev tier required")
		return
	}

	nodeOpts, err := a.nodeOptionsForProfileForm(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for profile form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "profile_form", "New model profile", profileFormPageData{
		Nodes: nodeOpts, EngineTypes: profileEngineTypeOptions,
	})
}

func (a *API) handleEditProfileForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for edit-profile form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !rbac.CanManageProfiles(actor) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "power_dev tier required")
		return
	}

	id := chi.URLParam(r, "id")
	p, err := a.profileEditor.GetProfile(ctx, id)
	switch {
	case errors.Is(err, db.ErrProfileNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "model profile not found")
		return
	case err != nil:
		a.logger.Printf("httpapi: get model profile %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodeOpts, err := a.nodeOptionsForProfileForm(ctx)
	if err != nil {
		a.logger.Printf("httpapi: list nodes for profile form: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.render(w, r, "profile_form", "Edit model profile", profileFormPageData{
		Form: profileFormValuesFromProfile(p), Nodes: nodeOpts, EngineTypes: profileEngineTypeOptions,
		IsEdit: true, ProfileID: id,
	})
}

func (a *API) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderProfileFormError(w, r, false, "", "invalid form submission", profileFormValues{})
		return
	}
	form := profileFormValues{
		Name:             r.PostFormValue("name"),
		ModelRef:         r.PostFormValue("model_ref"),
		EngineType:       r.PostFormValue("engine_type"),
		EngineParamsJSON: r.PostFormValue("engine_params"),
		RequiredMemoryGB: r.PostFormValue("required_memory_gb"),
		TargetNodeID:     r.PostFormValue("target_node_id"),
		Port:             r.PostFormValue("port"),
	}

	fields, err := fieldsFromForm(form)
	if err != nil {
		a.renderProfileFormError(w, r, false, "", err.Error(), form)
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for create profile: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	_, err = a.profileEditor.CreateProfile(ctx, actor, profiles.CreateParams{Fields: fields})
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "power_dev tier required")
		return
	case errors.Is(err, profiles.ErrInvalidProfile):
		a.renderProfileFormError(w, r, false, "", err.Error(), form)
		return
	case err != nil:
		a.logger.Printf("httpapi: create model profile: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/profiles", http.StatusSeeOther)
}

func (a *API) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	id := chi.URLParam(r, "id")

	if err := r.ParseForm(); err != nil {
		a.renderProfileFormError(w, r, true, id, "invalid form submission", profileFormValues{})
		return
	}
	form := profileFormValues{
		Name:             r.PostFormValue("name"),
		ModelRef:         r.PostFormValue("model_ref"),
		EngineType:       r.PostFormValue("engine_type"),
		EngineParamsJSON: r.PostFormValue("engine_params"),
		RequiredMemoryGB: r.PostFormValue("required_memory_gb"),
		TargetNodeID:     r.PostFormValue("target_node_id"),
		Port:             r.PostFormValue("port"),
	}

	fields, err := fieldsFromForm(form)
	if err != nil {
		a.renderProfileFormError(w, r, true, id, err.Error(), form)
		return
	}

	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for update profile: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	_, err = a.profileEditor.UpdateProfile(ctx, actor, profiles.UpdateParams{ID: id, Fields: fields})
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "power_dev tier required")
		return
	case errors.Is(err, db.ErrProfileNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "model profile not found")
		return
	case errors.Is(err, profiles.ErrInvalidProfile):
		a.renderProfileFormError(w, r, true, id, err.Error(), form)
		return
	case err != nil:
		a.logger.Printf("httpapi: update model profile %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/profiles", http.StatusSeeOther)
}
