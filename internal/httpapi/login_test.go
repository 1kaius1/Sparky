// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/session"
)

const testSessionSecret = "test-session-secret-value"

// fakeIdentityProvider implements auth.IdentityProvider for tests.
type fakeIdentityProvider struct {
	user *auth.AuthenticatedUser
	err  error

	isInAccessGroupResult bool
	isInAccessGroupErr    error
	isInAccessGroupCalls  []string
}

func (f *fakeIdentityProvider) Authenticate(_ context.Context, _, _ string) (*auth.AuthenticatedUser, error) {
	return f.user, f.err
}

func (f *fakeIdentityProvider) IsInAccessGroup(_ context.Context, dn string) (bool, error) {
	f.isInAccessGroupCalls = append(f.isInAccessGroupCalls, dn)
	if f.isInAccessGroupErr != nil {
		return false, f.isInAccessGroupErr
	}
	return f.isInAccessGroupResult, nil
}

// fakeUserStore implements userStore for tests without a real Postgres.
type fakeUserStore struct {
	byADSID map[string]*db.User

	createErr          error
	updateLastLoginErr error
	findErr            error // returned for any ad_sid not in byADSID, in place of db.ErrUserNotFound
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{byADSID: make(map[string]*db.User)}
}

func (f *fakeUserStore) FindByADSID(_ context.Context, adSID string) (*db.User, error) {
	if u, ok := f.byADSID[adSID]; ok {
		return u, nil
	}
	if f.findErr != nil {
		return nil, f.findErr
	}
	return nil, db.ErrUserNotFound
}

func (f *fakeUserStore) Create(_ context.Context, adSID, displayName, dn string, tier db.Tier) (*db.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	u := &db.User{ID: "new-" + adSID, ADSID: adSID, DisplayName: displayName, Tier: tier, CreatedAt: time.Now().UTC(), LDAPDN: &dn}
	f.byADSID[adSID] = u
	return u, nil
}

func (f *fakeUserStore) UpdateLastLogin(_ context.Context, id, dn string, at time.Time) error {
	if f.updateLastLoginErr != nil {
		return f.updateLastLoginErr
	}
	for _, u := range f.byADSID {
		if u.ID == id {
			u.LastLoginAt = &at
			u.LDAPDN = &dn
			return nil
		}
	}
	return db.ErrUserNotFound
}

func TestLoginService_Login_NewUser(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID:         "S-1-5-21-1",
		DisplayName:   "Jane Smith",
		InAccessGroup: true,
	}}
	store := newFakeUserStore()
	svc := NewLoginService(idp, store, testSessionSecret)

	user, cookieValue, err := svc.Login(context.Background(), "jsmith", "password")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if user.DisplayName != "Jane Smith" {
		t.Errorf("DisplayName = %q, want %q", user.DisplayName, "Jane Smith")
	}
	if user.Tier != db.TierReadOnly {
		t.Errorf("Tier = %q, want %q for a first-time login", user.Tier, db.TierReadOnly)
	}

	sess, err := session.Verify(testSessionSecret, cookieValue)
	if err != nil {
		t.Fatalf("session.Verify() error: %v", err)
	}
	if sess.UserID != user.ID {
		t.Errorf("session UserID = %q, want %q", sess.UserID, user.ID)
	}
}

func TestLoginService_Login_ExistingUser_UpdatesLastLogin(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID:         "S-1-5-21-1",
		DisplayName:   "Jane Smith",
		InAccessGroup: true,
	}}
	store := newFakeUserStore()
	existing := &db.User{ID: "existing-id", ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", Tier: db.TierDeveloper}
	store.byADSID["S-1-5-21-1"] = existing

	svc := NewLoginService(idp, store, testSessionSecret)

	user, _, err := svc.Login(context.Background(), "jsmith", "password")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if user.ID != "existing-id" {
		t.Errorf("ID = %q, want the existing user's ID", user.ID)
	}
	if user.Tier != db.TierDeveloper {
		t.Errorf("Tier = %q, want the existing tier to be preserved", user.Tier)
	}
	if existing.LastLoginAt == nil {
		t.Error("LastLoginAt was not updated")
	}
}

func TestLoginService_Login_InvalidCredentials(t *testing.T) {
	idp := &fakeIdentityProvider{err: auth.ErrInvalidCredentials}
	svc := NewLoginService(idp, newFakeUserStore(), testSessionSecret)

	_, _, err := svc.Login(context.Background(), "jsmith", "wrong")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want auth.ErrInvalidCredentials", err)
	}
}

func TestLoginService_Login_NotInAccessGroup(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID:         "S-1-5-21-1",
		DisplayName:   "Jane Smith",
		InAccessGroup: false,
	}}
	svc := NewLoginService(idp, newFakeUserStore(), testSessionSecret)

	_, _, err := svc.Login(context.Background(), "jsmith", "password")
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Login() error = %v, want ErrAccessDenied", err)
	}
}

func TestLoginService_Login_CreateFails(t *testing.T) {
	idp := &fakeIdentityProvider{user: &auth.AuthenticatedUser{
		ADSID: "S-1-5-21-1", DisplayName: "Jane Smith", InAccessGroup: true,
	}}
	store := newFakeUserStore()
	store.createErr = errors.New("database unreachable")
	svc := NewLoginService(idp, store, testSessionSecret)

	_, _, err := svc.Login(context.Background(), "jsmith", "password")
	if err == nil {
		t.Fatal("Login() succeeded despite a user-creation failure")
	}
	if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, ErrAccessDenied) {
		t.Errorf("Login() error = %v, want a wrapped infrastructure error", err)
	}
}
