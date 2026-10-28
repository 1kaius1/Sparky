// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"fmt"
	"log/slog"
	"time"
)

const redacted = "[redacted]"

// Secret is text that must never reach a log line, an error message or a
// report: the app registration's client secret. It prints as "[redacted]"
// under every fmt verb, as JSON and as a slog value, so a stray %v or a
// structured log of a config struct cannot leak it. Reveal is the one
// deliberate way to read it.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, which %#v uses.
func (Secret) GoString() string { return `entra.Secret("` + redacted + `")` }

// Format makes every verb print the placeholder. String alone is not enough:
// fmt calls it only for the verbs that suit a string, and for a wrong one such
// as %d it prints the raw value of a string-kind type instead.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON keeps the value out of encoded output.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText keeps the value out of text encodings.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the secret text. Call it only where the value is sent to the
// token endpoint.
func (s Secret) Reveal() string { return string(s) }

// AccessToken is a bearer token from the client credentials flow. It redacts
// itself for the same reason Secret does, and unlike Secret it is a struct, so
// the value sits in an unexported field that fmt cannot call methods on:
// without the methods below, %+v would print it in clear.
type AccessToken struct {
	value Secret

	// ExpiresIn is how long Entra said the token is valid, from the moment it
	// was received.
	ExpiresIn time.Duration
}

// Bearer returns the token text for an Authorization header.
func (t AccessToken) Bearer() string { return t.value.Reveal() }

// String implements fmt.Stringer.
func (t AccessToken) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (t AccessToken) GoString() string { return "entra.AccessToken{" + redacted + "}" }

// Format makes every verb, including %+v and %x, print the placeholder.
func (t AccessToken) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON keeps the value out of encoded output.
func (t AccessToken) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue implements slog.LogValuer.
func (t AccessToken) LogValue() slog.Value { return slog.StringValue(redacted) }
