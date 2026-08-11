// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
)

// credentialStore is the subset of *db.NodeRepository AuthService needs,
// narrow enough to fake in tests - same pattern as nodeStore.
type credentialStore interface {
	FindCredentialByName(ctx context.Context, name string) (*db.NodeCredential, error)
}

// ErrInvalidCredentials is returned for both an unknown node name and a
// wrong token - deliberately the same error either way, so a caller (the
// WebSocket handshake in internal/agentconn) can't be used to enumerate
// registered node names.
var ErrInvalidCredentials = errors.New("invalid node credentials")

// AuthService verifies a node's bearer token at connect time - see
// ARCHITECTURE.md Protocol. Distinct from Service: RegisterNode is
// RBAC-gated and called by an authenticated human Actor, while
// Authenticate has no Actor at all - the caller is the node itself,
// presenting the credential SCHEMA.md Nodes' bearer_token_hash exists to
// verify.
type AuthService struct {
	credentials credentialStore
}

// NewAuthService constructs an AuthService.
func NewAuthService(credentials credentialStore) *AuthService {
	return &AuthService{credentials: credentials}
}

// Authenticate verifies name and token against the stored credential,
// returning the node on success.
func (s *AuthService) Authenticate(ctx context.Context, name, token string) (*db.Node, error) {
	cred, err := s.credentials.FindCredentialByName(ctx, name)
	if errors.Is(err, db.ErrNodeNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("find node credential: %w", err)
	}

	if !auth.VerifyNodeToken(token, cred.BearerTokenHash) {
		return nil, ErrInvalidCredentials
	}

	return &cred.Node, nil
}
