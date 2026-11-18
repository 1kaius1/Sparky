// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	flowKeyInfo = "sparky/entra/flow-cookie/v1"

	// maxReturnPath bounds the one free-text field in the saved state, so the
	// cookie it travels in stays far below the 4 KB browsers allow.
	maxReturnPath = 512

	minFlowSecret = 16
)

// Flow is the state of one sign-in between sending the person to Entra and
// their coming back: the values that tie the response to the request. It is
// kept in a short-lived cookie (SealFlow, OpenFlow). Its fields are secrets for
// the length of the sign-in - the verifier especially, since knowing it is what
// lets anyone redeem an intercepted code - so it redacts itself like Secret.
type Flow struct {
	// State is echoed back in the redirect and compared, so a response that
	// does not belong to this browser's sign-in is refused.
	State string

	// Nonce is placed in the ID token by Entra and compared, so a token issued
	// for another sign-in cannot be replayed into this one.
	Nonce string

	// Verifier is the PKCE code verifier; only its SHA-256 went to Entra.
	Verifier string

	// ReturnPath is where to send the person after a successful sign-in. This
	// package does not interpret it; the caller validates it before use.
	ReturnPath string

	// Expires is when the saved state stops being accepted.
	Expires time.Time
}

// String implements fmt.Stringer.
func (Flow) String() string { return "entra.Flow{" + redacted + "}" }

// GoString implements fmt.GoStringer.
func (Flow) GoString() string { return "entra.Flow{" + redacted + "}" }

// Format makes every verb print the placeholder; see Secret.Format.
func (Flow) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("entra.Flow{" + redacted + "}")) }

// LogValue implements slog.LogValuer.
func (Flow) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON keeps the values out of encoded output. SealFlow does not use it:
// it encodes its own wire form.
func (Flow) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// flowWire is Flow as it is encrypted; short keys keep the cookie small.
type flowWire struct {
	S string `json:"s"`
	N string `json:"n"`
	V string `json:"v"`
	R string `json:"r,omitempty"`
	E int64  `json:"e"`
}

// flowAEAD derives the cookie key from the server's session secret, so there is
// no second secret to configure, and a different purpose string means a key
// that is useless for anything else the session secret signs.
func flowAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) < minFlowSecret {
		return nil, errors.New("entra: the session secret is too short to protect sign-in state")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, flowKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("entra: derive the sign-in state key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("entra: set up the sign-in state cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("entra: set up the sign-in state cipher: %w", err)
	}
	return aead, nil
}

// SealFlow encrypts and authenticates f for a cookie value. Encrypted, not only
// signed, because it carries the PKCE verifier.
func SealFlow(secret []byte, f Flow) (string, error) {
	if len(f.ReturnPath) > maxReturnPath {
		return "", errors.New("entra: the return path is too long to save")
	}
	aead, err := flowAEAD(secret)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(flowWire{S: f.State, N: f.Nonce, V: f.Verifier, R: f.ReturnPath, E: f.Expires.Unix()})
	if err != nil {
		return "", fmt.Errorf("entra: encode the sign-in state: %w", err)
	}
	// The random nonce is generated and prepended by the cipher itself.
	sealed := aead.Seal(nil, nil, plain, []byte(flowKeyInfo))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenFlow reverses SealFlow. It returns ErrFlowInvalid for anything that is
// not a current, untampered value sealed with the same secret, and says nothing
// about which of those it was.
func OpenFlow(secret []byte, sealed string, now time.Time) (Flow, error) {
	aead, err := flowAEAD(secret)
	if err != nil {
		return Flow{}, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return Flow{}, ErrFlowInvalid
	}
	plain, err := aead.Open(nil, nil, raw, []byte(flowKeyInfo))
	if err != nil {
		return Flow{}, ErrFlowInvalid
	}
	var w flowWire
	if err := json.Unmarshal(plain, &w); err != nil {
		return Flow{}, ErrFlowInvalid
	}
	f := Flow{State: w.S, Nonce: w.N, Verifier: w.V, ReturnPath: w.R, Expires: time.Unix(w.E, 0)}
	if f.State == "" || f.Nonce == "" || f.Verifier == "" || !now.Before(f.Expires) {
		return Flow{}, ErrFlowInvalid
	}
	return f, nil
}

// randomToken returns n random bytes as unpadded base64url: 43 characters for
// 32 bytes, which is also a valid PKCE verifier (RFC 7636 allows 43 to 128
// unreserved characters).
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("entra: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challengeS256 is the PKCE S256 transform of a verifier.
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
