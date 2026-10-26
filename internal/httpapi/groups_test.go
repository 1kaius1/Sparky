// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/groups"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/session"
)

const (
	testGroupID = "99999999-9999-4999-8999-999999999999"
	alphaDN     = "CN=Team-Alpha,OU=Groups,DC=example,DC=internal"
)

// fakeGroups implements groupManager. It repeats the real tier rules (so the
// handlers' handling of a refusal is exercised, not a rule of their own),
// records every call, and returns canned results and errors.
type fakeGroups struct {
	list   []*db.AppGroupSummary
	detail map[string]*groups.Detail

	// directory is what DirectoryAvailable reports.
	directory bool

	searchResult *groups.SearchResult
	mapResult    *groups.MapResult
	created      *db.AppGroup

	// err is returned by every state-changing call; searchErr by the search.
	err       error
	searchErr error

	calls []string
}

func (f *fakeGroups) DirectoryAvailable() bool { return f.directory }

func (f *fakeGroups) List(_ context.Context, actor rbac.Actor) ([]*db.AppGroupSummary, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	return f.list, nil
}

func (f *fakeGroups) Get(_ context.Context, actor rbac.Actor, id string) (*groups.Detail, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	d, ok := f.detail[id]
	if !ok {
		return nil, groups.ErrNotFound
	}
	cp := *d
	cp.CanManageLocalMembers = rbac.CanManageGroupLocalMembers(actor)
	if !cp.CanManageLocalMembers {
		cp.AddableLocalUsers = nil
	}
	return &cp, nil
}

func (f *fakeGroups) manage(actor rbac.Actor, call string) error {
	f.calls = append(f.calls, call)
	if !rbac.CanManageGroups(actor) {
		return rbac.ErrNotPermitted
	}
	return f.err
}

func (f *fakeGroups) Create(_ context.Context, actor rbac.Actor, name string) (*db.AppGroup, error) {
	if err := f.manage(actor, "create:"+name); err != nil {
		return nil, err
	}
	if f.created != nil {
		return f.created, nil
	}
	return &db.AppGroup{ID: testGroupID, Name: name}, nil
}

func (f *fakeGroups) Rename(_ context.Context, actor rbac.Actor, id, name string) error {
	return f.manage(actor, "rename:"+id+":"+name)
}

func (f *fakeGroups) Delete(_ context.Context, actor rbac.Actor, id string) error {
	return f.manage(actor, "delete:"+id)
}

func (f *fakeGroups) SearchDirectory(_ context.Context, actor rbac.Actor, id, term string) (*groups.SearchResult, error) {
	f.calls = append(f.calls, "search:"+id+":"+term)
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchResult, nil
}

func (f *fakeGroups) MapADGroup(_ context.Context, actor rbac.Actor, id, dn string) (*groups.MapResult, error) {
	if err := f.manage(actor, "map:"+id+":"+dn); err != nil {
		return nil, err
	}
	if f.mapResult != nil {
		return f.mapResult, nil
	}
	return &groups.MapResult{Group: auth.DirectoryGroup{DN: dn, Name: "Team"}}, nil
}

func (f *fakeGroups) UnmapADGroup(_ context.Context, actor rbac.Actor, id, dn string) error {
	return f.manage(actor, "unmap:"+id+":"+dn)
}

func (f *fakeGroups) AddLocalMember(_ context.Context, actor rbac.Actor, id, userID string) error {
	f.calls = append(f.calls, "addmember:"+id+":"+userID)
	if !rbac.CanManageGroupLocalMembers(actor) {
		return rbac.ErrNotPermitted
	}
	return f.err
}

func (f *fakeGroups) RemoveLocalMember(_ context.Context, actor rbac.Actor, id, userID string) error {
	f.calls = append(f.calls, "removemember:"+id+":"+userID)
	if !rbac.CanManageGroupLocalMembers(actor) {
		return rbac.ErrNotPermitted
	}
	return f.err
}

