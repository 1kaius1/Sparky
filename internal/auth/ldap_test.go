// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// Standing up a real AD-compatible LDAP server (with objectSid, memberOf,
// and LDAP_MATCHING_RULE_IN_CHAIN support) isn't practical to do
// disposably the way the Postgres integration tests do - see ldapConn's
// doc comment in ldap.go. These tests fake the connection instead, to
// cover this package's own logic: filter construction, the search-then-
// bind flow, and error mapping.

const (
	testBindDN        = "CN=svc-sparky,DC=example,DC=internal"
	testBindPassword  = "svc-password"
	testBaseDN        = "DC=example,DC=internal"
	testAccessGroupDN = "CN=Sparky-Access,DC=example,DC=internal"
	testUserDN        = "CN=jsmith,DC=example,DC=internal"
	testUserPassword  = "correct-horse-battery-staple"
)

// fakeConn implements ldapConn for tests.
type fakeConn struct {
	bindFunc   func(username, password string) error
	searchFunc func(req *ldap.SearchRequest) (*ldap.SearchResult, error)
	closed     bool
}

func (f *fakeConn) Bind(username, password string) error {
	return f.bindFunc(username, password)
}

func (f *fakeConn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	return f.searchFunc(req)
}

func (f *fakeConn) Close() error {
	f.closed = true
	return nil
}

// testDirectory configures a fake LDAP directory's behavior for one test.
type testDirectory struct {
	serviceBindErr error

	userSearchResult *ldap.SearchResult
	userSearchErr    error

	groupSearchResult *ldap.SearchResult
	groupSearchErr    error

	validUserDN       string
	validUserPassword string
}

func (d *testDirectory) dial(_ string) (ldapConn, error) {
	bind := func(username, password string) error {
		if username == testBindDN {
			if password != testBindPassword {
				return errors.New("wrong service account password")
			}
			return d.serviceBindErr
		}
		if username == d.validUserDN && password == d.validUserPassword {
			return nil
		}
		return errors.New("invalid credentials")
	}

	search := func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
		if strings.HasPrefix(req.Filter, "(sAMAccountName=") {
			return d.userSearchResult, d.userSearchErr
		}
		return d.groupSearchResult, d.groupSearchErr
	}

	return &fakeConn{bindFunc: bind, searchFunc: search}, nil
}

func newTestProvider(d *testDirectory) *LDAPProvider {
	p := NewLDAPProvider("ldap://unused.invalid", testBindDN, testBindPassword, testBaseDN, testAccessGroupDN)
	p.dial = d.dial
	return p
}

// validSIDBytes is a well-formed objectSid payload - see sid_test.go for
// the format this mirrors.
var validSIDBytes = encodeObjectSIDForTest(1, 5, []uint32{21, 3623811015, 3361044348, 30300820, 1013})

func oneResult(entry *ldap.Entry) *ldap.SearchResult {
	return &ldap.SearchResult{Entries: []*ldap.Entry{entry}}
}

func noResults() *ldap.SearchResult {
	return &ldap.SearchResult{}
}

func TestLDAPProvider_Authenticate_SuccessInAccessGroup(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {string(validSIDBytes)},
	})
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		groupSearchResult: oneResult(ldap.NewEntry(testUserDN, nil)),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}

	user, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", testUserPassword)
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}
	if user.DisplayName != "Jane Smith" {
		t.Errorf("DisplayName = %q, want %q", user.DisplayName, "Jane Smith")
	}
	if user.ADSID != "S-1-5-21-3623811015-3361044348-30300820-1013" {
		t.Errorf("ADSID = %q, want the decoded SID", user.ADSID)
	}
	if !user.InAccessGroup {
		t.Error("InAccessGroup = false, want true")
	}
	if user.DN != testUserDN {
		t.Errorf("DN = %q, want %q", user.DN, testUserDN)
	}
}

func TestLDAPProvider_Authenticate_SuccessNotInAccessGroup(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {string(validSIDBytes)},
	})
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		groupSearchResult: noResults(),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}

	user, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", testUserPassword)
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}
	if user.InAccessGroup {
		t.Error("InAccessGroup = true, want false")
	}
}

