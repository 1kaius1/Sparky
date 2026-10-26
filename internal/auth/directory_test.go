// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// These tests fake the LDAP connection, like ldap_test.go, because a real
// AD-compatible server (nested groups, the in-chain matching rule) cannot be
// run disposably here. They cover this file's own logic: the filters it
// builds and escapes, batching, truncation, containment in the search base,
// and error mapping. How a real AD answers is a test for a real AD.

// groupProvider returns a provider whose connection answers every search with
// search, plus a pointer to the requests it received.
func groupProvider(search func(*ldap.SearchRequest) (*ldap.SearchResult, error)) (*LDAPProvider, *[]*ldap.SearchRequest) {
	var seen []*ldap.SearchRequest
	p := NewLDAPProvider("ldap://fake", testBindDN, testBindPassword, testBaseDN, testAccessGroupDN)
	p.dial = func(string) (ldapConn, error) {
		return &fakeConn{
			bindFunc: func(user, pass string) error {
				if user == testBindDN && pass == testBindPassword {
					return nil
				}
				return errors.New("invalid credentials")
			},
			searchFunc: func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
				seen = append(seen, req)
				return search(req)
			},
		}, nil
	}
	return p, &seen
}

func groupEntry(dn, cn string) *ldap.Entry {
	return ldap.NewEntry(dn, map[string][]string{"cn": {cn}, "description": {cn + " people"}})
}

func result(entries ...*ldap.Entry) *ldap.SearchResult { return &ldap.SearchResult{Entries: entries} }

const alphaDN = "CN=Team-Alpha,OU=Groups,DC=example,DC=internal"

func TestSearchGroups_BuildsAnEscapedFilterAndReturnsNames(t *testing.T) {
	p, seen := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(groupEntry(alphaDN, "Team-Alpha")), nil
	})

	got, err := p.SearchGroups(context.Background(), `al*)(cn=*pha`, 25)
	if err != nil {
		t.Fatalf("SearchGroups() error: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0].DN != alphaDN || got.Groups[0].Name != "Team-Alpha" || got.Truncated {
		t.Errorf("result = %+v", got)
	}
	req := (*seen)[0]
	if req.BaseDN != testBaseDN || req.Scope != ldap.ScopeWholeSubtree {
		t.Errorf("search base/scope = %q/%d", req.BaseDN, req.Scope)
	}
	// A hostile term must not be able to add filter clauses of its own.
	if strings.Contains(req.Filter, ")(cn=*pha") || !strings.Contains(req.Filter, `\2a\29\28cn=\2apha`) {
		t.Errorf("the search term was not escaped: %s", req.Filter)
	}
	if req.SizeLimit != 26 {
		t.Errorf("size limit = %d, want 26 (one more than asked, to detect truncation)", req.SizeLimit)
	}
}

func TestSearchGroups_ReportsTruncationInBothWays(t *testing.T) {
	// 3 matches for a limit of 2: the directory returned one more than asked.
	p, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(groupEntry("CN=A,DC=example,DC=internal", "A"), groupEntry("CN=B,DC=example,DC=internal", "B"), groupEntry("CN=C,DC=example,DC=internal", "C")), nil
	})
	got, err := p.SearchGroups(context.Background(), "abc", 2)
	if err != nil || len(got.Groups) != 2 || !got.Truncated {
		t.Errorf("one past the limit: %+v, %v", got, err)
	}

	// A directory that stops at its own size limit still hands back entries.
	p, _ = groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(groupEntry("CN=A,DC=example,DC=internal", "A")), ldap.NewError(ldap.LDAPResultSizeLimitExceeded, errors.New("limit"))
	})
	got, err = p.SearchGroups(context.Background(), "abc", 25)
	if err != nil || len(got.Groups) != 1 || !got.Truncated {
		t.Errorf("directory size limit: %+v, %v", got, err)
	}
}

func TestSearchGroups_Errors(t *testing.T) {
	p, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return nil, errors.New("connection reset")
	})
	if _, err := p.SearchGroups(context.Background(), "abc", 25); err == nil || !strings.Contains(err.Error(), "search for groups") {
		t.Errorf("a search failure: %v", err)
	}

	// A wrong service password is reported, not mistaken for "no groups".
	p, _ = groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) { return result(), nil })
	p.bindPassword = "wrong"
	if _, err := p.SearchGroups(context.Background(), "abc", 25); err == nil || !strings.Contains(err.Error(), "bind as service account") {
		t.Errorf("a bind failure: %v", err)
	}
}

func TestFindGroup_ReturnsTheDirectorysOwnRecord(t *testing.T) {
	p, seen := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		// The directory spells the DN its own way, whatever case was asked for.
		return result(groupEntry(alphaDN, "Team-Alpha")), nil
	})

	got, err := p.FindGroup(context.Background(), strings.ToLower(alphaDN))
	if err != nil {
		t.Fatalf("FindGroup() error: %v", err)
	}
	if got.DN != alphaDN || got.Name != "Team-Alpha" {
		t.Errorf("group = %+v, want the directory's own DN %q", got, alphaDN)
	}
	if req := (*seen)[0]; req.BaseDN != strings.ToLower(alphaDN) || req.Scope != ldap.ScopeBaseObject || req.Filter != "(objectClass=group)" {
		t.Errorf("request = base %q scope %d filter %q", req.BaseDN, req.Scope, req.Filter)
	}
}

