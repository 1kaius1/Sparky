// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpapi is the HTTP layer: router, middleware, and handlers. Per
// CLAUDE.md HTTP and API Conventions, handlers stay thin (parse, delegate,
// respond); the actual login orchestration lives in LoginService below, so
// it can be tested independently of any HTTP concerns.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/session"
)

// sessionDuration bounds how long a session cookie is valid before the
// user must log in again.
const sessionDuration = 24 * time.Hour

// ErrAccessDenied is returned when a user's credentials are valid but they
// are not a member of the login-gate AD group - see ARCHITECTURE.md Auth &
// Identity Provider.
var ErrAccessDenied = errors.New("not a member of the access group")

// userStore is the subset of *db.UserRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/auth's ldapConn.
type userStore interface {
	FindByADSID(ctx context.Context, adSID string) (*db.User, error)
	Create(ctx context.Context, adSID, displayName string, tier db.Tier) (*db.User, error)
	UpdateLastLogin(ctx context.Context, id string, at time.Time) error
}

// LoginService orchestrates a login attempt: authenticate against the
// identity provider, enforce the login-gate group check (Auth & Identity
// Provider's job, not RBAC's - see ARCHITECTURE.md), and provision or
// update the corresponding Users row.
type LoginService struct {
	identityProvider auth.IdentityProvider
	users            userStore
	sessionSecret    string
}

// NewLoginService constructs a LoginService.
func NewLoginService(identityProvider auth.IdentityProvider, users userStore, sessionSecret string) *LoginService {
	return &LoginService{
		identityProvider: identityProvider,
		users:            users,
		sessionSecret:    sessionSecret,
	}
}

// Login authenticates username/password and, on success, returns the
// corresponding user record and a signed session cookie value. A
// first-time login creates the user at TierReadOnly - see SCHEMA.md Users,
// Elevation rules, where Read-only is the floor an Admin promotes up from.
func (s *LoginService) Login(ctx context.Context, username, password string) (*db.User, string, error) {
	authUser, err := s.identityProvider.Authenticate(ctx, username, password)
	if err != nil {
		return nil, "", err
	}

	if !authUser.InAccessGroup {
		return nil, "", ErrAccessDenied
	}

	user, err := s.users.FindByADSID(ctx, authUser.ADSID)
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		user, err = s.users.Create(ctx, authUser.ADSID, authUser.DisplayName, db.TierReadOnly)
		if err != nil {
			return nil, "", fmt.Errorf("create user: %w", err)
		}
	case err != nil:
		return nil, "", fmt.Errorf("find user: %w", err)
	default:
		if err := s.users.UpdateLastLogin(ctx, user.ID, time.Now().UTC()); err != nil {
			return nil, "", fmt.Errorf("update last login: %w", err)
		}
	}

	cookieValue, err := session.Sign(s.sessionSecret, session.New(user.ID, sessionDuration))
	if err != nil {
		return nil, "", fmt.Errorf("sign session: %w", err)
	}

	return user, cookieValue, nil
}
