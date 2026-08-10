// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session implements signed cookie sessions - see CLAUDE.md Tech
// Stack. The cookie holds a small signed, base64-encoded payload (user ID
// and expiry); it is not encrypted, since it contains no secret data, only
// a reference to the user record everything else is looked up from. Hand-
// rolled rather than a library, matching the "own it, don't add
// dependencies you don't need" reasoning already recorded for chi - HMAC
// signing and expiry checking is a small amount of well-understood code.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalid is returned for any malformed, unsigned, or expired cookie
// value - deliberately not distinguishing which, so a caller can never use
// this package's errors to probe for why verification failed.
var ErrInvalid = errors.New("invalid session")

// Session is the payload signed into the cookie.
type Session struct {
	UserID    string    `json:"uid"`
	ExpiresAt time.Time `json:"exp"`
}

// New creates a session for userID, valid for the given duration from now.
func New(userID string, duration time.Duration) Session {
	return Session{
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(duration),
	}
}

// Sign encodes and HMAC-signs a session into a cookie value, in the form
// "<base64-payload>.<base64-signature>".
func Sign(secret string, s Session) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal session: %w", err)
	}

	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := sign([]byte(secret), encodedPayload)

	return encodedPayload + "." + signature, nil
}

// Verify checks a cookie value's signature and expiry, returning the
// session it encodes if valid.
func Verify(secret, cookieValue string) (*Session, error) {
	encodedPayload, signature, ok := splitOnce(cookieValue, '.')
	if !ok {
		return nil, ErrInvalid
	}

	wantSignature := sign([]byte(secret), encodedPayload)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(wantSignature)) != 1 {
		return nil, ErrInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, ErrInvalid
	}

	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, ErrInvalid
	}

	if time.Now().UTC().After(s.ExpiresAt) {
		return nil, ErrInvalid
	}

	return &s, nil
}

func sign(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func splitOnce(s string, sep byte) (before, after string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