func TestFindGroup_NotFoundCases(t *testing.T) {
	ctx := context.Background()

	missing, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return nil, ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))
	})
	if _, err := missing.FindGroup(ctx, alphaDN); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("no such object: %v", err)
	}

	notAGroup, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) { return result(), nil })
	if _, err := notAGroup.FindGroup(ctx, alphaDN); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("an object that is not a group: %v", err)
	}

	// These never reach the directory at all.
	dialed := false
	guarded, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) { dialed = true; return result(), nil })
	for name, dn := range map[string]string{
		"empty":                   "",
		"outside the search base": "CN=Team,DC=other,DC=forest",
		"a prefix trick":          "CN=Team,DC=notexample,DC=internal",
		"a control character":     "CN=Team\n,DC=example,DC=internal",
		"too long":                "CN=" + strings.Repeat("a", 2000) + ",DC=example,DC=internal",
	} {
		if _, err := guarded.FindGroup(ctx, dn); !errors.Is(err, ErrGroupNotFound) {
			t.Errorf("%s: %v, want ErrGroupNotFound", name, err)
		}
	}
	if dialed {
		t.Error("an unacceptable DN reached the directory")
	}
}

func TestGroupMemberDNs_UsesTheInChainRuleAndPaging(t *testing.T) {
	p, seen := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(ldap.NewEntry("CN=Ann,OU=Users,DC=example,DC=internal", nil), ldap.NewEntry("CN=Bob,OU=Users,DC=example,DC=internal", nil)), nil
	})

	dns, err := p.GroupMemberDNs(context.Background(), "CN=Team (A*),OU=Groups,DC=example,DC=internal")
	if err != nil {
		t.Fatalf("GroupMemberDNs() error: %v", err)
	}
	if len(dns) != 2 || dns[0] != "CN=Ann,OU=Users,DC=example,DC=internal" {
		t.Errorf("dns = %v", dns)
	}
	f := (*seen)[0].Filter
	if !strings.Contains(f, "memberOf:"+matchingRuleInChain+":=") || !strings.Contains(f, `CN=Team \28A\2a\29`) {
		t.Errorf("filter = %s", f)
	}

	if _, err := p.GroupMemberDNs(context.Background(), "CN=X,DC=other,DC=forest"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("a group outside the base: %v", err)
	}
}

func TestMemberOf_NoGroupsMeansNoQuery(t *testing.T) {
	dialed := false
	p, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) { dialed = true; return result(), nil })
	got, err := p.MemberOf(context.Background(), testUserDN, nil)
	if err != nil || len(got) != 0 || dialed {
		t.Errorf("MemberOf(nil) = %v, %v, searched=%v", got, err, dialed)
	}
}

func TestMemberOf_AsksOnlyAboutTheMappedGroupsAndEscapesThem(t *testing.T) {
	p, seen := groupProvider(func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(groupEntry(alphaDN, "Team-Alpha")), nil
	})

	groups := []string{alphaDN, `CN=Odd (name)*,DC=example,DC=internal`}
	got, err := p.MemberOf(context.Background(), `CN=O'Brien\, Pat,OU=Users,DC=example,DC=internal`, groups)
	if err != nil {
		t.Fatalf("MemberOf() error: %v", err)
	}
	if len(got) != 1 || got[0] != alphaDN {
		t.Errorf("got %v", got)
	}
	if len(*seen) != 1 {
		t.Fatalf("searches = %d, want 1 for two groups", len(*seen))
	}
	f := (*seen)[0].Filter
	for _, want := range []string{
		"(objectClass=group)",
		"member:" + matchingRuleInChain + ":=",
		"(distinguishedName=" + alphaDN + ")",
		`\28name\29\2a`,   // the second group's DN, escaped
		`O'Brien\5c, Pat`, // the person's DN has a backslash-escaped comma; the filter escapes the backslash
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter is missing %q: %s", want, f)
		}
	}
	// It asks about membership, never "list this person's groups".
	if strings.Contains(f, "memberOf=*") {
		t.Errorf("filter fetches every group: %s", f)
	}
}

func TestMemberOf_BatchesALargeSet(t *testing.T) {
	var groups []string
	for i := 0; i < 120; i++ {
		groups = append(groups, fmt.Sprintf("CN=G%03d,OU=Groups,DC=example,DC=internal", i))
	}
	p, seen := groupProvider(func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
		// Say the person is in the first group of every batch.
		first := strings.Index(req.Filter, "(distinguishedName=")
		end := strings.Index(req.Filter[first:], ")") + first
		return result(ldap.NewEntry(req.Filter[first+len("(distinguishedName="):end], nil)), nil
	})

	got, err := p.MemberOf(context.Background(), testUserDN, groups)
	if err != nil {
		t.Fatalf("MemberOf() error: %v", err)
	}
	if len(*seen) != 3 {
		t.Errorf("searches = %d, want 3 batches (50, 50, 20)", len(*seen))
	}
	if len(got) != 3 || got[0] != groups[0] || got[1] != groups[50] || got[2] != groups[100] {
		t.Errorf("got %v", got)
	}
	for i, req := range *seen {
		if n := strings.Count(req.Filter, "(distinguishedName="); n > memberOfBatch {
			t.Errorf("batch %d has %d groups in one filter", i, n)
		}
	}
}

func TestMemberOf_Errors(t *testing.T) {
	p, _ := groupProvider(func(*ldap.SearchRequest) (*ldap.SearchResult, error) { return nil, errors.New("boom") })
	if _, err := p.MemberOf(context.Background(), testUserDN, []string{alphaDN}); err == nil || !strings.Contains(err.Error(), "check group memberships") {
		t.Errorf("a search failure: %v", err)
	}
}