func TestLDAPProvider_Authenticate_EmptyPassword(t *testing.T) {
	// No directory behavior configured at all - a real implementation
	// must reject this before ever dialing, since AD treats a bind with a
	// valid DN and an empty password as an unauthenticated bind that
	// succeeds without checking the password (RFC 4513 SS5.1.2).
	dir := &testDirectory{}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLDAPProvider_Authenticate_WrongPassword(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {string(validSIDBytes)},
	})
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", "wrong-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLDAPProvider_Authenticate_UserNotFound(t *testing.T) {
	dir := &testDirectory{
		userSearchResult: noResults(),
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "nobody", "irrelevant")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLDAPProvider_Authenticate_AmbiguousUser(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{"displayName": {"Jane Smith"}})
	dir := &testDirectory{
		userSearchResult: &ldap.SearchResult{Entries: []*ldap.Entry{entry, entry}},
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", "irrelevant")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLDAPProvider_Authenticate_ServiceAccountBindFails(t *testing.T) {
	dir := &testDirectory{
		serviceBindErr: errors.New("service account credentials rejected"),
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", "irrelevant")
	if errors.Is(err, ErrInvalidCredentials) {
		t.Error("Authenticate() returned ErrInvalidCredentials for an infrastructure failure, want a wrapped error")
	}
	if err == nil {
		t.Fatal("Authenticate() succeeded despite a service account bind failure")
	}
}

func TestLDAPProvider_Authenticate_UserSearchFails(t *testing.T) {
	dir := &testDirectory{
		userSearchErr: errors.New("directory unreachable"),
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", "irrelevant")
	if errors.Is(err, ErrInvalidCredentials) {
		t.Error("Authenticate() returned ErrInvalidCredentials for an infrastructure failure, want a wrapped error")
	}
	if err == nil {
		t.Fatal("Authenticate() succeeded despite a user search failure")
	}
}

func TestLDAPProvider_Authenticate_GroupSearchFails(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {string(validSIDBytes)},
	})
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		groupSearchErr:    errors.New("directory unreachable"),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", testUserPassword)
	if errors.Is(err, ErrInvalidCredentials) {
		t.Error("Authenticate() returned ErrInvalidCredentials for an infrastructure failure, want a wrapped error")
	}
	if err == nil {
		t.Fatal("Authenticate() succeeded despite a group search failure")
	}
}

func TestLDAPProvider_Authenticate_BadObjectSID(t *testing.T) {
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {"too-short"},
	})
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		groupSearchResult: noResults(),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}

	_, err := newTestProvider(dir).Authenticate(context.Background(), "jsmith", testUserPassword)
	if err == nil {
		t.Fatal("Authenticate() succeeded despite an undecodable objectSid")
	}
}

func TestLDAPProvider_Authenticate_FilterEscaping(t *testing.T) {
	// A username containing filter metacharacters must not be able to
	// alter the search filter's structure.
	entry := ldap.NewEntry(testUserDN, map[string][]string{
		"displayName": {"Jane Smith"},
		"objectSid":   {string(validSIDBytes)},
	})
	var capturedFilter string
	dir := &testDirectory{
		userSearchResult:  oneResult(entry),
		groupSearchResult: noResults(),
		validUserDN:       testUserDN,
		validUserPassword: testUserPassword,
	}
	p := newTestProvider(dir)
	originalDial := p.dial
	p.dial = func(addr string) (ldapConn, error) {
		conn, err := originalDial(addr)
		if err != nil {
			return nil, err
		}
		fc := conn.(*fakeConn)
		innerSearch := fc.searchFunc
		fc.searchFunc = func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
			if strings.HasPrefix(req.Filter, "(sAMAccountName=") {
				capturedFilter = req.Filter
			}
			return innerSearch(req)
		}
		return fc, nil
	}

	_, err := p.Authenticate(context.Background(), "jsmith)(sAMAccountName=admin", testUserPassword)
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}
	if strings.Contains(capturedFilter, ")(sAMAccountName=admin") {
		t.Errorf("filter was not escaped: %q", capturedFilter)
	}
}

func TestLDAPProvider_IsInAccessGroup_Member(t *testing.T) {
	dir := &testDirectory{groupSearchResult: oneResult(ldap.NewEntry(testUserDN, nil))}

	inGroup, err := newTestProvider(dir).IsInAccessGroup(context.Background(), testUserDN)
	if err != nil {
		t.Fatalf("IsInAccessGroup() error: %v", err)
	}
	if !inGroup {
		t.Error("IsInAccessGroup() = false, want true")
	}
}

func TestLDAPProvider_IsInAccessGroup_NotMember(t *testing.T) {
	dir := &testDirectory{groupSearchResult: noResults()}

	inGroup, err := newTestProvider(dir).IsInAccessGroup(context.Background(), testUserDN)
	if err != nil {
		t.Fatalf("IsInAccessGroup() error: %v", err)
	}
	if inGroup {
		t.Error("IsInAccessGroup() = true, want false")
	}
}

func TestLDAPProvider_IsInAccessGroup_ServiceAccountBindFails(t *testing.T) {
	dir := &testDirectory{serviceBindErr: errors.New("service account disabled")}

	_, err := newTestProvider(dir).IsInAccessGroup(context.Background(), testUserDN)
	if err == nil {
		t.Fatal("IsInAccessGroup() succeeded despite a service-account bind failure")
	}
}

func TestLDAPProvider_IsInAccessGroup_SearchFails(t *testing.T) {
	dir := &testDirectory{groupSearchErr: errors.New("directory unreachable")}

	_, err := newTestProvider(dir).IsInAccessGroup(context.Background(), testUserDN)
	if err == nil {
		t.Fatal("IsInAccessGroup() succeeded despite a group search failure")
	}
}