func sampleGroupDetail(directory bool) *groups.Detail {
	loginName := "loc1"
	return &groups.Detail{
		Group:    &db.AppGroup{ID: testGroupID, Name: "Data Analytics"},
		Mappings: []db.AppGroupADMapping{{DN: alphaDN, Name: "Team-Alpha"}},
		Members: []db.AppGroupMember{
			{UserID: "u-ann", DisplayName: "Ann Adams", Tier: db.TierDeveloper, ViaADGroups: []string{"Team-Alpha"}},
			{UserID: "u-loc", DisplayName: "Local One", Tier: db.TierReadOnly, LocalUsername: &loginName, AddedManually: true},
		},
		DirectoryAvailable: directory,
		AddableLocalUsers:  []*db.User{{ID: "u-new", DisplayName: "New Local", LocalUsername: &loginName}},
	}
}

func groupsAPI(t *testing.T, tier db.Tier, fake *fakeGroups) (*API, string) {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier, DisplayName: "Viewer"}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{})
	api.groups = fake
	return api, "u-1"
}

func TestGroupsNav_OnlyForPowerDevAndAbove(t *testing.T) {
	for tier, want := range map[db.Tier]bool{db.TierReadOnly: false, db.TierDeveloper: false, db.TierPowerDev: true, db.TierAdmin: true} {
		api, uid := groupsAPI(t, tier, &fakeGroups{})
		body := getAs(t, api, uid, "/dashboard").Body.String()
		if got := strings.Contains(body, `href="/groups"`); got != want {
			t.Errorf("tier %s: Groups link shown = %v, want %v", tier, got, want)
		}
	}
}

func TestGroupsList_ShowsGroupsAndTheCreateForm(t *testing.T) {
	fake := &fakeGroups{directory: true, list: []*db.AppGroupSummary{
		{AppGroup: db.AppGroup{ID: testGroupID, Name: "Data <b>Analytics</b>"}, ADGroupCount: 2, MemberCount: 7, LocalMemberCount: 1},
	}}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	rec := getAs(t, api, uid, "/groups")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/groups"`, `name="csrf_token"`, `name="name"`, `href="/groups/` + testGroupID + `"`, "Data &lt;b&gt;Analytics&lt;/b&gt;", "<td>2</td>", "<td>7</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("list is missing %q", want)
		}
	}
	if strings.Contains(body, "<b>Analytics</b>") {
		t.Error("a group name was rendered as live markup")
	}

	if body := getAs(t, groupsAPIOnly(t, db.TierPowerDev, &fakeGroups{}), "u-1", "/groups").Body.String(); !strings.Contains(body, "No groups yet") || !strings.Contains(body, "No AD directory is configured") {
		t.Error("the empty state, or the no-directory note, is missing")
	}
}

// groupsAPIOnly is groupsAPI returning just the API, for one-line uses.
func groupsAPIOnly(t *testing.T, tier db.Tier, fake *fakeGroups) *API {
	api, _ := groupsAPI(t, tier, fake)
	return api
}

