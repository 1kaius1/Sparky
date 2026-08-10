// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for the break-glass credential specifically - see
// SCHEMA.md Break-glass credential and PLANNING.md's 2026-08-10 decision.
// Stronger than a typical per-login hash would need, since this path is
// used rarely (emergency recovery), not on every request.
const (
	argon2Time    uint32 = 3
	argon2Memory  uint32 = 64 * 1024 // KiB (64 MiB)
	argon2Threads uint8  = 4
	argon2KeyLen  uint32 = 32
	argon2SaltLen        = 16
)

// ErrMalformedHash is returned when VerifyPassword is given a string that
// isn't in the format HashPassword produces.
var ErrMalformedHash = errors.New("malformed password hash")

// HashPassword derives an Argon2id hash for password, encoded in a
// PHC-string-like form: $argon2id$v=<version>$m=<mem>,t=<time>,p=<threads>$<salt>$<hash>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword checks password against an encoded hash produced by
// HashPassword, using the parameters embedded in the hash itself rather
// than the package's current defaults - a hash made with older parameters
// still verifies correctly even if argon2Time/Memory/Threads change later.
func VerifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrMalformedHash
	}
	if version != argon2.Version {
		return false, ErrMalformedHash
	}

	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false, ErrMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	wantHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrMalformedHash
	}

	gotHash := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(wantHash)))

	return subtle.ConstantTimeCompare(gotHash, wantHash) == 1, nil
}
