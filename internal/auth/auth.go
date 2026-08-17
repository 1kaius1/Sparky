// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth authenticates a user's credentials and resolves their
// login-gate group membership, behind the IdentityProvider interface - see
// ARCHITECTURE.md Auth & Identity Provider. LDAP is the only implementation
// today; an OIDC/Entra ID implementation can be added later without
// touching anything downstream of "authenticated user + resolved group
// memberships."
package auth

import (
	"context"
	"errors"
)

// AuthenticatedUser is what any IdentityProvider returns after successfully
// verifying a user's credentials.
type AuthenticatedUser struct {
	// ADSID is the AD SID (e.g. "S-1-5-21-...") - the external identity
	// reference stored on Users.ad_sid, see SCHEMA.md Users.
	ADSID string

	DisplayName string

	// InAccessGroup reports whether the user is a member of the dedicated
	// login-gate AD group (LDAP_ACCESS_GROUP_DN), resolved server-side
	// including nested membership. This package only resolves the
	// membership; it does not decide whether a non-member is allowed to
	// proceed - see ARCHITECTURE.md RBAC & Permission Overrides for where
	// that decision belongs.
	InAccessGroup bool

	// DN is the user's LDAP distinguishedName, cached on Users.ldap_dn so a
	// later mid-session group-membership recheck (IsInAccessGroup below) can
	// identify the same directory entry without needing the user's password
	// again - see PLANNING.md Decisions Log for why DN was chosen over a
	// binary objectSid filter.
	DN string
}

// ErrInvalidCredentials covers both "no such user" and "wrong password" -
// deliberately not distinguished, so a caller can never use this package to
// enumerate valid usernames.
var ErrInvalidCredentials = errors.New("invalid credentials")

// IdentityProvider authenticates a user's credentials and resolves their
// login-gate group membership.
type IdentityProvider interface {
	Authenticate(ctx context.Context, username, password string) (*AuthenticatedUser, error)

	// IsInAccessGroup re-checks whether dn (AuthenticatedUser.DN, cached at
	// login) is still a member of the login-gate group - no password
	// required, used for a mid-session recheck rather than a fresh login.
	IsInAccessGroup(ctx context.Context, dn string) (bool, error)
}
