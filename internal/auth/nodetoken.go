// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// nodeTokenPrefix marks a node bearer token as such at a glance in logs or
// config files - standard API-token UX (GitHub's ghp_, Stripe's sk_).
const nodeTokenPrefix = "spk_"

// nodeTokenRandomBytes is 256 bits - large enough that brute-forcing the
// token itself, not just its hash, is infeasible.
const nodeTokenRandomBytes = 32

// GenerateNodeToken returns a new random node bearer token in plaintext.
// The caller is responsible for hashing it (HashNodeToken) before
// persisting anything - see SCHEMA.md Nodes' bearer_token_hash.
func GenerateNodeToken() (string, error) {
	buf := make([]byte, nodeTokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate node token: %w", err)
	}
	return nodeTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashNodeToken hashes a node bearer token for storage. Unlike
// HashPassword, this is a single fast SHA-256 pass, not Argon2id: a
// memory-hard KDF defends against brute-forcing a human-chosen,
// low-entropy password, but GenerateNodeToken's output is already a
// uniformly random 256-bit value - slowing down the hash buys nothing
// against brute force and would only add latency to every agent
// reconnect handshake (ARCHITECTURE.md Protocol).
func HashNodeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyNodeToken reports whether token hashes to hash, in constant time.
func VerifyNodeToken(token, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashNodeToken(token)), []byte(hash)) == 1
}
