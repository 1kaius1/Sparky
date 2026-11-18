// SPDX-License-Identifier: AGPL-3.0-or-later

package entra

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
)

// The messages of the errors below describe what failed and never include a
// value that came from the network or the configuration (a token, an address,
// an ID): those arrive in text from Entra that can carry tenant and application
// names. A caller that wants the detail for its own log uses errors.Unwrap or
// the Description fields, deliberately.

var (
	// ErrFlowInvalid means the saved sign-in state is missing, damaged or past
	// its time limit. The person has to start again.
	ErrFlowInvalid = errors.New("entra: the sign-in state is missing, damaged or expired")

	// ErrStateMismatch means the response did not carry the state this sign-in
	// started with: a response for another sign-in, or a forged one.
	ErrStateMismatch = errors.New("entra: the sign-in response does not belong to this sign-in")

	// ErrNoCode means the response was not an error but carried no code.
	ErrNoCode = errors.New("entra: the sign-in response carried no authorization code")
)

// AuthorizationError is Entra refusing the sign-in on its own page, reported
// back through the redirect (for example access_denied when the person
// declined). Code is Entra's error value, reduced to lower-case letters, digits
// and underscore; Description is Entra's text, untrusted, for a log only.
type AuthorizationError struct {
	Code        string
	Description string
}

func (e *AuthorizationError) Error() string { return "entra: the sign-in was refused: " + e.Code }

// TokenError is the token endpoint answering with an error. Code is the OAuth
// error value (invalid_grant, invalid_client, ...), Codes the AADSTS numbers.
// Microsoft says applications should not depend on those numbers, and nothing
// here does: they are recorded to help someone read a log. Description is
// untrusted and can contain tenant and application names, so Error does not
// include it.
type TokenError struct {
	Status      int
	Code        string
	Codes       []int
	Description string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("entra: the token endpoint refused the request: %s (HTTP %d, codes %v)", e.Code, e.Status, e.Codes)
}

// ValidationError is an ID token that failed a check. Reason is one of a fixed
// set: malformed, signature, issuer, audience, expired, not_yet_valid, nonce,
// tenant, subject. The underlying error, which can quote token contents, is
// available through Unwrap and is not in the message.
type ValidationError struct {
	Reason string
	err    error
}

func (e *ValidationError) Error() string { return "entra: the ID token was rejected: " + e.Reason }

// Unwrap returns the underlying error, for a log.
func (e *ValidationError) Unwrap() error { return e.err }

// DiscoveryError is the discovery step failing. Kind is one of: unreachable,
// issuer_mismatch, bad_document, foreign_endpoint.
type DiscoveryError struct {
	Kind string
	err  error
}

func (e *DiscoveryError) Error() string { return "entra: discovery failed: " + e.Kind }

// Unwrap returns the underlying error, for a log.
func (e *DiscoveryError) Unwrap() error { return e.err }

// ProtocolError is Entra, or something in front of it, answering in a way the
// protocol does not allow. Reason is one of: unexpected_status, redirect,
// malformed_body, missing_field, too_large.
type ProtocolError struct {
	Reason string
	Status int
}

func (e *ProtocolError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("entra: unexpected response: %s (HTTP %d)", e.Reason, e.Status)
	}
	return "entra: unexpected response: " + e.Reason
}

var codeRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// cleanCode reduces an error value received from the network to something safe
// to put in a message or a report.
func cleanCode(s string) string {
	if codeRE.MatchString(s) {
		return s
	}
	return "unrecognized"
}

// ErrorKind names the kind of failure in err using a fixed vocabulary that
// contains no text from the network or the configuration, so it is safe in a
// report that is shared outside the organization. It is how the probe says what
// went wrong without quoting an error message.
func ErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	var (
		ae *AuthorizationError
		te *TokenError
		ve *ValidationError
		de *DiscoveryError
		pe *ProtocolError
	)
	switch {
	case errors.Is(err, ErrFlowInvalid):
		return "flow_state"
	case errors.Is(err, ErrStateMismatch):
		return "state_mismatch"
	case errors.Is(err, ErrNoCode):
		return "no_code"
	case errors.As(err, &ae):
		return "authorization:" + ae.Code
	case errors.As(err, &te):
		return "token_endpoint:" + te.Code
	case errors.As(err, &ve):
		return "validation:" + ve.Reason
	case errors.As(err, &pe):
		return "protocol:" + pe.Reason
	case errors.As(err, &de):
		// A discovery failure caused by the network is more useful named by
		// the network cause.
		if de.Kind == "unreachable" {
			return "discovery:unreachable/" + networkKind(de.err)
		}
		return "discovery:" + de.Kind
	}
	if k := networkKind(err); k != "other" {
		return "network:" + k
	}
	return "other"
}

// networkKind classifies a transport failure without quoting it (the message of
// a *url.Error contains the full URL, which contains the tenant ID).
func networkKind(err error) string {
	var (
		dnsErr  *net.DNSError
		certErr *tls.CertificateVerificationError
		unkAuth x509.UnknownAuthorityError
		hostErr x509.HostnameError
		invCert x509.CertificateInvalidError
		urlErr  *url.Error
		netErr  net.Error
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &certErr), errors.As(err, &unkAuth), errors.As(err, &hostErr), errors.As(err, &invCert):
		return "tls"
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return "timeout"
		}
		return "connection"
	case errors.As(err, &urlErr):
		return "connection"
	}
	return "other"
}
