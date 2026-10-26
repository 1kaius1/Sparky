// SPDX-License-Identifier: AGPL-3.0-or-later

package groups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	alphaDN = "CN=Team-Alpha,OU=Groups,DC=example,DC=internal"
	bravoDN = "CN=Team-Bravo,OU=Groups,DC=example,DC=internal"
)

// fakeStore is an in-memory *db.AppGroupRepository: it models names unique
// ignoring case, mappings, manual members and the membership cache, because
// the service's rules depend on them.
type fakeStore struct {
	next     int
	groups   map[string]*db.AppGroup
	mappings map[string][]db.AppGroupADMapping // by group id
	manual   map[string]map[string]bool        // group id -> user id
	cache    map[string][]string               // user id -> lower-cased AD group DNs
	pruned   int
	failDel  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		groups: map[string]*db.AppGroup{}, mappings: map[string][]db.AppGroupADMapping{},
		manual: map[string]map[string]bool{}, cache: map[string][]string{},
	}
}

func (f *fakeStore) Create(_ context.Context, name string, by *string) (*db.AppGroup, error) {
	for _, g := range f.groups {
		if strings.EqualFold(g.Name, name) {
			return nil, db.ErrAppGroupNameTaken
		}
	}
	f.next++
	g := &db.AppGroup{ID: fmt.Sprintf("g-%d", f.next), Name: name, CreatedBy: by, UpdatedBy: by}
	f.groups[g.ID] = g
	return g, nil
}

func (f *fakeStore) FindByID(_ context.Context, id string) (*db.AppGroup, error) {
	g, ok := f.groups[id]
	if !ok {
		return nil, db.ErrAppGroupNotFound
	}
	cp := *g
	return &cp, nil
}

func (f *fakeStore) Rename(_ context.Context, id, name string, by *string) error {
	g, ok := f.groups[id]
	if !ok {
		return db.ErrAppGroupNotFound
	}
	for _, o := range f.groups {
		if o.ID != id && strings.EqualFold(o.Name, name) {
			return db.ErrAppGroupNameTaken
		}
	}
	g.Name, g.UpdatedBy = name, by
	return nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	if f.failDel != nil {
		return f.failDel
	}
	if _, ok := f.groups[id]; !ok {
		return db.ErrAppGroupNotFound
	}
	delete(f.groups, id)
	delete(f.mappings, id)
	delete(f.manual, id)
	return nil
}

func (f *fakeStore) ListWithCounts(context.Context) ([]*db.AppGroupSummary, error) {
	var out []*db.AppGroupSummary
	for _, g := range f.groups {
		out = append(out, &db.AppGroupSummary{AppGroup: *g, ADGroupCount: len(f.mappings[g.ID])})
	}
	return out, nil
}

func (f *fakeStore) AddADMapping(_ context.Context, gid, dn, name string, _ *string) error {
	if _, ok := f.groups[gid]; !ok {
		return db.ErrAppGroupNotFound
	}
	for _, m := range f.mappings[gid] {
		if strings.EqualFold(m.DN, dn) {
			return db.ErrADGroupAlreadyMapped
		}
	}
	f.mappings[gid] = append(f.mappings[gid], db.AppGroupADMapping{DN: dn, Name: name})
	return nil
}

