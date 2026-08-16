// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// csrfCookieName holds the double-submit CSRF token - see csrf.go's own
// package doc comment below for the overall design. csrfHeaderName is what
// htmx-driven requests (base.html's hx-headers) present it as;
// csrfFormFieldName is what a classic <form method="post"> presents it as.
const (
	csrfCookieName    = "sparky_csrf"
	csrfHeaderName    = "X-CSRF-Token"
	csrfFormFieldName = "csrf_token"
)

// csrfTokenByteLen is the random token's size before base64 encoding - same
// 256-bit strength as internal/auth.GenerateNodeToken.
const csrfTokenByteLen = 32

// generateCSRFToken returns a random, URL-safe token.
func generateCSRFToken() (string, error) {
	b := make([]byte, csrfTokenByteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setCSRFCookie mirrors setSessionCookie's attributes exactly (handlers.go)
// - HttpOnly is safe here specifically because the server itself (not
// client-side JS) is what echoes the token back into the page (a hidden
// form field or the hx-headers attribute, both rendered server-side from
// csrfTokenFromContext) - a forged cross-site request gets this cookie
// attached automatically but can never read the page to learn the token to
// include in its own forged submission.
func setCSRFCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionDuration.Seconds()),
	})
}

const csrfTokenContextKey contextKey = "sparky_csrf_token"

// csrfTokenFromContext returns the token ensureCSRFToken stashed for this
// request, if any - used when rendering a page to embed the token a
// subsequent submission must echo back.
func csrfTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(csrfTokenContextKey).(string)
	return token
}

// ensureCSRFToken is global middleware (see router.go's Router - registered
// right after setupGate, before any route) that guarantees every request,
// including a plain GET that will render a form, has a CSRF token available
// to embed in the page it's about to serve. A missing or malformed cookie
// gets a freshly generated one; either way the plaintext value is stashed
// into the request context for render()/the login and break-glass page
// handlers to read. This has to run globally, not just on write routes -
// unlike RequireCSRF (which only matters once a form is submitted), the
// token needs to already exist by the time the form is first rendered.
func (a *API) ensureCSRFToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if cookie, err := r.Cookie(csrfCookieName); err == nil && cookie.Value != "" {
			token = cookie.Value
		} else {
			generated, err := generateCSRFToken()
			if err != nil {
				a.logger.Printf("httpapi: generate csrf token: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			token = generated
			setCSRFCookie(w, r, token)
		}

		ctx := context.WithValue(r.Context(), csrfTokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireCSRF rejects a state-changing request whose submitted token
// doesn't match the sparky_csrf cookie ensureCSRFToken already guaranteed
// exists - the double-submit check itself. Applied explicitly per write
// route (router.go), matching how RequireSession and the break-glass IP
// whitelist are already wired, rather than as an implicit global.
//
// Skipped entirely for a non-form request (isFormRequest, login_page.go) -
// covers both /login and /login/break-glass's JSON API branch, which isn't
// triggerable by a naive cross-site HTML form in the first place (a simple
// form submission can't set Content-Type: application/json without
// JavaScript and a CORS preflight, itself a standard CSRF mitigation), so
// there is nothing here for a stolen/forged form submission to exploit.
//
// The submitted token is read from the X-CSRF-Token header first (what
// base.html's hx-headers attribute sends for every htmx-driven write -
// logout, tier-change, load, unload), falling back to the csrf_token form
// field (what the four classic <form method="post"> writes send - login,
// break-glass login, node registration, profile create/edit).
// r.ParseForm() is safe to call even if a handler downstream also calls it
// - Go's net/http caches the parsed form after the first call.
func (a *API) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isFormRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(csrfCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, r, http.StatusForbidden, "CSRF_INVALID", "missing or invalid CSRF token")
			return
		}

		submitted := r.Header.Get(csrfHeaderName)
		if submitted == "" {
			if err := r.ParseForm(); err != nil {
				writeError(w, r, http.StatusForbidden, "CSRF_INVALID", "missing or invalid CSRF token")
				return
			}
			submitted = r.PostFormValue(csrfFormFieldName)
		}

		if submitted == "" || subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie.Value)) != 1 {
			writeError(w, r, http.StatusForbidden, "CSRF_INVALID", "missing or invalid CSRF token")
			return
		}

		next.ServeHTTP(w, r)
	})
}
