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
	loginService           *LoginService
	breakGlassLoginService *BreakGlassLoginService
	setupGate              *setupGate
	sessionSecret          string
	agentConn              http.Handler
}

// New constructs an API. sessionSecret is used to verify session cookies
// on protected routes via RequireSession - login/logout themselves go
// through LoginService/BreakGlassLoginService, which hold their own copies
// for signing. breakGlassStore gates every route via setupGate until
// first-run setup has completed - see setup_gate.go. agentConn is
// internal/agentconn's WebSocket endpoint (ARCHITECTURE.md's
// Agent-Communication Layer) - it is a plain http.Handler here, not a
// concrete type, so this package doesn't need to depend on
// internal/agentconn's other exports.
func New(loginService *LoginService, breakGlassLoginService *BreakGlassLoginService, breakGlassStore breakGlassStore, sessionSecret string, agentConn http.Handler) *API {
	return &API{
		loginService:           loginService,
		breakGlassLoginService: breakGlassLoginService,
		setupGate:              newSetupGate(breakGlassStore),
		sessionSecret:          sessionSecret,
		agentConn:              agentConn,
	}
}

// Router builds the full route tree. Per ARCHITECTURE.md Application
// Lifecycle, request ID, logging, recovery, the Setup Check, auth, and
// audit middleware are registered here; logging and audit middleware, and
// every route beyond login/logout, are later v0.1.0 work (RBAC, Dashboard
// UI).
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(setRequestIDHeader)
	r.Use(middleware.Recoverer)
	r.Use(a.setupGate.middleware)

	r.Post("/login", a.handleLogin)
	r.Post("/login/break-glass", a.handleBreakGlassLogin)
	r.Post("/logout", a.handleLogout)

	// Outside /api/v1: this isn't a REST endpoint (CLAUDE.md API
	// Conventions) - it's a WebSocket upgrade, authenticated by the
	// node's own bearer token (ARCHITECTURE.md Protocol), not a session
	// cookie or RequireSession. r.Method (not r.Get) takes the
	// http.Handler interface directly rather than eagerly binding a
	// method value at route-registration time - a nil agentConn (as in
	// this package's own tests, which don't exercise this route) would
	// otherwise panic building the router at all, not just on a request.
	r.Method(http.MethodGet, "/agent/connect", a.agentConn)

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

const identityContextKey contextKey = "sparky_identity"

// Identity is what RequireSession stores in the request context. UserID
// is empty when IsSuperAdmin is true, mirroring internal/session.Session
// and internal/rbac.Actor.
type Identity struct {
	UserID       string
	IsSuperAdmin bool
}

// RequireSession verifies the session cookie and stores the authenticated
// Identity in the request context, responding 401 if it is missing or
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

		identity := Identity{UserID: sess.UserID, IsSuperAdmin: sess.IsSuperAdmin}
		ctx := context.WithValue(r.Context(), identityContextKey, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdentityFromContext returns the Identity stored by RequireSession, if
// any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(Identity)
	return id, ok
}
