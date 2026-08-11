// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"strings"
	"testing"
)

func TestGenerateNodeToken_HasPrefix(t *testing.T) {
	token, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	if !strings.HasPrefix(token, nodeTokenPrefix) {
		t.Errorf("token = %q, want prefix %q", token, nodeTokenPrefix)
	}
}

func TestGenerateNodeToken_Unique(t *testing.T) {
	a, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	b, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	if a == b {
		t.Error("two calls to GenerateNodeToken() produced the same token")
	}
}

func TestHashVerifyNodeToken_RoundTrip(t *testing.T) {
	token, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	hash := HashNodeToken(token)

	if !VerifyNodeToken(token, hash) {
		t.Error("VerifyNodeToken() = false, want true for the correct token")
	}
}

func TestVerifyNodeToken_WrongToken(t *testing.T) {
	token, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	hash := HashNodeToken(token)

	other, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	if VerifyNodeToken(other, hash) {
		t.Error("VerifyNodeToken() = true, want false for a different token")
	}
}

func TestHashNodeToken_Deterministic(t *testing.T) {
	token, err := GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	if HashNodeToken(token) != HashNodeToken(token) {
		t.Error("HashNodeToken() produced different output for the same input across calls")
	}
}
