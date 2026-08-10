// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"testing"
)

func TestHashVerifyPassword_RoundTrip(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}

	ok, err := VerifyPassword("correct-horse-battery-staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false, want true for the correct password")
	}
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}

	ok, err := VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error: %v", err)
	}
	if ok {
		t.Error("VerifyPassword() = true, want false for the wrong password")
	}
}

func TestHashPassword_DifferentSaltsPerCall(t *testing.T) {
	hash1, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	hash2, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}

	if hash1 == hash2 {
		t.Error("HashPassword() produced identical output for two calls with the same password - salt is not random")
	}

	// Both must still verify correctly despite differing.
	for _, h := range []string{hash1, hash2} {
		ok, err := VerifyPassword("same-password", h)
		if err != nil || !ok {
			t.Errorf("VerifyPassword(%q) = %v, %v, want true, nil", h, ok, err)
		}
	}
}

func TestVerifyPassword_MalformedHash(t *testing.T) {
	tests := []string{
		"",
		"not-a-hash-at-all",
		"$argon2id$v=19$m=65536,t=3,p=4$onlyfourparts",
		"$bcrypt$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=999$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$garbage$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$not!valid!base64$aGFzaA",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			_, err := VerifyPassword("anything", tt)
			if !errors.Is(err, ErrMalformedHash) {
				t.Errorf("VerifyPassword(%q) error = %v, want ErrMalformedHash", tt, err)
			}
		})
	}
}