func (f *fakeStore) RemoveADMapping(_ context.Context, gid, dn string) (bool, error) {
	list := f.mappings[gid]
	for i, m := range list {
		if strings.EqualFold(m.DN, dn) {
			f.mappings[gid] = append(list[:i:i], list[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) ListADMappings(_ context.Context, gid string) ([]db.AppGroupADMapping, error) {
	return append([]db.AppGroupADMapping(nil), f.mappings[gid]...), nil
}

func (f *fakeStore) MappedDNs(context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, list := range f.mappings {
		for _, m := range list {
			if !seen[strings.ToLower(m.DN)] {
				seen[strings.ToLower(m.DN)] = true
				out = append(out, m.DN)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) AddLocalMember(_ context.Context, gid, uid string, _ *string) (bool, error) {
	if _, ok := f.groups[gid]; !ok {
		return false, db.ErrAppGroupNotFound
	}
	if f.manual[gid] == nil {
		f.manual[gid] = map[string]bool{}
	}
	if f.manual[gid][uid] {
		return false, nil
	}
	f.manual[gid][uid] = true
	return true, nil
}

func (f *fakeStore) RemoveLocalMember(_ context.Context, gid, uid string) (bool, error) {
	if !f.manual[gid][uid] {
		return false, nil
	}
	delete(f.manual[gid], uid)
	return true, nil
}

func (f *fakeStore) Members(_ context.Context, gid string) ([]db.AppGroupMember, error) {
	var out []db.AppGroupMember
	for uid := range f.manual[gid] {
		out = append(out, db.AppGroupMember{UserID: uid, AddedManually: true})
	}
	return out, nil
}

func (f *fakeStore) ReplaceUserMemberships(_ context.Context, uid string, dns []string) error {
	f.cache[uid] = nil
	for _, d := range dns {
		f.cache[uid] = append(f.cache[uid], strings.ToLower(d))
	}
	return nil
}

func (f *fakeStore) ReplaceMembersOfDN(_ context.Context, dn string, uids []string) error {
	l := strings.ToLower(dn)
	for uid, dns := range f.cache {
		var kept []string
		for _, d := range dns {
			if d != l {
				kept = append(kept, d)
			}
		}
		f.cache[uid] = kept
	}
	for _, uid := range uids {
		f.cache[uid] = append(f.cache[uid], l)
	}
	return nil
}

func (f *fakeStore) PruneUnmappedMemberships(ctx context.Context) (int64, error) {
	f.pruned++
	mapped := map[string]bool{}
	dns, _ := f.MappedDNs(ctx)
	for _, d := range dns {
		mapped[strings.ToLower(d)] = true
	}
	for uid, list := range f.cache {
		var kept []string
		for _, d := range list {
			if mapped[d] {
				kept = append(kept, d)
			}
		}
		f.cache[uid] = kept
	}
	return 0, nil
}

type fakeUsers struct {
	byID      map[string]*db.User
	idsByDN   map[string]string // lower dn -> id
	findIDErr error
}

func (f *fakeUsers) FindByID(_ context.Context, id string) (*db.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, db.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeUsers) List(context.Context) ([]*db.User, error) {
	var out []*db.User
	for _, u := range f.byID {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUsers) FindIDsByLDAPDNs(_ context.Context, dns []string) ([]string, error) {
	if f.findIDErr != nil {
		return nil, f.findIDErr
	}
	var out []string
	for _, d := range dns {
		if id, ok := f.idsByDN[strings.ToLower(d)]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// fakeDirectory answers the GroupDirectory calls from a small configured world.
type fakeDirectory struct {
	groups  map[string]auth.DirectoryGroup // lower dn -> canonical record
	members map[string][]string            // lower group dn -> member user DNs
	userIn  map[string][]string            // lower user dn -> group DNs the person is in
	search  *auth.GroupSearchResult
	err     error
	memErr  error

	memberOfCalls int
	searchedFor   string
}

func (f *fakeDirectory) SearchGroups(_ context.Context, term string, _ int) (*auth.GroupSearchResult, error) {
	f.searchedFor = term
	if f.err != nil {
		return nil, f.err
	}
	return f.search, nil
}

func (f *fakeDirectory) FindGroup(_ context.Context, dn string) (*auth.DirectoryGroup, error) {
	if f.err != nil {
		return nil, f.err
	}
	g, ok := f.groups[strings.ToLower(dn)]
	if !ok {
		return nil, auth.ErrGroupNotFound
	}
	return &g, nil
}

func (f *fakeDirectory) GroupMemberDNs(_ context.Context, dn string) ([]string, error) {
	if f.memErr != nil {
		return nil, f.memErr
	}
	return f.members[strings.ToLower(dn)], nil
}

func (f *fakeDirectory) MemberOf(_ context.Context, userDN string, groupDNs []string) ([]string, error) {
	f.memberOfCalls++
	if f.err != nil {
		return nil, f.err
	}
	asked := map[string]bool{}
	for _, g := range groupDNs {
		asked[strings.ToLower(g)] = true
	}
	var out []string
	for _, g := range f.userIn[strings.ToLower(userDN)] {
		if asked[strings.ToLower(g)] {
			out = append(out, g)
		}
	}
	return out, nil
}

type auditEntry struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	objectID   string
	detail     map[string]any
}

type fakeAudit struct {
	entries []auditEntry
	fail    bool
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, superAdmin bool, action, objectType, objectID string, detail map[string]any) error {
	if f.fail {
		return errors.New("audit down")
	}
	f.entries = append(f.entries, auditEntry{actorID, superAdmin, action, objectType, objectID, detail})
	return nil
}

func (f *fakeAudit) last() auditEntry { return f.entries[len(f.entries)-1] }

type harness struct {
	svc   *Service
	store *fakeStore
	users *fakeUsers
	dir   *fakeDirectory
	aud   *fakeAudit
}

func newHarness() *harness {
	h := &harness{
		store: newFakeStore(),
		users: &fakeUsers{byID: map[string]*db.User{}, idsByDN: map[string]string{}},
		dir: &fakeDirectory{
			groups: map[string]auth.DirectoryGroup{
				strings.ToLower(alphaDN): {DN: alphaDN, Name: "Team-Alpha"},
				strings.ToLower(bravoDN): {DN: bravoDN, Name: "Team-Bravo"},
			},
			members: map[string][]string{}, userIn: map[string][]string{},
		},
		aud: &fakeAudit{},
	}
	h.svc = NewService(h.store, h.users, h.dir, h.aud, log.New(io.Discard, "", 0))
	return h
}

func (h *harness) addUser(id, name string, local bool, dn string) *db.User {
	u := &db.User{ID: id, DisplayName: name, Tier: db.TierDeveloper}
	if local {
		un := strings.ToLower(name)
		u.LocalUsername = &un
	} else {
		u.LDAPDN = &dn
		h.users.idsByDN[strings.ToLower(dn)] = id
	}
	h.users.byID[id] = u
	return u
}

func power(id string) rbac.Actor { return rbac.Actor{UserID: id, Tier: db.TierPowerDev} }
func admin(id string) rbac.Actor { return rbac.Actor{UserID: id, Tier: db.TierAdmin} }
func dev(id string) rbac.Actor   { return rbac.Actor{UserID: id, Tier: db.TierDeveloper} }

var superAdmin = rbac.Actor{IsSuperAdmin: true}

func (h *harness) group(t *testing.T, name string) *db.AppGroup {
	t.Helper()
	g, err := h.svc.Create(context.Background(), power("p"), name)
	if err != nil {
		t.Fatalf("Create(%q) error: %v", name, err)
	}
	h.aud.entries = nil
	return g
}

func TestEveryManagementCallNeedsPowerDevAndLocalMembersNeedAdmin(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	h.addUser("l1", "Local One", true, "")

	for _, actor := range []rbac.Actor{dev("d"), {Tier: db.TierReadOnly, UserID: "r"}} {
		calls := map[string]func() error{
			"List":        func() error { _, err := h.svc.List(ctx, actor); return err },
			"Get":         func() error { _, err := h.svc.Get(ctx, actor, g.ID); return err },
			"Create":      func() error { _, err := h.svc.Create(ctx, actor, "X"); return err },
			"Rename":      func() error { return h.svc.Rename(ctx, actor, g.ID, "Y") },
			"Delete":      func() error { return h.svc.Delete(ctx, actor, g.ID) },
			"Search":      func() error { _, err := h.svc.SearchDirectory(ctx, actor, g.ID, "alpha"); return err },
			"Map":         func() error { _, err := h.svc.MapADGroup(ctx, actor, g.ID, alphaDN); return err },
			"Unmap":       func() error { return h.svc.UnmapADGroup(ctx, actor, g.ID, alphaDN) },
			"AddLocal":    func() error { return h.svc.AddLocalMember(ctx, actor, g.ID, "l1") },
			"RemoveLocal": func() error { return h.svc.RemoveLocalMember(ctx, actor, g.ID, "l1") },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, rbac.ErrNotPermitted) {
				t.Errorf("%s as %s: %v, want ErrNotPermitted", name, actor.Tier, err)
			}
		}
	}

	// PowerDev manages groups and AD mappings but not manual local members.
	p := power("p")
	if _, err := h.svc.MapADGroup(ctx, p, g.ID, alphaDN); err != nil {
		t.Errorf("PowerDev Map: %v", err)
	}
	if err := h.svc.AddLocalMember(ctx, p, g.ID, "l1"); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("PowerDev AddLocalMember: %v, want ErrNotPermitted", err)
	}
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Errorf("Admin AddLocalMember: %v", err)
	}
	if err := h.svc.AddLocalMember(ctx, superAdmin, g.ID, "l1"); err != nil {
		t.Errorf("SuperAdmin AddLocalMember (already a member): %v", err)
	}
}

func TestCreate_NameRulesAndAudit(t *testing.T) {
	ctx := context.Background()
	h := newHarness()

	g, err := h.svc.Create(ctx, power("p"), "  Data Analytics  ")
	if err != nil || g.Name != "Data Analytics" {
		t.Fatalf("Create() = %+v, %v; want the trimmed name", g, err)
	}
	e := h.aud.last()
	if e.action != "created_app_group" || e.objectType != "app_group" || e.objectID != g.ID || e.detail["name"] != "Data Analytics" || *e.actorID != "p" {
		t.Errorf("audit entry = %+v", e)
	}

	for name, bad := range map[string]string{
		"empty": "", "blank": "   ", "too long": strings.Repeat("x", MaxNameLen+1), "control char": "a\x00b", "newline": "a\nb",
	} {
		if _, err := h.svc.Create(ctx, power("p"), bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%s: %v, want ErrInvalidName", name, err)
		}
	}
	if _, err := h.svc.Create(ctx, power("p"), strings.Repeat("é", MaxNameLen)); err != nil {
		t.Errorf("a name of exactly %d characters (multi-byte) was refused: %v", MaxNameLen, err)
	}
	if _, err := h.svc.Create(ctx, power("p"), "data ANALYTICS"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a name differing only by case: %v, want ErrNameTaken", err)
	}

	// The break-glass SuperAdmin is audited as such, with no actor id.
	if _, err := h.svc.Create(ctx, superAdmin, "From break-glass"); err != nil {
		t.Fatal(err)
	}
	if e := h.aud.last(); !e.superAdmin || e.actorID != nil {
		t.Errorf("SuperAdmin audit entry = %+v", e)
	}
}

func TestCreate_AuditFailureRemovesTheGroup(t *testing.T) {
	h := newHarness()
	h.aud.fail = true
	if _, err := h.svc.Create(context.Background(), power("p"), "Ghost"); err == nil {
		t.Fatal("Create() succeeded with a failing audit")
	}
	if len(h.store.groups) != 0 {
		t.Error("the group stayed after its audit record failed")
	}
}

func TestRename(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	a, b := h.group(t, "Alpha"), h.group(t, "Bravo")

	if err := h.svc.Rename(ctx, power("p"), a.ID, "Alpha Prime"); err != nil {
		t.Fatalf("Rename() error: %v", err)
	}
	if e := h.aud.last(); e.action != "renamed_app_group" || e.detail["old_name"] != "Alpha" || e.detail["new_name"] != "Alpha Prime" {
		t.Errorf("audit entry = %+v", e)
	}
	n := len(h.aud.entries)
	if err := h.svc.Rename(ctx, power("p"), a.ID, "Alpha Prime"); err != nil || len(h.aud.entries) != n {
		t.Errorf("renaming to the same name: %v, new audit entries %d", err, len(h.aud.entries)-n)
	}
	if err := h.svc.Rename(ctx, power("p"), b.ID, "alpha prime"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a taken name: %v", err)
	}
	if err := h.svc.Rename(ctx, power("p"), "nope", "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown group: %v", err)
	}
	if err := h.svc.Rename(ctx, power("p"), a.ID, ""); !errors.Is(err, ErrInvalidName) {
		t.Errorf("an empty name: %v", err)
	}

	h.aud.fail = true
	if err := h.svc.Rename(ctx, power("p"), b.ID, "Bravo Prime"); err == nil {
		t.Fatal("Rename() succeeded with a failing audit")
	}
	if h.store.groups[b.ID].Name != "Bravo" {
		t.Errorf("the name stayed %q after its audit record failed", h.store.groups[b.ID].Name)
	}
}

func TestDelete_AuditsCountsAndForgetsTheCache(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	h.addUser("l1", "Local One", true, "")
	g := h.group(t, "Analytics")
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Fatal(err)
	}
	h.store.cache["u1"] = []string{strings.ToLower(alphaDN)}
	h.aud.entries = nil

	if err := h.svc.Delete(ctx, power("p"), g.ID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	e := h.aud.last()
	if e.action != "deleted_app_group" || e.detail["name"] != "Analytics" || e.detail["ad_groups"] != 1 || e.detail["manual_members"] != 1 {
		t.Errorf("audit entry = %+v", e)
	}
	if len(h.store.cache["u1"]) != 0 {
		t.Errorf("the membership cache outlived the only mapping: %v", h.store.cache["u1"])
	}
	if err := h.svc.Delete(ctx, power("p"), g.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestSearchDirectory(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err != nil {
		t.Fatal(err)
	}
	h.dir.search = &auth.GroupSearchResult{
		Groups:    []auth.DirectoryGroup{{DN: strings.ToUpper(alphaDN), Name: "Team-Alpha"}, {DN: bravoDN, Name: "Team-Bravo"}},
		Truncated: true,
	}

	got, err := h.svc.SearchDirectory(ctx, power("p"), g.ID, "  team  ")
	if err != nil {
		t.Fatalf("SearchDirectory() error: %v", err)
	}
	if h.dir.searchedFor != "team" {
		t.Errorf("searched for %q, want the trimmed term", h.dir.searchedFor)
	}
	if len(got.Candidates) != 2 || !got.Candidates[0].Mapped || got.Candidates[1].Mapped || !got.Truncated {
		t.Errorf("result = %+v (the first is already mapped, ignoring case; the list is truncated)", got)
	}
	if len(h.aud.entries) != 1 { // only the mapping from the setup; a search is a read
		t.Errorf("a search was audited: %d entries", len(h.aud.entries))
	}

	for name, term := range map[string]string{"empty": "", "too short": "ab", "blank": "    ", "too long": strings.Repeat("x", MaxSearchLen+1), "control char": "te\x00am"} {
		h.dir.searchedFor = ""
		if _, err := h.svc.SearchDirectory(ctx, power("p"), g.ID, term); !errors.Is(err, ErrInvalidSearch) {
			t.Errorf("%s: %v, want ErrInvalidSearch", name, err)
		}
		if h.dir.searchedFor != "" {
			t.Errorf("%s: the directory was asked anyway", name)
		}
	}
	if _, err := h.svc.SearchDirectory(ctx, power("p"), "nope", "team"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown group: %v", err)
	}
	h.dir.err = errors.New("ldap down")
	if _, err := h.svc.SearchDirectory(ctx, power("p"), g.ID, "team"); !errors.Is(err, ErrDirectory) || !strings.Contains(err.Error(), "ldap down") {
		t.Errorf("a directory failure: %v, want ErrDirectory carrying the detail for the log", err)
	}

	// No AD configured at all.
	h.svc.directory = nil
	if _, err := h.svc.SearchDirectory(ctx, power("p"), g.ID, "team"); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Errorf("no directory: %v", err)
	}
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Errorf("mapping with no directory: %v", err)
	}
	if h.svc.DirectoryAvailable() {
		t.Error("DirectoryAvailable() is true without a directory")
	}
}

func TestMapADGroup_StoresTheDirectorysRecordAndBackfills(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	ann := h.addUser("u-ann", "Ann", false, "CN=Ann,OU=Users,DC=example,DC=internal")
	h.addUser("u-never", "Never Logged In Here", false, "CN=Never,OU=Users,DC=example,DC=internal")
	h.dir.members[strings.ToLower(alphaDN)] = []string{*ann.LDAPDN, "CN=Stranger,OU=Users,DC=example,DC=internal"}

	// The browser posts the DN in the wrong case; the directory's spelling wins.
	res, err := h.svc.MapADGroup(ctx, power("p"), g.ID, strings.ToLower(alphaDN))
	if err != nil {
		t.Fatalf("MapADGroup() error: %v", err)
	}
	if res.Group.DN != alphaDN || res.Group.Name != "Team-Alpha" || res.MatchedMembers != 1 || res.BackfillFailed {
		t.Errorf("result = %+v", res)
	}
	if m := h.store.mappings[g.ID]; len(m) != 1 || m[0].DN != alphaDN || m[0].Name != "Team-Alpha" {
		t.Errorf("stored mapping = %+v, want the directory's own DN and name", m)
	}
	if got := h.store.cache["u-ann"]; len(got) != 1 || got[0] != strings.ToLower(alphaDN) {
		t.Errorf("Ann's cached memberships = %v, want the new group at once", got)
	}
	if _, cached := h.store.cache["u-never"]; cached {
		t.Error("a user who is not in the group was cached as a member")
	}
	e := h.aud.last()
	if e.action != "mapped_ad_group" || e.detail["ad_group_dn"] != alphaDN || e.detail["matched_members"] != 1 || e.detail["group_name"] != "Analytics" {
		t.Errorf("audit entry = %+v", e)
	}
}

func TestMapADGroup_Refusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")

	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, "CN=Nope,DC=example,DC=internal"); !errors.Is(err, ErrADGroupNotFound) {
		t.Errorf("a group the directory does not have: %v", err)
	}
	if len(h.store.mappings[g.ID]) != 0 || len(h.aud.entries) != 0 {
		t.Error("a refused mapping left something behind")
	}
	if _, err := h.svc.MapADGroup(ctx, power("p"), "nope", alphaDN); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown application group: %v", err)
	}
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, strings.ToUpper(alphaDN)); !errors.Is(err, ErrAlreadyMapped) {
		t.Errorf("mapping twice: %v", err)
	}
	h.dir.err = errors.New("ldap down")
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, bravoDN); !errors.Is(err, ErrDirectory) || errors.Is(err, ErrADGroupNotFound) {
		t.Errorf("a directory failure must be ErrDirectory, not 'group not found': %v", err)
	}
}

