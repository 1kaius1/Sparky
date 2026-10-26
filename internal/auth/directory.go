// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/go-ldap/ldap/v3"
)

// DirectoryGroup is a group in the directory, as the directory itself reports
// it. DN is the directory's own spelling of the distinguishedName - the value
// to store, never one a browser supplied.
type DirectoryGroup struct {
	DN          string
	Name        string
	Description string
}

// GroupSearchResult is a capped list of directory groups. Truncated is true
// when more matched than were returned, so the caller can say "refine your
// search" instead of pretending the list is complete.
type GroupSearchResult struct {
	Groups    []DirectoryGroup
	Truncated bool
}

// ErrGroupNotFound is returned when a DN names no group in the directory (or
// names something outside the configured search base).
var ErrGroupNotFound = errors.New("group not found in the directory")

// GroupDirectory is what application groups need from the directory, beside
// IdentityProvider's login: find groups by name, verify one, list one's
// members, and ask which of a set of groups a person is in. LDAPProvider
// implements it; an OIDC/Entra provider could too. Every call uses the
// service account.
type GroupDirectory interface {
	// SearchGroups finds groups whose name contains term, at most limit of
	// them. Nothing is listed unprompted: the caller must pass a term.
	SearchGroups(ctx context.Context, term string, limit int) (*GroupSearchResult, error)

	// FindGroup verifies that dn is a group under the search base and returns
	// the directory's own record of it, or ErrGroupNotFound.
	FindGroup(ctx context.Context, dn string) (*DirectoryGroup, error)

	// GroupMemberDNs returns the DNs of every person in the group, nested
	// membership included.
	GroupMemberDNs(ctx context.Context, groupDN string) ([]string, error)

	// MemberOf returns which of groupDNs the person at userDN is in, nested
	// membership included, as the directory spells their DNs. One query per
	// batch: it never fetches the person's other groups.
	MemberOf(ctx context.Context, userDN string, groupDNs []string) ([]string, error)
}

var _ GroupDirectory = (*LDAPProvider)(nil)

const (
	// memberOfBatch bounds how many group DNs go into one MemberOf filter, so
	// a deployment with many mapped groups never builds an enormous filter.
	memberOfBatch = 50

	// memberPageSize is the page size for listing a group's members; AD caps
	// one response at 1000 entries by default, so a big group needs paging.
	memberPageSize = 500
)

// serviceConn dials and binds as the service account.
func (p *LDAPProvider) serviceConn() (ldapConn, error) {
	conn, err := p.dial(p.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to LDAP server: %w", err)
	}
	if err := conn.Bind(p.bindDN, p.bindPassword); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("bind as service account: %w", err)
	}
	return conn, nil
}

// SearchGroups implements GroupDirectory.
func (p *LDAPProvider) SearchGroups(ctx context.Context, term string, limit int) (*GroupSearchResult, error) {
	if limit < 1 {
		limit = 1
	}
	conn, err := p.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	escaped := ldap.EscapeFilter(term)
	// One more than asked for tells us whether the list was cut short.
	req := ldap.NewSearchRequest(
		p.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, limit+1, 0, false,
		fmt.Sprintf("(&(objectClass=group)(|(cn=*%s*)(sAMAccountName=*%s*)))", escaped, escaped),
		[]string{"cn", "description"},
		nil,
	)
	result, err := conn.Search(req)
	// A directory that stops at its own size limit still returns the entries it
	// found; that is a truncated list, not a failure.
	if err != nil && !(ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && result != nil) {
		return nil, fmt.Errorf("search for groups: %w", err)
	}

	out := &GroupSearchResult{}
	for _, e := range result.Entries {
		if len(out.Groups) == limit {
			out.Truncated = true
			break
		}
		out.Groups = append(out.Groups, groupOf(e))
	}
	if err != nil {
		out.Truncated = true
	}
	return out, nil
}

func groupOf(e *ldap.Entry) DirectoryGroup {
	name := e.GetAttributeValue("cn")
	if name == "" {
		name = e.DN
	}
	return DirectoryGroup{DN: e.DN, Name: name, Description: e.GetAttributeValue("description")}
}

// validGroupDN refuses a DN that could not be a normal directory name, or that
// lies outside the search base: a group elsewhere in the forest is not one this
// deployment looks users up in.
func (p *LDAPProvider) validGroupDN(dn string) bool {
	if dn == "" || len(dn) > 1024 {
		return false
	}
	for _, r := range dn {
		if unicode.IsControl(r) {
			return false
		}
	}
	lower, base := strings.ToLower(dn), strings.ToLower(p.baseDN)
	return lower == base || strings.HasSuffix(lower, ","+base)
}

// FindGroup implements GroupDirectory.
func (p *LDAPProvider) FindGroup(ctx context.Context, dn string) (*DirectoryGroup, error) {
	if !p.validGroupDN(dn) {
		return nil, ErrGroupNotFound
	}
	conn, err := p.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := ldap.NewSearchRequest(
		dn,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false,
		"(objectClass=group)",
		[]string{"cn", "description"},
		nil,
	)
	result, err := conn.Search(req)
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return nil, ErrGroupNotFound
		}
		return nil, fmt.Errorf("look up group: %w", err)
	}
	if len(result.Entries) != 1 {
		return nil, ErrGroupNotFound
	}
	g := groupOf(result.Entries[0])
	return &g, nil
}

// GroupMemberDNs implements GroupDirectory.
func (p *LDAPProvider) GroupMemberDNs(ctx context.Context, groupDN string) ([]string, error) {
	if !p.validGroupDN(groupDN) {
		return nil, ErrGroupNotFound
	}
	conn, err := p.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := ldap.NewSearchRequest(
		p.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		fmt.Sprintf("(&(objectCategory=person)(objectClass=user)(memberOf:%s:=%s))", matchingRuleInChain, ldap.EscapeFilter(groupDN)),
		[]string{"dn"},
		nil,
	)
	result, err := conn.SearchWithPaging(req, memberPageSize)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	dns := make([]string, 0, len(result.Entries))
	for _, e := range result.Entries {
		dns = append(dns, e.DN)
	}
	return dns, nil
}

// MemberOf implements GroupDirectory.
func (p *LDAPProvider) MemberOf(ctx context.Context, userDN string, groupDNs []string) ([]string, error) {
	if len(groupDNs) == 0 {
		return nil, nil
	}
	conn, err := p.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var in []string
	for start := 0; start < len(groupDNs); start += memberOfBatch {
		end := start + memberOfBatch
		if end > len(groupDNs) {
			end = len(groupDNs)
		}
		var alternatives strings.Builder
		for _, g := range groupDNs[start:end] {
			fmt.Fprintf(&alternatives, "(distinguishedName=%s)", ldap.EscapeFilter(g))
		}
		req := ldap.NewSearchRequest(
			p.baseDN,
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
			fmt.Sprintf("(&(objectClass=group)(member:%s:=%s)(|%s))", matchingRuleInChain, ldap.EscapeFilter(userDN), alternatives.String()),
			[]string{"dn"},
			nil,
		)
		result, err := conn.Search(req)
		if err != nil {
			return nil, fmt.Errorf("check group memberships: %w", err)
		}
		for _, e := range result.Entries {
			in = append(in, e.DN)
		}
	}
	return in, nil
}
