// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/session"
)

// localUserStore is the subset of *db.UserRepository this package needs for
// local-account login, narrow enough to fake in tests - same pattern as
// userStore.
type localUserStore interface {
	FindByLocalUsername(ctx context.Context, username string) (*db.User, string, error)
}

// LocalLoginService authenticates a local-only account (SCHEMA.md Users'
// Local-only accounts subsection) - a distinct path from LoginService's
// LDAP flow and BreakGlassLoginService's own single-credential flow, since
// a local account is an ordinary RBAC-tiered Users row but has no AD
// identity behind it at all.
type LocalLoginService struct {
	users         localUserStore
	sessionSecret string
}

// NewLocalLoginService constructs a LocalLoginService.
func NewLocalLoginService(users localUserStore, sessionSecret string) *LocalLoginService {
	return &LocalLoginService{users: users, sessionSecret: sessionSecret}
}

// Login verifies username/password against the stored local-account
// credential and, on success, returns the corresponding user record and a
// signed local-account session cookie value. An unknown username is
// treated the same as a wrong password - this path never reveals whether a
// given username exists, same posture as BreakGlassLoginService's own
// never-configured-credential handling.
func (s *LocalLoginService) Login(ctx context.Context, username, password string) (*db.User, string, error) {
	user, hash, err := s.users.FindByLocalUsername(ctx, username)
	if errors.Is(err, db.ErrUserNotFound) {
		return nil, "", auth.ErrInvalidCredentials
	}
	if err != nil {
		return nil, "", fmt.Errorf("find local user: %w", err)
	}

	ok, err := auth.VerifyPassword(password, hash)
	if err != nil {
		return nil, "", fmt.Errorf("verify local password: %w", err)
	}
	if !ok {
		return nil, "", auth.ErrInvalidCredentials
	}

	cookieValue, err := session.Sign(s.sessionSecret, session.NewLocal(user.ID, sessionDuration))
	if err != nil {
		return nil, "", fmt.Errorf("sign session: %w", err)
	}
	return user, cookieValue, nil
}