func TestMapADGroup_KeepsTheMappingWhenTheBackfillFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	h.dir.memErr = errors.New("size limit")

	res, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN)
	if err != nil {
		t.Fatalf("MapADGroup() error: %v", err)
	}
	if !res.BackfillFailed || res.MatchedMembers != 0 || len(h.store.mappings[g.ID]) != 1 {
		t.Errorf("result = %+v, mappings = %v", res, h.store.mappings[g.ID])
	}
	if e := h.aud.last(); e.detail["backfill_failed"] != true {
		t.Errorf("the audit entry does not say the backfill failed: %+v", e)
	}
}

func TestMapADGroup_AuditFailureUnmapsAndForgets(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	ann := h.addUser("u-ann", "Ann", false, "CN=Ann,OU=Users,DC=example,DC=internal")
	h.dir.members[strings.ToLower(alphaDN)] = []string{*ann.LDAPDN}
	h.aud.fail = true

	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err == nil {
		t.Fatal("MapADGroup() succeeded with a failing audit")
	}
	if len(h.store.mappings[g.ID]) != 0 || len(h.store.cache["u-ann"]) != 0 {
		t.Errorf("mapping or cache stayed: %v / %v", h.store.mappings[g.ID], h.store.cache["u-ann"])
	}
}

func TestUnmapADGroup(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err != nil {
		t.Fatal(err)
	}
	h.store.cache["u1"] = []string{strings.ToLower(alphaDN)}
	h.aud.entries = nil

	if err := h.svc.UnmapADGroup(ctx, power("p"), g.ID, strings.ToUpper(alphaDN)); err != nil {
		t.Fatalf("UnmapADGroup() error: %v", err)
	}
	if e := h.aud.last(); e.action != "unmapped_ad_group" || e.detail["ad_group_name"] != "Team-Alpha" {
		t.Errorf("audit entry = %+v", e)
	}
	if len(h.store.cache["u1"]) != 0 {
		t.Errorf("the cache kept a group nobody maps: %v", h.store.cache["u1"])
	}
	if err := h.svc.UnmapADGroup(ctx, power("p"), g.ID, alphaDN); !errors.Is(err, ErrNotFound) {
		t.Errorf("unmapping what is not mapped: %v", err)
	}

	// A failed audit maps it back.
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, bravoDN); err != nil {
		t.Fatal(err)
	}
	h.aud.fail = true
	if err := h.svc.UnmapADGroup(ctx, power("p"), g.ID, bravoDN); err == nil {
		t.Fatal("UnmapADGroup() succeeded with a failing audit")
	}
	if m := h.store.mappings[g.ID]; len(m) != 1 || m[0].Name != "Team-Bravo" {
		t.Errorf("the mapping was not restored: %v", m)
	}
}

