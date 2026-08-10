// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/1kaius1/Sparky/internal/session"
)

// API holds the dependencies HTTP handlers need.
type API struct {
	loginService  *LoginService
	sessionSecret string
}

// New constructs an API. sessionSecret is used to verify session cookies
// on protected routes via RequireSession - login/logout themselves go
// through LoginService, which holds its own copy for signing.
func New(loginService *LoginService, sessionSecret string) *API {
	return &API{loginService: loginService, sessionSecret: sessionSecret}
}

// Router builds the full route tree. Per ARCHITECTURE.md Application
// Lifecycle, request ID, logging, recovery, auth, and audit middleware are
// registered here; only request ID and recovery exist yet - logging and
// audit middleware, and every route beyond login/logout, are later v0.1.0
// work (RBAC, Dashboard UI).
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(setRequestIDHeader)
	r.Use(middleware.Recoverer)

	r.Post("/login", a.handleLogin)
	r.Post("/logout", a.handleLogout)

	return r
}

// setRequestIDHeader writes chi's per-request ID as the X-Request-ID
// response header - see CLAUDE.md API Conventions: "All responses include
// an X-Request-ID header for log correlation."
func setRequestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", middleware.GetReqID(r.Context()))
		next.ServeHTTP(w, r)
	})
}

type contextKey string

const userIDContextKey contextKey = "sparky_user_id"

// RequireSession verifies the session cookie and stores the authenticated
// user's ID in the request context, responding 401 if it is missing or
// invalid. Not yet used by any route in this package - future protected
// routes (RBAC-gated actions, the Dashboard UI) will register through it.
func (a *API) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
			return
		}

		sess, err := session.Verify(a.sessionSecret, cookie.Value)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid or expired session")
			return
		}

		ctx := context.WithValue(r.Context(), userIDContextKey, sess.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UserIDFromContext returns the authenticated user's ID stored by
// RequireSession, if any.
func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDContextKey).(string)
	return id, ok
}
