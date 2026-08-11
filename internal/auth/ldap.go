// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// matchingRuleInChain is AD's LDAP_MATCHING_RULE_IN_CHAIN extended match
// OID, used to resolve nested group membership server-side rather than
// walking it recursively in application code - see PLANNING.md Decisions
// Log.
const matchingRuleInChain = "1.2.840.113556.1.4.1941"

// dialTimeout bounds how long a connection attempt waits, so an
// unreachable LDAP server fails a login attempt quickly rather than
// hanging - matching internal/db's ping-timeout approach.
const dialTimeout = 5 * time.Second

// ldapConn is the subset of *ldap.Conn this package uses, narrow enough to
// fake in tests. Standing up a real AD-compatible LDAP server (with
// objectSid, memberOf, and LDAP_MATCHING_RULE_IN_CHAIN support) isn't
// practical to do disposably the way the Postgres integration tests do -
// see ARCHITECTURE.md Testing Strategy on fakes for protocol-level tests
// that can't safely run against real infrastructure in CI.
type ldapConn interface {
	Bind(username, password string) error
	Search(searchRequest *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// LDAPProvider is the on-prem AD implementation of IdentityProvider - see
// ARCHITECTURE.md Auth & Identity Provider.
type LDAPProvider struct {
	serverAddr    string
	bindDN        string
	bindPassword  string
	baseDN        string
	accessGroupDN string

	// dial is overridable in tests; defaults to a real LDAP connection.
	dial func(serverAddr string) (ldapConn, error)
}

// NewLDAPProvider constructs an LDAPProvider from the LDAP_* environment
// variables - see CLAUDE.md Configuration and Environment Variables.
func NewLDAPProvider(serverAddr, bindDN, bindPassword, baseDN, accessGroupDN string) *LDAPProvider {
	return &LDAPProvider{
		serverAddr:    serverAddr,
		bindDN:        bindDN,
		bindPassword:  bindPassword,
		baseDN:        baseDN,
		accessGroupDN: accessGroupDN,
		dial:          dialLDAP,
	}
}

func dialLDAP(serverAddr string) (ldapConn, error) {
	conn, err := ldap.DialURL(serverAddr, ldap.DialWithDialer(&net.Dialer{Timeout: dialTimeout}))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Authenticate implements IdentityProvider. ctx is accepted for
// consistency with the rest of this codebase's handler/service functions -
// see CLAUDE.md's Go Language Conventions - but go-ldap v3's Bind and
// Search calls do not accept a context themselves; dialTimeout bounds
// connection latency instead.
func (p *LDAPProvider) Authenticate(ctx context.Context, username, password string) (*AuthenticatedUser, error) {
	// AD (like most LDAP servers) treats a bind with a valid DN and an
	// EMPTY password as an "unauthenticated bind" that succeeds without
	// checking the password at all (RFC 4513 SS5.1.2). Guard against this
	// explicitly rather than relying on the server to reject it.
	if password == "" {
		return nil, ErrInvalidCredentials
	}

	searchConn, err := p.dial(p.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to LDAP server: %w", err)
	}
	defer searchConn.Close()

	if err := searchConn.Bind(p.bindDN, p.bindPassword); err != nil {
		return nil, fmt.Errorf("bind as service account: %w", err)
	}

	entry, err := p.findUser(searchConn, username)
	if err != nil {
		return nil, err
	}

	if err := p.verifyPassword(entry.DN, password); err != nil {
		return nil, err
	}

	inAccessGroup, err := p.isMemberOf(searchConn, entry.DN, p.accessGroupDN)
	if err != nil {
		return nil, fmt.Errorf("resolve access group membership: %w", err)
	}

	sid, err := decodeObjectSID(entry.GetRawAttributeValue("objectSid"))
	if err != nil {
		return nil, fmt.Errorf("decode objectSid: %w", err)
	}

	return &AuthenticatedUser{
		ADSID:         sid,
		DisplayName:   entry.GetAttributeValue("displayName"),
		InAccessGroup: inAccessGroup,
	}, nil
}

// findUser looks up exactly one user by sAMAccountName. Zero or more than
// one match both return ErrInvalidCredentials - an ambiguous match is
// treated as a failed login, not resolved by picking one arbitrarily.
func (p *LDAPProvider) findUser(conn ldapConn, username string) (*ldap.Entry, error) {
	req := ldap.NewSearchRequest(
		p.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
		fmt.Sprintf("(sAMAccountName=%s)", ldap.EscapeFilter(username)),
		[]string{"displayName", "objectSid"},
		nil,
	)

	result, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search for user: %w", err)
	}
	if len(result.Entries) != 1 {
		return nil, ErrInvalidCredentials
	}
	return result.Entries[0], nil
}

// verifyPassword performs the actual credential check. AD does not expose
// password hashes over LDAP, so binding as the user with their supplied
// password is the only way to verify it - this uses a separate, dedicated
// connection so the search connection's service-account identity (and its
// read permissions) is never replaced by the user's own.
func (p *LDAPProvider) verifyPassword(userDN, password string) error {
	conn, err := p.dial(p.serverAddr)
	if err != nil {
		return fmt.Errorf("connect to LDAP server: %w", err)
	}
	defer conn.Close()

	if err := conn.Bind(userDN, password); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// isMemberOf resolves nested group membership server-side via
// LDAP_MATCHING_RULE_IN_CHAIN - see PLANNING.md Decisions Log.
func (p *LDAPProvider) isMemberOf(conn ldapConn, userDN, groupDN string) (bool, error) {
	filter := fmt.Sprintf(
		"(&(distinguishedName=%s)(memberOf:%s:=%s))",
		ldap.EscapeFilter(userDN), matchingRuleInChain, ldap.EscapeFilter(groupDN),
	)
	req := ldap.NewSearchRequest(
		p.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		filter,
		[]string{"dn"},
		nil,
	)

	result, err := conn.Search(req)
	if err != nil {
		return false, fmt.Errorf("check group membership: %w", err)
	}
	return len(result.Entries) == 1, nil
}