func TestGroups_DeveloperIsForbiddenEverywhere(t *testing.T) {
	fake := &fakeGroups{detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierDeveloper, fake)

	for _, path := range []string{"/groups", "/groups/" + testGroupID} {
		if rec := getAs(t, api, uid, path); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, rec.Code)
		}
	}
	for _, target := range []string{"/groups", "/groups/" + testGroupID + "/rename", "/groups/" + testGroupID + "/delete", "/groups/" + testGroupID + "/ad-groups", "/groups/" + testGroupID + "/ad-groups/remove", "/groups/" + testGroupID + "/members", "/groups/" + testGroupID + "/members/remove"} {
		rec := postFormAs(t, api, uid, target, url.Values{"name": {"x"}, "confirm": {"yes"}, "dn": {alphaDN}, "user_id": {"u"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", target, rec.Code)
		}
	}
	if rec := getAs(t, api, uid, "/groups/"+testGroupID+"/ad-search?q=team"); rec.Code != http.StatusForbidden {
		t.Errorf("GET ad-search = %d, want 403", rec.Code)
	}
}

func TestGroupPage_ShowsMappingsMembersAndTheirReasons(t *testing.T) {
	fake := &fakeGroups{directory: true, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierAdmin, fake)

	rec := getAs(t, api, uid, "/groups/"+testGroupID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Data Analytics", "Team-Alpha", alphaDN,
		`action="/groups/` + testGroupID + `/ad-groups/remove"`, // Unmap
		`hx-get="/groups/` + testGroupID + `/ad-search"`,        // the search box
		"Ann Adams", "Team-Alpha", "Local One", "(loc1)", "added by hand",
		`action="/groups/` + testGroupID + `/members/remove"`, // Admin may remove a manual member
		`action="/groups/` + testGroupID + `/members"`,        // and add one
		`value="u-new"`, "New Local",
		`action="/groups/` + testGroupID + `/rename"`, `action="/groups/` + testGroupID + `/delete"`, `name="confirm"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("group page is missing %q", want)
		}
	}
}

func TestGroupPage_PowerDevCannotManageLocalMembers(t *testing.T) {
	fake := &fakeGroups{directory: true, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	body := getAs(t, api, uid, "/groups/"+testGroupID).Body.String()
	if !strings.Contains(body, "Local One") {
		t.Error("a PowerDev cannot see the members")
	}
	for _, hidden := range []string{`/members/remove"`, `action="/groups/` + testGroupID + `/members"`, "Add a local account", "New Local"} {
		if strings.Contains(body, hidden) {
			t.Errorf("a PowerDev is offered %q", hidden)
		}
	}

	// And the server refuses it even if they post the form by hand.
	rec := postFormAs(t, api, uid, "/groups/"+testGroupID+"/members", url.Values{"user_id": {"u-new"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST members as PowerDev = %d, want 403", rec.Code)
	}
}

func TestGroupPage_NoDirectoryHidesTheSearch(t *testing.T) {
	fake := &fakeGroups{directory: false, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(false)}}
	api, uid := groupsAPI(t, db.TierAdmin, fake)

	body := getAs(t, api, uid, "/groups/"+testGroupID).Body.String()
	if strings.Contains(body, "ad-search") || strings.Contains(body, "Map an AD group") {
		t.Error("the search is offered with no directory")
	}
	if !strings.Contains(body, "No AD directory is configured") {
		t.Error("the page does not say why there is no search")
	}
}

func TestGroupPage_UnknownGroupIs404AndNoticesAreAFixedSet(t *testing.T) {
	fake := &fakeGroups{directory: true, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	if rec := getAs(t, api, uid, "/groups/00000000-0000-4000-8000-000000000000"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown group = %d, want 404", rec.Code)
	}

	for query, want := range map[string]string{
		"notice=created":                "Group created.",
		"notice=renamed":                "Group renamed.",
		"notice=unmapped":               "AD group unmapped.",
		"notice=member_added":           "Member added.",
		"notice=mapped&matched=3":       "3 of its members have already signed in",
		"notice=mapped_late":            "member list could not be read",
		"notice=mapped&matched=notanum": "AD group mapped.",
	} {
		body := getAs(t, api, uid, "/groups/"+testGroupID+"?"+query).Body.String()
		if !strings.Contains(body, `class="form-success"`) || !strings.Contains(body, want) {
			t.Errorf("?%s: page is missing the notice %q", query, want)
		}
	}
	// A crafted URL cannot put its own words on the page.
	body := getAs(t, api, uid, "/groups/"+testGroupID+"?notice="+url.QueryEscape("<script>alert(1)</script>")).Body.String()
	if strings.Contains(body, "alert(1)") || strings.Contains(body, `class="form-success"`) {
		t.Error("an unknown notice code produced a banner")
	}
}

func TestCreateGroup(t *testing.T) {
	fake := &fakeGroups{}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	rec := postFormAs(t, api, uid, "/groups", url.Values{"name": {"Data Analytics"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/groups/"+testGroupID+"?notice=created" {
		t.Errorf("success: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(fake.calls) != 1 || fake.calls[0] != "create:Data Analytics" {
		t.Errorf("calls = %v", fake.calls)
	}

	cases := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"taken", groups.ErrNameTaken, http.StatusConflict, "A group with that name already exists."},
		{"invalid", fmt.Errorf("%w: a name is required", groups.ErrInvalidName), http.StatusBadRequest, "not a valid group name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeGroups{err: c.err}
			api, uid := groupsAPI(t, db.TierPowerDev, f)
			rec := postFormAs(t, api, uid, "/groups", url.Values{"name": {"Typed Name"}})
			body := rec.Body.String()
			if rec.Code != c.status || !strings.Contains(body, c.text) || !strings.Contains(body, `value="Typed Name"`) {
				t.Errorf("status %d; the message or the refilled name is missing: %.300s", rec.Code, body)
			}
		})
	}

	// An unexpected failure is a 500 that does not leak its detail.
	f := &fakeGroups{err: errors.New("pq: secret detail")}
	api2, uid2 := groupsAPI(t, db.TierPowerDev, f)
	rec = postFormAs(t, api2, uid2, "/groups", url.Values{"name": {"x"}})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret detail") {
		t.Errorf("internal failure: %d %q", rec.Code, rec.Body.String())
	}
}

func TestGroupForms_RedirectWithANotice(t *testing.T) {
	cases := []struct {
		path     string
		form     url.Values
		wantCall string
		wantLoc  string
	}{
		{"/rename", url.Values{"name": {"New Name"}}, "rename:" + testGroupID + ":New Name", "?notice=renamed"},
		{"/ad-groups/remove", url.Values{"dn": {alphaDN}}, "unmap:" + testGroupID + ":" + alphaDN, "?notice=unmapped"},
		{"/members", url.Values{"user_id": {"u-new"}}, "addmember:" + testGroupID + ":u-new", "?notice=member_added"},
		{"/members/remove", url.Values{"user_id": {"u-loc"}}, "removemember:" + testGroupID + ":u-loc", "?notice=member_removed"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			fake := &fakeGroups{directory: true, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
			api, uid := groupsAPI(t, db.TierAdmin, fake)
			rec := postFormAs(t, api, uid, "/groups/"+testGroupID+c.path, c.form)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/groups/"+testGroupID+c.wantLoc {
				t.Errorf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
			}
			if len(fake.calls) != 1 || fake.calls[0] != c.wantCall {
				t.Errorf("calls = %v, want [%s]", fake.calls, c.wantCall)
			}
		})
	}
}

func TestMapADGroup_RedirectsWithTheMatchCountOrTheLateNotice(t *testing.T) {
	post := func(res *groups.MapResult) *httptest.ResponseRecorder {
		fake := &fakeGroups{directory: true, mapResult: res, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
		api, uid := groupsAPI(t, db.TierPowerDev, fake)
		return postFormAs(t, api, uid, "/groups/"+testGroupID+"/ad-groups", url.Values{"dn": {alphaDN}})
	}
	if rec := post(&groups.MapResult{MatchedMembers: 4}); rec.Header().Get("Location") != "/groups/"+testGroupID+"?matched=4&notice=mapped" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	if rec := post(&groups.MapResult{BackfillFailed: true}); rec.Header().Get("Location") != "/groups/"+testGroupID+"?notice=mapped_late" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
}

func TestGroupForms_RefusalsRedrawThePageWithTheirStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		path   string
		form   url.Values
		status int
		text   string
	}{
		{"rename taken", groups.ErrNameTaken, "/rename", url.Values{"name": {"Taken"}}, http.StatusConflict, "already exists"},
		{"rename invalid", groups.ErrInvalidName, "/rename", url.Values{"name": {""}}, http.StatusBadRequest, "not a valid group name"},
		{"map: not in the directory", groups.ErrADGroupNotFound, "/ad-groups", url.Values{"dn": {"CN=Nope"}}, http.StatusBadRequest, "not found in the directory"},
		{"map: already mapped", groups.ErrAlreadyMapped, "/ad-groups", url.Values{"dn": {alphaDN}}, http.StatusConflict, "already mapped"},
		{"map: no directory", groups.ErrDirectoryUnavailable, "/ad-groups", url.Values{"dn": {alphaDN}}, http.StatusConflict, "No directory is configured"},
		{"map: directory down", fmt.Errorf("%w: ldap secret detail", groups.ErrDirectory), "/ad-groups", url.Values{"dn": {alphaDN}}, http.StatusBadGateway, "could not be reached"},
		{"add: an AD account", groups.ErrNotLocalAccount, "/members", url.Values{"user_id": {"u-ad"}}, http.StatusBadRequest, "Only local accounts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeGroups{directory: true, err: c.err, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
			api, uid := groupsAPI(t, db.TierAdmin, fake)
			rec := postFormAs(t, api, uid, "/groups/"+testGroupID+c.path, c.form)
			body := rec.Body.String()
			if rec.Code != c.status || !strings.Contains(body, c.text) || !strings.Contains(body, "Data Analytics") {
				t.Errorf("status %d; want %d with %q on the redrawn page: %.300s", rec.Code, c.status, c.text, body)
			}
			if strings.Contains(body, "secret detail") {
				t.Error("an internal detail leaked into the page")
			}
		})
	}

	// A failed rename refills the form with what was typed.
	fake := &fakeGroups{err: groups.ErrNameTaken, detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/groups/"+testGroupID+"/rename", url.Values{"name": {"Typed Again"}})
	if !strings.Contains(rec.Body.String(), `value="Typed Again"`) {
		t.Error("the rename form was not refilled")
	}

	// An unknown group is a 404.
	fake = &fakeGroups{err: groups.ErrNotFound}
	api, uid = groupsAPI(t, db.TierAdmin, fake)
	if rec := postFormAs(t, api, uid, "/groups/"+testGroupID+"/rename", url.Values{"name": {"x"}}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown group = %d, want 404", rec.Code)
	}
}

