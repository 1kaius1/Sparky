// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/base64"
	"encoding/json"
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
	if !got.LastVerifiedAt.Equal(s.LastVerifiedAt) {
		t.Errorf("LastVerifiedAt = %v, want %v", got.LastVerifiedAt, s.LastVerifiedAt)
	}
}

func TestNew_SetsLastVerifiedAt(t *testing.T) {
	before := time.Now().UTC()
	s := New("user-123", time.Hour)
	after := time.Now().UTC()

	if s.LastVerifiedAt.Before(before) || s.LastVerifiedAt.After(after) {
		t.Errorf("LastVerifiedAt = %v, want between %v and %v", s.LastVerifiedAt, before, after)
	}
}

// TestVerify_LegacyTokenWithoutLastVerifiedAt confirms a session signed
// before LastVerifiedAt existed (no "lva" field in the payload at all, not
// just an empty one) still verifies successfully - the field decodes to its
// zero value, which RequireSession's staleness check treats as "immediately
// due for a recheck" rather than a decode failure.
func TestVerify_LegacyTokenWithoutLastVerifiedAt(t *testing.T) {
	legacyPayload := struct {
		UserID    string    `json:"uid"`
		ExpiresAt time.Time `json:"exp"`
	}{
		UserID:    "user-123",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	raw, err := json.Marshal(legacyPayload)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(raw)
	cookieValue := encodedPayload + "." + sign([]byte(testSecret), encodedPayload)

	got, err := Verify(testSecret, cookieValue)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if got.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", got.UserID, "user-123")
	}
	if !got.LastVerifiedAt.IsZero() {
		t.Errorf("LastVerifiedAt = %v, want the zero value for a legacy token", got.LastVerifiedAt)
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
