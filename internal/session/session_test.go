// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"testing"
	"time"
)

const testSecret = "test-session-secret-value"

func TestSignVerify_RoundTrip(t *testing.T) {
	s := New("user-123", time.Hour)

	cookieValue, err := Sign(testSecret, s)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	got, err := Verify(testSecret, cookieValue)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if got.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", got.UserID, "user-123")
	}
	if !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, s.ExpiresAt)
	}
}

func TestSignVerify_SuperAdmin_RoundTrip(t *testing.T) {
	s := NewSuperAdmin(time.Hour)

	cookieValue, err := Sign(testSecret, s)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	got, err := Verify(testSecret, cookieValue)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if !got.IsSuperAdmin {
		t.Error("IsSuperAdmin = false, want true")
	}
	if got.UserID != "" {
		t.Errorf("UserID = %q, want empty for a SuperAdmin session", got.UserID)
	}
}

func TestVerify_Expired(t *testing.T) {
	s := New("user-123", -time.Hour) // already expired

	cookieValue, err := Sign(testSecret, s)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	_, err = Verify(testSecret, cookieValue)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify() error = %v, want ErrInvalid", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	s := New("user-123", time.Hour)

	cookieValue, err := Sign(testSecret, s)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	_, err = Verify("a-completely-different-secret", cookieValue)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify() error = %v, want ErrInvalid", err)
	}
}

func TestVerify_TamperedPayload(t *testing.T) {
	s := New("user-123", time.Hour)

	cookieValue, err := Sign(testSecret, s)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	encodedPayload, signature, ok := splitOnce(cookieValue, '.')
	if !ok {
		t.Fatal("test setup: cookie value has no separator")
	}
	tampered := encodedPayload + "x." + signature

	_, err = Verify(testSecret, tampered)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify() error = %v, want ErrInvalid", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	tests := []string{
		"",
		"no-separator-at-all",
		".",
		"not-valid-base64!!!.also-not-base64!!!",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			_, err := Verify(testSecret, tt)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Verify(%q) error = %v, want ErrInvalid", tt, err)
			}
		})
	}
}