func TestDeleteGroup_NeedsTheConfirmBox(t *testing.T) {
	fake := &fakeGroups{detail: map[string]*groups.Detail{testGroupID: sampleGroupDetail(true)}}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	rec := postFormAs(t, api, uid, "/groups/"+testGroupID+"/delete", url.Values{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Tick the box") {
		t.Errorf("without confirm: %d %.200s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 0 {
		t.Errorf("a delete without confirmation reached the service: %v", fake.calls)
	}

	rec = postFormAs(t, api, uid, "/groups/"+testGroupID+"/delete", url.Values{"confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/groups" {
		t.Errorf("with confirm: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(fake.calls) != 1 || fake.calls[0] != "delete:"+testGroupID {
		t.Errorf("calls = %v", fake.calls)
	}

	fake.err = groups.ErrNotFound
	if rec := postFormAs(t, api, uid, "/groups/"+testGroupID+"/delete", url.Values{"confirm": {"yes"}}); rec.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown group = %d, want 404", rec.Code)
	}
}

func TestADSearchFragment(t *testing.T) {
	fake := &fakeGroups{directory: true, searchResult: &groups.SearchResult{
		Candidates: []groups.Candidate{
			{DirectoryGroup: auth.DirectoryGroup{DN: alphaDN, Name: "Team-Alpha", Description: "Alpha <i>people</i>"}, Mapped: true},
			{DirectoryGroup: auth.DirectoryGroup{DN: `CN=Team "Bravo",OU=Groups,DC=example,DC=internal`, Name: "Team-Bravo"}},
		},
		Truncated: true,
	}}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)

	rec := getAs(t, api, uid, "/groups/"+testGroupID+"/ad-search?q=team")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("the search answered with a whole page, not a fragment")
	}
	for _, want := range []string{
		"Team-Alpha", "Already mapped", "Team-Bravo",
		`action="/groups/` + testGroupID + `/ad-groups"`, `name="csrf_token"`,
		`value="CN=Team &#34;Bravo&#34;,OU=Groups,DC=example,DC=internal"`, // the DN is escaped as an attribute value
		"Alpha &lt;i&gt;people&lt;/i&gt;", "More groups matched than are shown",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment is missing %q: %s", want, body)
		}
	}
	// Only the unmapped group gets a Map button.
	if n := strings.Count(body, `<button type="submit">Map</button>`); n != 1 {
		t.Errorf("%d Map buttons, want 1", n)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "search:"+testGroupID+":team" {
		t.Errorf("calls = %v", fake.calls)
	}
}

func TestADSearchFragment_MessagesAreHintsNotErrorStatuses(t *testing.T) {
	cases := []struct {
		name string
		term string
		err  error
		res  *groups.SearchResult
		want string
	}{
		{"too short", "ab", groups.ErrInvalidSearch, nil, "at least 3 characters"},
		{"empty box says nothing", "", groups.ErrInvalidSearch, nil, ""},
		{"no directory", "team", groups.ErrDirectoryUnavailable, nil, "No directory is configured"},
		{"directory down", "team", fmt.Errorf("%w: secret detail", groups.ErrDirectory), nil, "could not be searched just now"},
		{"no matches", "zzz", nil, &groups.SearchResult{}, "name contains that"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeGroups{directory: true, searchErr: c.err, searchResult: c.res}
			api, uid := groupsAPI(t, db.TierPowerDev, fake)
			rec := getAs(t, api, uid, "/groups/"+testGroupID+"/ad-search?q="+c.term)
			body := rec.Body.String()
			// htmx does not swap a non-2xx response in, so these must be 200s.
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if c.want == "" && strings.Contains(body, "ad-results-message") {
				t.Errorf("an empty box showed a message: %s", body)
			}
			if c.want != "" && !strings.Contains(body, c.want) {
				t.Errorf("fragment is missing %q: %s", c.want, body)
			}
			if strings.Contains(body, "secret detail") {
				t.Error("an internal detail leaked into the fragment")
			}
		})
	}

	fake := &fakeGroups{searchErr: groups.ErrNotFound}
	api, uid := groupsAPI(t, db.TierPowerDev, fake)
	if rec := getAs(t, api, uid, "/groups/"+testGroupID+"/ad-search?q=team"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown group = %d, want 404", rec.Code)
	}
}

func TestGroupPosts_RequireASessionAndCSRF(t *testing.T) {
	fake := &fakeGroups{}
	api, uid := groupsAPI(t, db.TierAdmin, fake)

	targets := []string{"/groups", "/groups/" + testGroupID + "/rename", "/groups/" + testGroupID + "/delete", "/groups/" + testGroupID + "/ad-groups", "/groups/" + testGroupID + "/ad-groups/remove", "/groups/" + testGroupID + "/members", "/groups/" + testGroupID + "/members/remove"}
	for _, target := range targets {
		// A session, but no CSRF token. The form content type is what a real
		// <form> sends, and what RequireCSRF checks.
		req := newAuthenticatedRequest(t, http.MethodPost, target, uid)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF = %d, want 403", target, rec.Code)
		}

		// No session at all.
		req = httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		addValidCSRF(req)
		rec = httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK || rec.Code == http.StatusSeeOther {
			t.Errorf("POST %s without a session = %d", target, rec.Code)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("a request without CSRF or a session reached the service: %v", fake.calls)
	}
}

// refresherFake records the membership refreshes LoginService asks for.
type refresherFake struct{ calls []string }

func (r *refresherFake) RefreshUser(_ context.Context, userID, dn string) {
	r.calls = append(r.calls, userID+"|"+dn)
}

func TestLoginService_RefreshesMembershipsAfterALogin(t *testing.T) {
	dn := "CN=jsmith,DC=example,DC=internal"
	newSvc := func(inGroup bool, err error) (*LoginService, *refresherFake, *fakeUserStore) {
		idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{ADSID: "S-1-5-21-1", DisplayName: "J", InAccessGroup: inGroup, DN: dn}, err: err}
		store := newFakeUserStore()
		ref := &refresherFake{}
		return NewLoginService(idp, store, testSessionSecret).WithMembershipRefresher(ref), ref, store
	}

	// A new user and a returning one both get a refresh, with their own id and DN.
	svc, ref, store := newSvc(true, nil)
	user, _, err := svc.Login(context.Background(), "jsmith", "pw")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if len(ref.calls) != 1 || ref.calls[0] != user.ID+"|"+dn {
		t.Errorf("new user: refresh calls = %v", ref.calls)
	}
	store.byADSID["S-1-5-21-1"] = user
	if _, _, err := svc.Login(context.Background(), "jsmith", "pw"); err != nil || len(ref.calls) != 2 {
		t.Errorf("returning user: err %v, calls %v", err, ref.calls)
	}

	// Nobody who is refused a login is looked up: not outside the access group,
	// not with bad credentials.
	svc, ref, _ = newSvc(false, nil)
	if _, _, err := svc.Login(context.Background(), "jsmith", "pw"); !errors.Is(err, ErrAccessDenied) || len(ref.calls) != 0 {
		t.Errorf("outside the access group: %v, calls %v", err, ref.calls)
	}
	svc, ref, _ = newSvc(true, auth.ErrInvalidCredentials)
	if _, _, err := svc.Login(context.Background(), "jsmith", "pw"); err == nil || len(ref.calls) != 0 {
		t.Errorf("bad credentials: %v, calls %v", err, ref.calls)
	}

	// Without group support wired in, login works exactly as before.
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{ADSID: "S-1-5-21-2", DisplayName: "K", InAccessGroup: true, DN: dn}}
	if _, _, err := NewLoginService(idp, newFakeUserStore(), testSessionSecret).Login(context.Background(), "k", "pw"); err != nil {
		t.Errorf("Login() with no refresher: %v", err)
	}
}

