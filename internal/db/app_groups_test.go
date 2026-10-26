// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// newTestGroup creates a group and deletes it (with its mappings, manual
// members and alert audiences) when the test ends.
func newTestGroup(t *testing.T, repo *AppGroupRepository, name string) *AppGroup {
	t.Helper()
	g, err := repo.Create(context.Background(), name, nil)
	if err != nil {
		t.Fatalf("Create(%q) error: %v", name, err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), g.ID) })
	return g
}

// newTestADUser creates a user who has "signed in" with the given LDAP DN, so
// the membership cache and the DN matching have someone to point at.
func newTestADUser(t *testing.T, users *UserRepository, label, dn string) *User {
	t.Helper()
	u := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-%s", t.Name(), label))
	if err := users.UpdateLastLogin(context.Background(), u.ID, dn, time.Now()); err != nil {
		t.Fatalf("set LDAP DN: %v", err)
	}
	return u
}

func newTestLocalUser(t *testing.T, users *UserRepository, label string) *User {
	t.Helper()
	name := fmt.Sprintf("local-%s-%d", label, time.Now().UnixNano())
	u, err := users.CreateLocal(context.Background(), name, "hash", "Local "+label, TierDeveloper)
	if err != nil {
		t.Fatalf("CreateLocal() error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = users.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

// uniqueName makes a group name that cannot collide with another test's or a
// previous run's, and stays within the 64-character limit.
func uniqueName(_ *testing.T, label string) string {
	return fmt.Sprintf("%s %d", label, time.Now().UnixNano())
}

func TestAppGroupRepository_NamesAreUniqueIgnoringCaseAndRenamable(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAppGroupRepository(pool)
	ctx := context.Background()

	name := uniqueName(t, "Data Analytics")
	g := newTestGroup(t, repo, name)
	if _, err := repo.Create(ctx, strings.ToUpper(name), nil); !errors.Is(err, ErrAppGroupNameTaken) {
		t.Errorf("a name differing only by case: %v, want ErrAppGroupNameTaken", err)
	}

	other := newTestGroup(t, repo, uniqueName(t, "Other"))
	if err := repo.Rename(ctx, other.ID, strings.ToLower(name), nil); !errors.Is(err, ErrAppGroupNameTaken) {
		t.Errorf("renaming onto a taken name: %v", err)
	}
	renamed := uniqueName(t, "Renamed")
	if err := repo.Rename(ctx, g.ID, renamed, nil); err != nil {
		t.Fatalf("Rename() error: %v", err)
	}
	got, err := repo.FindByID(ctx, g.ID)
	if err != nil || got.Name != renamed {
		t.Errorf("FindByID() = %+v, %v", got, err)
	}

	missing := "00000000-0000-4000-8000-000000000000"
	if _, err := repo.FindByID(ctx, missing); !errors.Is(err, ErrAppGroupNotFound) {
		t.Errorf("FindByID(missing): %v", err)
	}
	if err := repo.Rename(ctx, missing, "x", nil); !errors.Is(err, ErrAppGroupNotFound) {
		t.Errorf("Rename(missing): %v", err)
	}
	if err := repo.Delete(ctx, missing); !errors.Is(err, ErrAppGroupNotFound) {
		t.Errorf("Delete(missing): %v", err)
	}

	// The CHECK refuses a name the service would never send, as a second line.
	if _, err := repo.Create(ctx, "", nil); err == nil {
		t.Error("an empty name was accepted")
	}
	if _, err := repo.Create(ctx, strings.Repeat("x", 65), nil); err == nil {
		t.Error("a 65-character name was accepted")
	}
}

func TestAppGroupRepository_ADMappings(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAppGroupRepository(pool)
	ctx := context.Background()

	a := newTestGroup(t, repo, uniqueName(t, "A"))
	b := newTestGroup(t, repo, uniqueName(t, "B"))
	alpha := "CN=Team-Alpha," + t.Name() + ",DC=example,DC=internal"
	bravo := "CN=Team-Bravo," + t.Name() + ",DC=example,DC=internal"

	if err := repo.AddADMapping(ctx, a.ID, alpha, "Team-Alpha", nil); err != nil {
		t.Fatalf("AddADMapping() error: %v", err)
	}
	if err := repo.AddADMapping(ctx, a.ID, strings.ToUpper(alpha), "Team-Alpha", nil); !errors.Is(err, ErrADGroupAlreadyMapped) {
		t.Errorf("mapping again with different case: %v", err)
	}
	if err := repo.AddADMapping(ctx, a.ID, bravo, "Team-Bravo", nil); err != nil {
		t.Fatal(err)
	}
	// One AD group may be mapped by several application groups.
	if err := repo.AddADMapping(ctx, b.ID, alpha, "Team-Alpha", nil); err != nil {
		t.Errorf("mapping the same AD group to a second application group: %v", err)
	}
	if err := repo.AddADMapping(ctx, "00000000-0000-4000-8000-000000000000", alpha, "x", nil); !errors.Is(err, ErrAppGroupNotFound) {
		t.Errorf("mapping onto a missing group: %v", err)
	}

	list, err := repo.ListADMappings(ctx, a.ID)
	if err != nil || len(list) != 2 || list[0].Name != "Team-Alpha" || list[1].Name != "Team-Bravo" {
		t.Errorf("ListADMappings() = %+v, %v", list, err)
	}

	// MappedDNs lists a DN once however many groups map it (alpha is mapped twice).
	dns, err := repo.MappedDNs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range dns {
		if strings.EqualFold(d, alpha) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("MappedDNs listed %q %d times, want 1", alpha, count)
	}

	if removed, err := repo.RemoveADMapping(ctx, a.ID, strings.ToUpper(alpha)); err != nil || !removed {
		t.Errorf("RemoveADMapping(different case) = %v, %v", removed, err)
	}
	if removed, _ := repo.RemoveADMapping(ctx, a.ID, alpha); removed {
		t.Error("removed a mapping twice")
	}
}

func TestAppGroupRepository_MembersAreTheUnionOfLocalAndADWithTheirReasons(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAppGroupRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	g := newTestGroup(t, repo, uniqueName(t, "Analytics"))
	alpha := "CN=Alpha," + t.Name() + ",DC=example,DC=internal"
	bravo := "CN=Bravo," + t.Name() + ",DC=example,DC=internal"
	if err := repo.AddADMapping(ctx, g.ID, alpha, "Team-Alpha", nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddADMapping(ctx, g.ID, bravo, "Team-Bravo", nil); err != nil {
		t.Fatal(err)
	}

	ann := newTestADUser(t, users, "ann", "CN=Ann,"+t.Name())
	bob := newTestADUser(t, users, "bob", "CN=Bob,"+t.Name())
	outsider := newTestADUser(t, users, "outsider", "CN=Outsider,"+t.Name())
	loc := newTestLocalUser(t, users, "loc")

	// Ann is in both mapped groups, Bob in one, the outsider in a group nobody maps.
	if err := repo.ReplaceUserMemberships(ctx, ann.ID, []string{alpha, bravo}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceUserMemberships(ctx, bob.ID, []string{strings.ToUpper(alpha)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceUserMemberships(ctx, outsider.ID, []string{"CN=Unmapped," + t.Name()}); err != nil {
		t.Fatal(err)
	}
	if added, err := repo.AddLocalMember(ctx, g.ID, loc.ID, nil); err != nil || !added {
		t.Fatalf("AddLocalMember() = %v, %v", added, err)
	}
	if added, _ := repo.AddLocalMember(ctx, g.ID, loc.ID, nil); added {
		t.Error("added the same local member twice")
	}
	if _, err := repo.AddLocalMember(ctx, g.ID, "00000000-0000-4000-8000-000000000000", nil); !errors.Is(err, ErrAppGroupNotFound) {
		t.Errorf("adding a missing user: %v", err)
	}

	members, err := repo.Members(ctx, g.ID)
	if err != nil {
		t.Fatalf("Members() error: %v", err)
	}
	byID := map[string]AppGroupMember{}
	for _, m := range members {
		byID[m.UserID] = m
	}
	if len(members) != 3 {
		t.Fatalf("members = %d, want 3 (ann, bob, the local account): %+v", len(members), members)
	}
	if m := byID[ann.ID]; m.AddedManually || len(m.ViaADGroups) != 2 {
		t.Errorf("ann = %+v, want in via both AD groups, not manual", m)
	}
	if m := byID[bob.ID]; m.AddedManually || len(m.ViaADGroups) != 1 || m.ViaADGroups[0] != "Team-Alpha" {
		t.Errorf("bob = %+v, want via Team-Alpha (cached with different case)", m)
	}
	if m := byID[loc.ID]; !m.AddedManually || len(m.ViaADGroups) != 0 || m.LocalUsername == nil {
		t.Errorf("local account = %+v, want manual only", m)
	}
	if _, in := byID[outsider.ID]; in {
		t.Error("someone in an unmapped AD group is a member")
	}

	// Being both manual and in AD (not normally possible, but the union must not double count).
	summaries, err := repo.ListWithCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var s *AppGroupSummary
	for _, x := range summaries {
		if x.ID == g.ID {
			s = x
		}
	}
	if s == nil || s.ADGroupCount != 2 || s.LocalMemberCount != 1 || s.MemberCount != 3 {
		t.Errorf("summary = %+v, want 2 AD groups, 1 local member, 3 members", s)
	}

	if removed, err := repo.RemoveLocalMember(ctx, g.ID, loc.ID); err != nil || !removed {
		t.Errorf("RemoveLocalMember() = %v, %v", removed, err)
	}
}

func TestAppGroupRepository_CacheReplacement(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAppGroupRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	dn1 := "CN=One," + t.Name() + ",DC=example,DC=internal"
	dn2 := "CN=Two," + t.Name() + ",DC=example,DC=internal"
	u := newTestADUser(t, users, "u", "CN=U,"+t.Name())
	v := newTestADUser(t, users, "v", "CN=V,"+t.Name())

	cached := func(uid string) []string {
		rows, err := pool.Query(ctx, `SELECT ad_group_dn FROM user_ad_group_membership WHERE user_id = $1 ORDER BY ad_group_dn`, uid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var d string
			_ = rows.Scan(&d)
			out = append(out, d)
		}
		return out
	}

	// Replace sets, dedupes ignoring case, and removes what is no longer there.
	if err := repo.ReplaceUserMemberships(ctx, u.ID, []string{dn1, strings.ToUpper(dn1), dn2}); err != nil {
		t.Fatalf("ReplaceUserMemberships() error: %v", err)
	}
	if got := cached(u.ID); len(got) != 2 {
		t.Errorf("cached = %v, want 2 rows", got)
	}
	if err := repo.ReplaceUserMemberships(ctx, u.ID, []string{dn2}); err != nil || len(cached(u.ID)) != 1 {
		t.Errorf("after replacing with one: %v", cached(u.ID))
	}
	if err := repo.ReplaceUserMemberships(ctx, u.ID, nil); err != nil || len(cached(u.ID)) != 0 {
		t.Errorf("after clearing: %v", cached(u.ID))
	}

	// ReplaceMembersOfDN sets the people in one group and leaves other groups alone.
	if err := repo.ReplaceUserMemberships(ctx, v.ID, []string{dn2}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceMembersOfDN(ctx, dn1, []string{u.ID}); err != nil {
		t.Fatalf("ReplaceMembersOfDN() error: %v", err)
	}
	if len(cached(u.ID)) != 1 || len(cached(v.ID)) != 1 {
		t.Errorf("u=%v v=%v; v's other group must be untouched", cached(u.ID), cached(v.ID))
	}
	if err := repo.ReplaceMembersOfDN(ctx, dn1, nil); err != nil || len(cached(u.ID)) != 0 {
		t.Errorf("after emptying the group: %v", cached(u.ID))
	}

	// Prune drops memberships no mapping justifies.
	g := newTestGroup(t, repo, uniqueName(t, "Prune"))
	if err := repo.AddADMapping(ctx, g.ID, dn2, "Two", nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceUserMemberships(ctx, u.ID, []string{dn1, dn2}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PruneUnmappedMemberships(ctx); err != nil {
		t.Fatal(err)
	}
	if got := cached(u.ID); len(got) != 1 || !strings.EqualFold(got[0], dn2) {
		t.Errorf("after prune: %v, want only the mapped group", got)
	}
}

func TestUserRepository_FindIDsByLDAPDNs(t *testing.T) {
	pool := newTestPool(t)
	users := NewUserRepository(pool)
	ctx := context.Background()

	ann := newTestADUser(t, users, "ann", "CN=Ann,OU=Users,"+t.Name())
	bob := newTestADUser(t, users, "bob", "CN=Bob,OU=Users,"+t.Name())
	newTestLocalUser(t, users, "loc")

	ids, err := users.FindIDsByLDAPDNs(ctx, []string{strings.ToUpper("CN=Ann,OU=Users," + t.Name()), "CN=Nobody," + t.Name(), "CN=Bob,OU=Users," + t.Name()})
	if err != nil {
		t.Fatalf("FindIDsByLDAPDNs() error: %v", err)
	}
	sort.Strings(ids)
	want := []string{ann.ID, bob.ID}
	sort.Strings(want)
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if ids, err := users.FindIDsByLDAPDNs(ctx, nil); err != nil || len(ids) != 0 {
		t.Errorf("no DNs: %v, %v", ids, err)
	}
}

func TestAppGroupRepository_DeletingThingsCascades(t *testing.T) {
	pool := newTestPool(t)
	repo := NewAppGroupRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	g, err := repo.Create(ctx, uniqueName(t, "Doomed"), nil)
	if err != nil {
		t.Fatal(err)
	}
	loc := newTestLocalUser(t, users, "loc")
	if err := repo.AddADMapping(ctx, g.ID, "CN=X,"+t.Name(), "X", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AddLocalMember(ctx, g.ID, loc.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"app_group_ad_groups", "app_group_local_members"} {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE app_group_id = $1`, g.ID).Scan(&n)
		if n != 0 {
			t.Errorf("%s kept %d rows after the group was deleted", table, n)
		}
	}

	// Deleting a user removes their manual memberships and cached AD memberships.
	g2 := newTestGroup(t, repo, uniqueName(t, "Survivor"))
	gone, err := users.CreateLocal(ctx, fmt.Sprintf("gone-%d", time.Now().UnixNano()), "hash", "Gone", TierDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AddLocalMember(ctx, g2.ID, gone.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, gone.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	members, err := repo.Members(ctx, g2.ID)
	if err != nil || len(members) != 0 {
		t.Errorf("members after the user was deleted: %+v, %v", members, err)
	}
}

func TestAlertRepository_GroupAudienceVisibility(t *testing.T) {
	pool := newTestPool(t)
	alerts := NewAlertRepository(pool)
	groups := NewAppGroupRepository(pool)
	users := NewUserRepository(pool)
	ctx := context.Background()

	g := newTestGroup(t, groups, uniqueName(t, "Team"))
	adDN := "CN=Team," + t.Name() + ",DC=example,DC=internal"
	if err := groups.AddADMapping(ctx, g.ID, adDN, "Team", nil); err != nil {
		t.Fatal(err)
	}
	viaAD := newTestADUser(t, users, "viaad", "CN=ViaAD,"+t.Name())
	manual := newTestLocalUser(t, users, "manual")
	outsider := newTestADUser(t, users, "outsider", "CN=Outsider,"+t.Name())
	if err := groups.ReplaceUserMemberships(ctx, viaAD.ID, []string{adDN}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.AddLocalMember(ctx, g.ID, manual.ID, nil); err != nil {
		t.Fatal(err)
	}

	res := raiseTestAlert(t, alerts, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "for the team",
		Audiences: []AlertAudience{{Type: AlertAudienceGroup, AppGroupID: g.ID}},
	})
	id := res.Alert.ID

	sees := func(uid string, tier Tier) bool {
		_, err := alerts.FindVisible(ctx, AlertViewer{UserID: uid, Tier: tier}, id)
		if err != nil && !errors.Is(err, ErrAlertNotFound) {
			t.Fatalf("FindVisible() error: %v", err)
		}
		return err == nil
	}
	if !sees(viaAD.ID, TierReadOnly) {
		t.Error("a member through a mapped AD group does not see the alert")
	}
	if !sees(manual.ID, TierReadOnly) {
		t.Error("a manually added member does not see the alert")
	}
	if sees(outsider.ID, TierAdmin) {
		t.Error("a non-member sees it, even as an Admin: a group audience is not a tier")
	}
	if sees("", TierAdmin) {
		t.Error("the SuperAdmin (no user id) sees a group-addressed alert")
	}

	// It also shows up in the member's list and badge, and not the outsider's.
	if n, _ := alerts.CountUnread(ctx, AlertViewer{UserID: viaAD.ID, Tier: TierReadOnly}); n != 1 {
		t.Errorf("member badge = %d, want 1", n)
	}
	if n, _ := alerts.CountUnread(ctx, AlertViewer{UserID: outsider.ID, Tier: TierReadOnly}); n != 0 {
		t.Errorf("outsider badge = %d, want 0", n)
	}

	// Leaving the AD group (cache cleared) or being removed by hand removes access.
	if err := groups.ReplaceUserMemberships(ctx, viaAD.ID, nil); err != nil {
		t.Fatal(err)
	}
	if sees(viaAD.ID, TierReadOnly) {
		t.Error("someone who left the AD group still sees the alert")
	}
	if _, err := groups.RemoveLocalMember(ctx, g.ID, manual.ID); err != nil {
		t.Fatal(err)
	}
	if sees(manual.ID, TierReadOnly) {
		t.Error("someone removed by hand still sees the alert")
	}

	// The audience is returned with its group id, and a repeat does not duplicate it.
	key := "group-dedupe-" + t.Name()
	first := raiseTestAlert(t, alerts, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "x", DedupeKey: &key,
		Audiences: []AlertAudience{{Type: AlertAudienceGroup, AppGroupID: g.ID}},
	})
	if _, err := alerts.Raise(ctx, NewAlert{
		Kind: "test_kind", Severity: AlertSeverityInfo, Title: "x", DedupeKey: &key,
		Audiences: []AlertAudience{{Type: AlertAudienceGroup, AppGroupID: g.ID}},
	}); err != nil {
		t.Fatalf("repeat Raise() error: %v", err)
	}
	auds, err := alerts.Audiences(ctx, first.Alert.ID)
	if err != nil || len(auds) != 1 || auds[0].Type != AlertAudienceGroup || auds[0].AppGroupID != g.ID {
		t.Errorf("Audiences() = %+v, %v", auds, err)
	}

	// The database refuses a group audience without a group, or one that also names a user.
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_audiences (alert_id, audience_type) VALUES ($1, 'group')`, id); err == nil {
		t.Error("a group audience with no group was accepted")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_audiences (alert_id, audience_type, app_group_id, user_id) VALUES ($1, 'group', $2, $3)`, id, g.ID, viaAD.ID); err == nil {
		t.Error("a group audience that also names a user was accepted")
	}

	// Deleting the group removes it from the audience, so nobody sees the alert any more.
	if err := groups.Delete(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := groups.ReplaceUserMemberships(ctx, viaAD.ID, []string{adDN}); err != nil {
		t.Fatal(err)
	}
	if sees(viaAD.ID, TierReadOnly) {
		t.Error("the alert is still visible after its group was deleted")
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alert_audiences WHERE alert_id = $1`, id).Scan(&left)
	if left != 0 {
		t.Errorf("%d audience rows left after the group was deleted", left)
	}
}