func TestAddAndRemoveLocalMember(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	h.addUser("l1", "Local One", true, "")
	h.addUser("ad1", "AD One", false, "CN=AD1,OU=Users,DC=example,DC=internal")

	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Fatalf("AddLocalMember() error: %v", err)
	}
	if e := h.aud.last(); e.action != "added_group_member" || e.detail["user"] != "Local One" || e.detail["group_name"] != "Analytics" {
		t.Errorf("audit entry = %+v", e)
	}
	n := len(h.aud.entries)
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil || len(h.aud.entries) != n {
		t.Errorf("adding twice: %v, new audit entries %d", err, len(h.aud.entries)-n)
	}
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "ad1"); !errors.Is(err, ErrNotLocalAccount) {
		t.Errorf("an AD account: %v, want ErrNotLocalAccount", err)
	}
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown user: %v", err)
	}
	if err := h.svc.AddLocalMember(ctx, admin("a"), "nope", "l1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown group: %v", err)
	}

	if err := h.svc.RemoveLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Fatalf("RemoveLocalMember() error: %v", err)
	}
	if e := h.aud.last(); e.action != "removed_group_member" || e.detail["user"] != "Local One" {
		t.Errorf("audit entry = %+v", e)
	}
	n = len(h.aud.entries)
	if err := h.svc.RemoveLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil || len(h.aud.entries) != n {
		t.Errorf("removing someone who is not a member: %v, new audit entries %d", err, len(h.aud.entries)-n)
	}
}