func TestRequireSession_RecheckRefreshesMembershipsOnlyForAPersonStillInTheAccessGroup(t *testing.T) {
	dn := "CN=stillmember,DC=example,DC=internal"
	run := func(idp *fakeIdentityProvider) (*refresherFake, int) {
		users := newFakeUserLister()
		users.byID["u-stale"] = &db.User{ID: "u-stale", LDAPDN: &dn}
		ref := &refresherFake{}
		svc := NewLoginService(idp, newFakeUserStore(), testSessionSecret).WithMembershipRefresher(ref)
		api, err := New(svc, NewLocalLoginService(newFakeUserStore(), testSessionSecret), NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret), newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, &fakeTransferInitiator{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeLocalAccountManager{}, &fakeSelfAccountManager{}, &fakeSettingsViewer{}, &fakeThemeSettingsReader{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, &fakeInventoryLister{}, &fakeSizeEstimator{}, &fakeContainerLogs{}, &fakeAlerts{}, &fakeGroups{}, testLogger())
		if err != nil {
			t.Fatalf("New() error: %v", err)
		}
		srv := httptest.NewServer(api.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })))
		defer srv.Close()

		stale := session.Session{UserID: "u-stale", ExpiresAt: time.Now().UTC().Add(sessionDuration), LastVerifiedAt: time.Now().UTC().Add(-2 * testAuthRecheckInterval)}
		cookieValue, err := session.Sign(testSessionSecret, stale)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("GET error: %v", err)
		}
		defer resp.Body.Close()
		return ref, resp.StatusCode
	}

	ref, status := run(&fakeIdentityProvider{isInAccessGroupResult: true})
	if status != http.StatusOK || len(ref.calls) != 1 || ref.calls[0] != "u-stale|"+dn {
		t.Errorf("still a member: status %d, refreshes %v", status, ref.calls)
	}

	ref, status = run(&fakeIdentityProvider{isInAccessGroupResult: false})
	if status == http.StatusOK || len(ref.calls) != 0 {
		t.Errorf("no longer a member: status %d, refreshes %v (they are signed out, not looked up)", status, ref.calls)
	}

	// A directory that cannot answer keeps the person signed in (the existing posture)
	// and does not pretend to know their groups.
	ref, status = run(&fakeIdentityProvider{isInAccessGroupErr: errors.New("ldap down")})
	if status != http.StatusOK || len(ref.calls) != 0 {
		t.Errorf("directory down: status %d, refreshes %v", status, ref.calls)
	}
}
