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

// breakGlassStore is the subset of *db.BreakGlassRepository this package
// needs, narrow enough to fake in tests - same pattern as userStore.
type breakGlassStore interface {
	Get(ctx context.Context) (*db.BreakGlassCredential, error)
}

// BreakGlassLoginService authenticates the SuperAdmin break-glass
// credential - a distinct path from LoginService's LDAP flow, since the
// SuperAdmin is not an LDAP/AD identity and is not a Users row at all -
// see SCHEMA.md Break-glass credential.
type BreakGlassLoginService struct {
	credentials   breakGlassStore
	sessionSecret string
}

// NewBreakGlassLoginService constructs a BreakGlassLoginService.
func NewBreakGlassLoginService(credentials breakGlassStore, sessionSecret string) *BreakGlassLoginService {
	return &BreakGlassLoginService{credentials: credentials, sessionSecret: sessionSecret}
}

// Login verifies password against the configured break-glass credential
// and, on success, returns a signed SuperAdmin session cookie value. A
// never-configured credential (ErrBreakGlassNotSet) is treated the same as
// a wrong password, so this path never reveals whether break-glass access
// has been set up at all.
func (s *BreakGlassLoginService) Login(ctx context.Context, password string) (string, error) {
	cred, err := s.credentials.Get(ctx)
	if errors.Is(err, db.ErrBreakGlassNotSet) {
		return "", auth.ErrInvalidCredentials
	}
	if err != nil {
		return "", fmt.Errorf("get break-glass credential: %w", err)
	}

	ok, err := auth.VerifyPassword(password, cred.PasswordHash)
	if err != nil {
		return "", fmt.Errorf("verify break-glass password: %w", err)
	}
	if !ok {
		return "", auth.ErrInvalidCredentials
	}

	cookieValue, err := session.Sign(s.sessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		return "", fmt.Errorf("sign session: %w", err)
	}
	return cookieValue, nil
}