func TestLocalMemberChangesAreRevertedWhenTheAuditFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	h.addUser("l1", "Local One", true, "")

	h.aud.fail = true
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err == nil {
		t.Fatal("AddLocalMember() succeeded with a failing audit")
	}
	if h.store.manual[g.ID]["l1"] {
		t.Error("the member stayed after the audit failed")
	}

	h.aud.fail = false
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Fatal(err)
	}
	h.aud.fail = true
	if err := h.svc.RemoveLocalMember(ctx, admin("a"), g.ID, "l1"); err == nil {
		t.Fatal("RemoveLocalMember() succeeded with a failing audit")
	}
	if !h.store.manual[g.ID]["l1"] {
		t.Error("the member was not put back after the audit failed")
	}
}

func TestGet_OffersOnlyAddableLocalAccountsToAdmins(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	h.addUser("l1", "Local One", true, "")
	h.addUser("l2", "Local Two", true, "")
	h.addUser("ad1", "AD One", false, "CN=AD1,OU=Users,DC=example,DC=internal")
	if err := h.svc.AddLocalMember(ctx, admin("a"), g.ID, "l1"); err != nil {
		t.Fatal(err)
	}

	d, err := h.svc.Get(ctx, admin("a"), g.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if !d.CanManageLocalMembers || !d.DirectoryAvailable || len(d.AddableLocalUsers) != 1 || d.AddableLocalUsers[0].ID != "l2" {
		t.Errorf("admin view: %+v (addable %v)", d, d.AddableLocalUsers)
	}

	d, err = h.svc.Get(ctx, power("p"), g.ID)
	if err != nil || d.CanManageLocalMembers || d.AddableLocalUsers != nil {
		t.Errorf("PowerDev view: %+v, %v; it must not be offered local accounts to add", d, err)
	}
	if _, err := h.svc.Get(ctx, power("p"), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown group: %v", err)
	}
}

func TestRefreshUser(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	g := h.group(t, "Analytics")
	annDN := "CN=Ann,OU=Users,DC=example,DC=internal"
	h.dir.userIn[strings.ToLower(annDN)] = []string{alphaDN, "CN=Unrelated,DC=example,DC=internal"}

	// Nothing is mapped: no directory query at all, and the cache is cleared.
	h.store.cache["u-ann"] = []string{"stale"}
	h.svc.RefreshUser(ctx, "u-ann", annDN)
	if h.dir.memberOfCalls != 0 || len(h.store.cache["u-ann"]) != 0 {
		t.Errorf("nothing mapped: queries=%d cache=%v", h.dir.memberOfCalls, h.store.cache["u-ann"])
	}

	// Mapped groups: only those are asked about and cached.
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, alphaDN); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.MapADGroup(ctx, power("p"), g.ID, bravoDN); err != nil {
		t.Fatal(err)
	}
	h.svc.RefreshUser(ctx, "u-ann", annDN)
	if got := h.store.cache["u-ann"]; len(got) != 1 || got[0] != strings.ToLower(alphaDN) {
		t.Errorf("cache = %v, want only the mapped group she is in", got)
	}

	// A directory that cannot be reached leaves the cache as it was.
	h.dir.err = errors.New("ldap down")
	h.svc.RefreshUser(ctx, "u-ann", annDN)
	if got := h.store.cache["u-ann"]; len(got) != 1 {
		t.Errorf("a directory failure changed the cache: %v", got)
	}

	// No DN yet, or no directory: nothing happens.
	h.dir.err = nil
	calls := h.dir.memberOfCalls
	h.svc.RefreshUser(ctx, "u-ann", "")
	h.svc.directory = nil
	h.svc.RefreshUser(ctx, "u-ann", annDN)
	if h.dir.memberOfCalls != calls {
		t.Error("a refresh without a DN or without a directory still queried")
	}
}
