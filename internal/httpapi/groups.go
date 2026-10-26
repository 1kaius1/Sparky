// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/groups"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// groupManager is the subset of *groups.Service this package needs. Every call
// checks the actor's tier itself, and every change is audited inside it.
type groupManager interface {
	List(ctx context.Context, actor rbac.Actor) ([]*db.AppGroupSummary, error)
	Get(ctx context.Context, actor rbac.Actor, id string) (*groups.Detail, error)
	Create(ctx context.Context, actor rbac.Actor, name string) (*db.AppGroup, error)
	Rename(ctx context.Context, actor rbac.Actor, id, name string) error
	Delete(ctx context.Context, actor rbac.Actor, id string) error
	SearchDirectory(ctx context.Context, actor rbac.Actor, groupID, term string) (*groups.SearchResult, error)
	MapADGroup(ctx context.Context, actor rbac.Actor, groupID, dn string) (*groups.MapResult, error)
	UnmapADGroup(ctx context.Context, actor rbac.Actor, groupID, dn string) error
	AddLocalMember(ctx context.Context, actor rbac.Actor, groupID, userID string) error
	RemoveLocalMember(ctx context.Context, actor rbac.Actor, groupID, userID string) error
	DirectoryAvailable() bool
}

// maxGroupFormBytes bounds a Groups form: a name, a DN, a user id and the token.
const maxGroupFormBytes = 16 << 10

// groupRow is one group in the list.
type groupRow struct {
	ID           string
	Name         string
	ADGroups     int
	Members      int
	LocalMembers int
}

type groupsPageData struct {
	Rows               []groupRow
	DirectoryAvailable bool

	// NewName refills the create form after a refused attempt.
	NewName string
	Error   string
	Notice  string
}

type groupMappingRow struct {
	DN   string
	Name string
}

type groupMemberRow struct {
	UserID string
	Name   string
	Tier   string
	Login  string
	Via    []string
	Manual bool
}

type addableUser struct {
	ID    string
	Label string
}

type groupPageData struct {
	ID       string
	Name     string
	Mappings []groupMappingRow
	Members  []groupMemberRow

	DirectoryAvailable    bool
	CanManageLocalMembers bool
	AddableUsers          []addableUser

	// RenameValue refills the rename form after a refused attempt.
	RenameValue string
	Error       string
	Notice      string
}

// adCandidateRow is one directory group offered by the search.
type adCandidateRow struct {
	DN          string
	Name        string
	Description string
	Mapped      bool
}

// adSearchData is the search results fragment. Message is a hint or a failure
// the fragment shows in place of results.
type adSearchData struct {
	GroupID    string
	CSRFToken  string
	Candidates []adCandidateRow
	Truncated  bool
	Message    string
}

// groupsActor resolves the viewer, answering the request itself on failure.
// ok is false when the handler should stop.
func (a *API) groupsActor(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for groups: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	return actor, true
}

// groupNotice turns a ?notice= code from a redirect into the sentence shown. It
// is a fixed set, so a crafted URL cannot put its own text on the page.
func groupNotice(code, matched string) string {
	switch code {
	case "created":
		return "Group created."
	case "renamed":
		return "Group renamed."
	case "mapped":
		n, err := strconv.Atoi(matched)
		if err != nil || n < 0 {
			return "AD group mapped."
		}
		return "AD group mapped. " + strconv.Itoa(n) + " of its members have already signed in to Sparky and were added."
	case "mapped_late":
		return "AD group mapped, but its member list could not be read just now. Members will appear as people sign in."
	case "unmapped":
		return "AD group unmapped."
	case "member_added":
		return "Member added."
	case "member_removed":
		return "Member removed."
	default:
		return ""
	}
}

