// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// initialPasswordRandomBytes is 256 bits - same entropy budget as
// GenerateNodeToken, though the threat model here is different (a
// human-typed-once secret, not a machine-held bearer token).
const initialPasswordRandomBytes = 32

// GenerateInitialPassword returns a new random password in plaintext, for
// the one-time bootstrap of the default local "admin" account
// (cmd/sparky-server's setup subcommand). Unlike GenerateNodeToken, this
// carries no API-token-style prefix - it's a password a person reads off a
// terminal and types into a login form once, not a credential identified by
// its shape in logs or config files. The caller hashes it with HashPassword
// before persisting, same as any other local-account password.
func GenerateInitialPassword() (string, error) {
	buf := make([]byte, initialPasswordRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate initial password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
