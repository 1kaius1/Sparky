// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"strings"
	"testing"
)

func TestGenerateInitialPassword_NoTokenPrefix(t *testing.T) {
	password, err := GenerateInitialPassword()
	if err != nil {
		t.Fatalf("GenerateInitialPassword() error: %v", err)
	}
	if strings.HasPrefix(password, nodeTokenPrefix) {
		t.Errorf("password = %q, should not carry the node-token prefix %q", password, nodeTokenPrefix)
	}
}

func TestGenerateInitialPassword_Unique(t *testing.T) {
	a, err := GenerateInitialPassword()
	if err != nil {
		t.Fatalf("GenerateInitialPassword() error: %v", err)
	}
	b, err := GenerateInitialPassword()
	if err != nil {
		t.Fatalf("GenerateInitialPassword() error: %v", err)
	}
	if a == b {
		t.Error("two calls to GenerateInitialPassword() produced the same password")
	}
}

func TestGenerateInitialPassword_HashAndVerifyRoundTrip(t *testing.T) {
	password, err := GenerateInitialPassword()
	if err != nil {
		t.Fatalf("GenerateInitialPassword() error: %v", err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false, want true for the correct password")
	}
}