func groupURL(id, notice string, extra url.Values) string {
	u := "/groups/" + url.PathEscape(id)
	q := url.Values{}
	if notice != "" {
		q.Set("notice", notice)
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// groupErrorResponse maps a service error to the status and the sentence shown
// to the person. ok is false for an error with no friendly form (the caller
// logs it and answers 500); a permission or not-found error is handled by the
// caller before this.
func groupErrorResponse(err error) (status int, msg string, ok bool) {
	switch {
	case errors.Is(err, groups.ErrNameTaken):
		return http.StatusConflict, "A group with that name already exists.", true
	case errors.Is(err, groups.ErrInvalidName):
		return http.StatusBadRequest, "That is not a valid group name: it must be 1 to " + strconv.Itoa(groups.MaxNameLen) + " characters, with no control characters.", true
	case errors.Is(err, groups.ErrADGroupNotFound):
		return http.StatusBadRequest, "That group was not found in the directory.", true
	case errors.Is(err, groups.ErrAlreadyMapped):
		return http.StatusConflict, "That AD group is already mapped to this group.", true
	case errors.Is(err, groups.ErrNotLocalAccount):
		return http.StatusBadRequest, "Only local accounts can be added by hand. An AD account is a member through its AD groups.", true
	case errors.Is(err, groups.ErrDirectoryUnavailable):
		return http.StatusConflict, "No directory is configured, so AD groups cannot be searched or mapped.", true
	case errors.Is(err, groups.ErrDirectory):
		return http.StatusBadGateway, "The directory could not be reached just now. Try again in a moment.", true
	default:
		return 0, "", false
	}
}

func (a *API) buildGroupsRows(list []*db.AppGroupSummary) []groupRow {
	rows := make([]groupRow, 0, len(list))
	for _, g := range list {
		rows = append(rows, groupRow{ID: g.ID, Name: g.Name, ADGroups: g.ADGroupCount, Members: g.MemberCount, LocalMembers: g.LocalMemberCount})
	}
	return rows
}

// renderGroupsPage draws the list. status is 200 unless it is showing a refused
// form.
func (a *API) renderGroupsPage(w http.ResponseWriter, r *http.Request, actor rbac.Actor, status int, data groupsPageData) {
	list, err := a.groups.List(r.Context(), actor)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err != nil:
		a.logger.Printf("httpapi: list groups: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.Rows = a.buildGroupsRows(list)
	data.DirectoryAvailable = a.groups.DirectoryAvailable()
	a.renderStatus(w, r, status, "groups", "Groups", data)
}

// handleGroups is GET /groups: every group, and the form to create one.
func (a *API) handleGroups(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.groupsActor(w, r)
	if !ok {
		return
	}
	a.renderGroupsPage(w, r, actor, http.StatusOK, groupsPageData{Notice: groupNotice(r.URL.Query().Get("notice"), "")})
}

// handleCreateGroup is POST /groups.
func (a *API) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.groupsActor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderGroupsPage(w, r, actor, http.StatusBadRequest, groupsPageData{Error: "invalid form submission"})
		return
	}
	name := r.PostFormValue("name")
	g, err := a.groups.Create(r.Context(), actor, name)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case err == nil:
		http.Redirect(w, r, groupURL(g.ID, "created", nil), http.StatusSeeOther)
		return
	}
	if status, msg, known := groupErrorResponse(err); known {
		a.renderGroupsPage(w, r, actor, status, groupsPageData{Error: msg, NewName: name})
		return
	}
	a.logger.Printf("httpapi: create group: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// renderGroupPage draws one group. status is 200 unless it is showing a refused
// form; renameValue refills the rename form after one.
func (a *API) renderGroupPage(w http.ResponseWriter, r *http.Request, actor rbac.Actor, id string, status int, notice, errMsg, renameValue string) {
	d, err := a.groups.Get(r.Context(), actor, id)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, groups.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		a.logger.Printf("httpapi: get group %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := groupPageData{
		ID: d.Group.ID, Name: d.Group.Name,
		DirectoryAvailable: d.DirectoryAvailable, CanManageLocalMembers: d.CanManageLocalMembers,
		RenameValue: d.Group.Name, Error: errMsg, Notice: notice,
	}
	if renameValue != "" {
		data.RenameValue = renameValue
	}
	for _, m := range d.Mappings {
		data.Mappings = append(data.Mappings, groupMappingRow{DN: m.DN, Name: m.Name})
	}
	for _, m := range d.Members {
		row := groupMemberRow{UserID: m.UserID, Name: m.DisplayName, Tier: string(m.Tier), Via: m.ViaADGroups, Manual: m.AddedManually}
		if m.LocalUsername != nil {
			row.Login = *m.LocalUsername
		}
		data.Members = append(data.Members, row)
	}
	for _, u := range d.AddableLocalUsers {
		label := u.DisplayName
		if u.LocalUsername != nil && *u.LocalUsername != u.DisplayName {
			label += " (" + *u.LocalUsername + ")"
		}
		data.AddableUsers = append(data.AddableUsers, addableUser{ID: u.ID, Label: label})
	}
	a.renderStatus(w, r, status, "group", d.Group.Name+" - Groups", data)
}

// handleGroup is GET /groups/{id}.
func (a *API) handleGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.groupsActor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	a.renderGroupPage(w, r, actor, chi.URLParam(r, "id"), http.StatusOK, groupNotice(q.Get("notice"), q.Get("matched")), "", "")
}

// groupFormHandler is the shape every POST on a group page shares: resolve the
// actor, parse the form, run one service call, then redirect with a notice on
// success or redraw the page with the reason on a refusal. do returns the
// redirect URL's notice code and extra query values.
func (a *API) groupFormHandler(what string, do func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (notice string, extra url.Values, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.groupsActor(w, r)
		if !ok {
			return
		}
		id := chi.URLParam(r, "id")
		if err := r.ParseForm(); err != nil {
			a.renderGroupPage(w, r, actor, id, http.StatusBadRequest, "", "invalid form submission", "")
			return
		}
		notice, extra, err := do(r.Context(), actor, id, r.PostForm)
		switch {
		case errors.Is(err, rbac.ErrNotPermitted):
			a.renderForbidden(w, r, actor.Tier)
			return
		case errors.Is(err, groups.ErrNotFound):
			http.NotFound(w, r)
			return
		case err == nil:
			http.Redirect(w, r, groupURL(id, notice, extra), http.StatusSeeOther)
			return
		}
		if status, msg, known := groupErrorResponse(err); known {
			if status == http.StatusBadGateway {
				a.logger.Printf("httpapi: %s for group %s: %v", what, id, err)
			}
			a.renderGroupPage(w, r, actor, id, status, "", msg, r.PostForm.Get("name"))
			return
		}
		a.logger.Printf("httpapi: %s for group %s: %v", what, id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (a *API) handleRenameGroup() http.HandlerFunc {
	return a.groupFormHandler("rename group", func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (string, url.Values, error) {
		return "renamed", nil, a.groups.Rename(ctx, actor, id, form.Get("name"))
	})
}

// handleDeleteGroup is POST /groups/{id}/delete. It needs the confirm box
// ticked: a delete takes the group's mappings and manual members with it.
func (a *API) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.groupsActor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil || r.PostForm.Get("confirm") != "yes" {
		a.renderGroupPage(w, r, actor, id, http.StatusBadRequest, "", "Tick the box to confirm before deleting the group.", "")
		return
	}
	err := a.groups.Delete(r.Context(), actor, id)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
	case errors.Is(err, groups.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		a.logger.Printf("httpapi: delete group %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, "/groups", http.StatusSeeOther)
	}
}

func (a *API) handleMapADGroup() http.HandlerFunc {
	return a.groupFormHandler("map AD group", func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (string, url.Values, error) {
		res, err := a.groups.MapADGroup(ctx, actor, id, form.Get("dn"))
		if err != nil {
			return "", nil, err
		}
		if res.BackfillFailed {
			return "mapped_late", nil, nil
		}
		return "mapped", url.Values{"matched": {strconv.Itoa(res.MatchedMembers)}}, nil
	})
}

func (a *API) handleUnmapADGroup() http.HandlerFunc {
	return a.groupFormHandler("unmap AD group", func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (string, url.Values, error) {
		return "unmapped", nil, a.groups.UnmapADGroup(ctx, actor, id, form.Get("dn"))
	})
}

func (a *API) handleAddGroupMember() http.HandlerFunc {
	return a.groupFormHandler("add group member", func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (string, url.Values, error) {
		return "member_added", nil, a.groups.AddLocalMember(ctx, actor, id, form.Get("user_id"))
	})
}

func (a *API) handleRemoveGroupMember() http.HandlerFunc {
	return a.groupFormHandler("remove group member", func(ctx context.Context, actor rbac.Actor, id string, form url.Values) (string, url.Values, error) {
		return "member_removed", nil, a.groups.RemoveLocalMember(ctx, actor, id, form.Get("user_id"))
	})
}

// handleADSearch is GET /groups/{id}/ad-search?q=, the fragment the group page
// fills in as someone types. A hint or a failure is shown as a message in the
// fragment (with a 200), not as an error status: htmx does not swap a non-2xx
// response in, so the person would see nothing. The search itself is a read and
// not audited; it needs the same tier as the page and only ever returns a
// capped list for a typed term.
func (a *API) handleADSearch(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.groupsActor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	data := adSearchData{GroupID: id, CSRFToken: csrfTokenFromContext(r.Context())}

	res, err := a.groups.SearchDirectory(r.Context(), actor, id, r.URL.Query().Get("q"))
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case errors.Is(err, groups.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, groups.ErrInvalidSearch):
		// Typing is not an error; say what is needed, and nothing at all for an empty box.
		if strings.TrimSpace(r.URL.Query().Get("q")) != "" {
			data.Message = "Type at least " + strconv.Itoa(groups.MinSearchLen) + " characters of the group's name."
		}
	case errors.Is(err, groups.ErrDirectoryUnavailable):
		data.Message = "No directory is configured, so AD groups cannot be searched."
	case errors.Is(err, groups.ErrDirectory):
		a.logger.Printf("httpapi: search directory for group %s: %v", id, err)
		data.Message = "The directory could not be searched just now. Try again in a moment."
	case err != nil:
		a.logger.Printf("httpapi: search directory for group %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	default:
		for _, c := range res.Candidates {
			data.Candidates = append(data.Candidates, adCandidateRow{DN: c.DN, Name: c.Name, Description: c.Description, Mapped: c.Mapped})
		}
		data.Truncated = res.Truncated
		if len(data.Candidates) == 0 {
			data.Message = "No AD group's name contains that."
		}
	}
	a.renderPartial(w, "ad_group_results", data)
}
