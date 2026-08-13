// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/1kaius1/Sparky/internal/session"
	"github.com/1kaius1/Sparky/web"
)

// API holds the dependencies HTTP handlers need.
type API struct {
	loginService           *LoginService
	breakGlassLoginService *BreakGlassLoginService
	setupGate              *setupGate
	sessionSecret          string
	agentConn              http.Handler

	nodes      nodeLister
	profiles   profileLister
	instances  instanceLister
	transfers  transferLister
	users      userLister
	audit      auditLister
	userRoster userRoster
	settings   settingsViewer
	metrics    metricsLister
	templates  map[string]*template.Template
	static     http.Handler
	logger     *log.Logger
}

// New constructs an API. sessionSecret is used to verify session cookies
// on protected routes via RequireSession - login/logout themselves go
// through LoginService/BreakGlassLoginService, which hold their own copies
// for signing. breakGlassStore gates every route via setupGate until
// first-run setup has completed - see setup_gate.go. agentConn is
// internal/agentconn's WebSocket endpoint (ARCHITECTURE.md's
// Agent-Communication Layer) - it is a plain http.Handler here, not a
// concrete type, so this package doesn't need to depend on
// internal/agentconn's other exports. nodes/profiles/instances/transfers
// back the Dashboard UI's Read-only-tier pages (Dashboard, Nodes, Model
// profiles, Transfers); users/audit back the Audit log page, the first
// whose sidebar tier sits above Read-only (CLAUDE.md Frontend
// Conventions: "Audit log (Admin)") - users resolves both the viewer's own
// tier for that RBAC check and each audit record's actor_id to a display
// name (see PLANNING.md's Dashboard UI milestone item); roster backs the
// Users & permissions page (same Admin floor as Audit log) via
// rbac.Service.ListUsers - a distinct, RBAC-gated dependency from users,
// since that page exposes the full roster itself rather than resolving an
// already-permitted record's actor_id; settingsSvc backs the Settings
// page (same Admin floor) via internal/settings.Service.Get, covering the
// two singleton config rows neither internal/metrics nor internal/audit
// owns - see that package's doc comment; metricsSvc backs the Metrics
// page, back at the Read-only floor like nodes/profiles/instances/
// transfers - unlike audit/roster/settingsSvc, no RBAC check is involved;
// logger is used for rendering/query failures a handler can't turn into a
// useful HTTP response on its own. Returns an error if the embedded
// templates (web.FS) fail to parse - a template syntax error is a
// build-time bug, caught here rather than surfacing as a broken page on
// first request.
func New(loginService *LoginService, breakGlassLoginService *BreakGlassLoginService, breakGlassStore breakGlassStore, sessionSecret string, agentConn http.Handler,
	nodes nodeLister, profiles profileLister, instances instanceLister, transfers transferLister, users userLister, auditLog auditLister, roster userRoster, settingsSvc settingsViewer, metricsSvc metricsLister, logger *log.Logger) (*API, error) {
	templates, err := loadPageTemplates()
	if err != nil {
		return nil, fmt.Errorf("load page templates: %w", err)
	}
	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, fmt.Errorf("sub static FS: %w", err)
	}

	return &API{
		loginService:           loginService,
		breakGlassLoginService: breakGlassLoginService,
		setupGate:              newSetupGate(breakGlassStore),
		sessionSecret:          sessionSecret,
		agentConn:              agentConn,
		nodes:                  nodes,
		profiles:               profiles,
		instances:              instances,
		transfers:              transfers,
		users:                  users,
		audit:                  auditLog,
		userRoster:             roster,
		settings:               settingsSvc,
		metrics:                metricsSvc,
		templates:              templates,
		static:                 http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
		logger:                 logger,
	}, nil
}

// Router builds the full route tree. Per ARCHITECTURE.md Application
// Lifecycle, request ID, logging, recovery, the Setup Check, auth, and
// audit middleware are registered here; logging and audit middleware
// beyond login/logout are later v0.1.0 work (RBAC-gated write actions -
// see PLANNING.md's Dashboard UI milestone item for what this phase
// covers: three read-only pages, no write/action routes yet).
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(setRequestIDHeader)
	r.Use(middleware.Recoverer)
	r.Use(a.setupGate.middleware)

	// GET /login serves the HTML login form (Dashboard UI Phase 2);
	// POST /login itself now serves two callers - the JSON API/htmx
	// contract (unchanged) and this form's own browser submission - see
	// isFormRequest in login_page.go.
	r.Get("/login", a.handleLoginPage)
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

	// Dashboard UI - server-rendered pages, not /api/v1 REST/JSON (CLAUDE.md
	// API Conventions reserves that base path for actions; these render
	// HTML). Read-only for every authenticated session regardless of tier
	// (CLAUDE.md Frontend Conventions: Dashboard/Nodes/Model profiles/
	// Transfers all have "Read-only" as their minimum visible tier), so
	// RequireSession is the only gate - no RBAC check, matching
	// internal/nodes.Service.ListNodes et al.'s own "unguarded by RBAC"
	// reasoning. An unauthenticated request
	// here still gets RequireSession's existing JSON 401, not a redirect to
	// /login - deliberately unchanged by the login page's own addition,
	// since an htmx partial (HX-Request) fetch following a redirect would
	// swap login.html's full standalone document into #main-content rather
	// than showing it as a real page. Wiring a real unauthenticated ->
	// /login redirect (full-page navigations only, not htmx partials) is
	// left to a later phase - see PLANNING.md Known Issues.
	r.Get("/", handleIndex)
	r.With(a.RequireSession).Get("/dashboard", a.handleDashboard)
	r.With(a.RequireSession).Get("/nodes", a.handleNodes)
	r.With(a.RequireSession).Get("/profiles", a.handleModelProfiles)
	r.With(a.RequireSession).Get("/transfers", a.handleTransfers)
	r.With(a.RequireSession).Get("/metrics", a.handleMetrics)
	// /audit-log's floor is Admin, not Read-only - RequireSession only
	// confirms a session exists; the actual tier check happens inside
	// handleAuditLog via audit.Recorder.List (see its own doc comment for
	// why the RBAC check lives there and not in a second middleware).
	r.With(a.RequireSession).Get("/audit-log", a.handleAuditLog)
	// /users' floor is also Admin, same reasoning as /audit-log - the tier
	// check happens inside handleUsers via rbac.Service.ListUsers.
	r.With(a.RequireSession).Get("/users", a.handleUsers)
	// /settings' floor is also Admin, same reasoning as /audit-log and
	// /users - the tier check happens inside handleSettings via
	// settings.Service.Get.
	r.With(a.RequireSession).Get("/settings", a.handleSettings)

	// Static assets (CSS, vendored htmx) - public, no session required,
	// same reasoning a login page's own assets would need if one existed.
	r.Method(http.MethodGet, "/static/*", a.static)

	return r
}

// handleIndex redirects the site root to the Dashboard - there is nothing
// else to show at "/".
func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard", http.StatusFound)
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
// invalid. Used by the Dashboard UI's read-only pages; future RBAC-gated
// write actions will register through it too.
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
