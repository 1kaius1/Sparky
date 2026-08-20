// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/1kaius1/Sparky/internal/session"
	"github.com/1kaius1/Sparky/web"
)

// API holds the dependencies HTTP handlers need.
type API struct {
	loginService           *LoginService
	breakGlassLoginService *BreakGlassLoginService
	breakGlassIPWhitelist  *breakGlassIPWhitelist
	breakGlassLoginPath    string
	loginRateLimiter       *loginRateLimiter
	breakGlassRateLimiter  *loginRateLimiter
	authRecheckInterval    time.Duration
	setupGate              *setupGate
	sessionSecret          string
	agentConn              http.Handler

	nodes             nodeLister
	registrar         nodeRegistrar
	profiles          profileLister
	profileEditor     profileEditor
	instances         instanceLister
	launcher          instanceLauncher
	transfers         transferLister
	users             userLister
	audit             auditLister
	userRoster        userRoster
	elevator          userElevator
	settings          settingsViewer
	metrics           metricsLister
	events            eventSource
	engineProvisioner engineProvisioner
	engineTransfers   engineTransferLister
	templates         map[string]*template.Template
	static            http.Handler
	logger            *log.Logger
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
// profiles, Transfers); registrar backs the Nodes page's registration
// form (PLANNING.md Dashboard UI Phase 9, the second write/action form)
// via nodes.Service.RegisterNode - a distinct interface from nodes even
// though the same *nodes.Service value satisfies both in production,
// same pattern as roster/elevator below; users/audit back the Audit log
// page, the first whose sidebar tier sits above Read-only (CLAUDE.md
// Frontend Conventions: "Audit log (Admin)") - users resolves both the
// viewer's own tier for that RBAC check and each audit record's
// actor_id to a display name (see PLANNING.md's Dashboard UI milestone
// item); roster backs the Users & permissions page (same Admin floor as
// Audit log) via rbac.Service.ListUsers - a distinct, RBAC-gated
// dependency from users, since that page exposes the full roster itself
// rather than resolving an already-permitted record's actor_id; elevator
// backs that page's tier-change form (PLANNING.md Dashboard UI Phase 8,
// the first write/action form) via rbac.Service.ElevateTier - roster and
// elevator are two narrow interfaces satisfied by the same *rbac.Service
// value in production, not two different dependencies; profileEditorSvc
// backs the Model profiles page's create/edit form (PLANNING.md
// Dashboard UI Phase 10) via profiles.Service.CreateProfile/
// UpdateProfile/GetProfile - a distinct interface from profiles, same
// pattern as registrar/nodes; settingsSvc backs the Settings page (same
// Admin floor) via internal/settings.Service.Get, covering the two
// singleton config rows neither internal/metrics nor internal/audit
// owns - see that package's doc comment; metricsSvc backs the Metrics
// page, back at the Read-only floor like nodes/profiles/instances/
// transfers - unlike audit/roster/elevator/settingsSvc, no RBAC check is
// involved; launcher backs the Model profiles page's Load/Unload controls
// (PLANNING.md Dashboard UI Phase 11, the fourth and last write/action
// form) via lifecycle.Service.LoadInstance/UnloadInstance - a distinct
// interface from instances, same "same value, multiple interfaces"
// pattern as registrar/nodes; eventsSource backs GET /events, the SSE
// endpoint (also Phase 11) via events.Broker.Subscribe - ARCHITECTURE.md's
// committed live-telemetry/transfer-progress channel; engineProvisionerSvc
// backs the Engine transfers page's provisioning form via
// engineprovision.Service.ProvisionEngine, gated by the same
// rbac.CanManageNodes rule node registration uses (Admin/SuperAdmin only, no
// PowerDev override - see SCHEMA.md Engine transfers); engineTransfersSvc
// backs that page's read-only list via
// engineprovision.Service.ListEngineTransfers, back at the Read-only floor
// like nodes/profiles/instances/transfers - a distinct interface from
// engineProvisionerSvc, same "same value, multiple interfaces" pattern as
// registrar/nodes; logger is used for
// rendering/query failures a handler can't turn into a useful HTTP
// response on its own. breakGlassAllowedIPs (BREAKGLASS_ALLOWED_IPS) is
// parsed once here into breakGlassIPWhitelist, gating both GET and POST
// breakGlassLoginPath - empty means allow from anywhere, see
// breakglass_ip_whitelist.go. breakGlassLoginPath (BREAKGLASS_LOGIN_PATH,
// default "/login/break-glass", already validated by config.Load) is where
// both verbs of the break-glass login route are mounted - configurable so an
// operator can move it off the well-known default, security-through-
// obscurity layered on top of (not instead of) breakGlassAllowedIPs and the
// rate limiting below. authRateLimitMaxAttempts/authRateLimitWindow
// (AUTH_RATE_LIMIT_MAX_ATTEMPTS/AUTH_RATE_LIMIT_WINDOW_SECONDS) build two
// independent loginRateLimiter instances, one for POST /login and one for
// POST breakGlassLoginPath, so a burst against one credential can't exhaust
// the other's budget - see rate_limit.go. authRecheckInterval
// (AUTH_RECHECK_INTERVAL_SECONDS) is how long a session's AD login-gate
// group membership is trusted before RequireSession re-verifies it against
// LDAP - see RequireSession's own doc comment and PLANNING.md's mid-session
// AD group re-validation Decisions Log entry. Returns an error if the
// embedded templates (web.FS) fail to parse - a template syntax error is a
// build-time bug, caught here rather than surfacing as a broken page on
// first request.
func New(loginService *LoginService, breakGlassLoginService *BreakGlassLoginService, breakGlassStore breakGlassStore, breakGlassAllowedIPs string, breakGlassLoginPath string, authRateLimitMaxAttempts int, authRateLimitWindow time.Duration, authRecheckInterval time.Duration, sessionSecret string, agentConn http.Handler,
	nodes nodeLister, registrar nodeRegistrar, profiles profileLister, profileEditorSvc profileEditor, instances instanceLister, launcher instanceLauncher, transfers transferLister, users userLister, auditLog auditLister, roster userRoster, elevator userElevator, settingsSvc settingsViewer, metricsSvc metricsLister, eventsSource eventSource, engineProvisionerSvc engineProvisioner, engineTransfersSvc engineTransferLister, logger *log.Logger) (*API, error) {
	templates, err := loadPageTemplates()
	if err != nil {
		return nil, fmt.Errorf("load page templates: %w", err)
	}
	ipWhitelist, err := newBreakGlassIPWhitelist(breakGlassAllowedIPs, logger)
	if err != nil {
		return nil, fmt.Errorf("parse BREAKGLASS_ALLOWED_IPS: %w", err)
	}
	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, fmt.Errorf("sub static FS: %w", err)
	}

	return &API{
		loginService:           loginService,
		breakGlassLoginService: breakGlassLoginService,
		breakGlassIPWhitelist:  ipWhitelist,
		breakGlassLoginPath:    breakGlassLoginPath,
		loginRateLimiter:       newLoginRateLimiter(authRateLimitMaxAttempts, authRateLimitWindow),
		breakGlassRateLimiter:  newLoginRateLimiter(authRateLimitMaxAttempts, authRateLimitWindow),
		authRecheckInterval:    authRecheckInterval,
		setupGate:              newSetupGate(breakGlassStore),
		sessionSecret:          sessionSecret,
		agentConn:              agentConn,
		nodes:                  nodes,
		registrar:              registrar,
		profiles:               profiles,
		profileEditor:          profileEditorSvc,
		instances:              instances,
		launcher:               launcher,
		transfers:              transfers,
		users:                  users,
		audit:                  auditLog,
		userRoster:             roster,
		elevator:               elevator,
		settings:               settingsSvc,
		metrics:                metricsSvc,
		events:                 eventsSource,
		engineProvisioner:      engineProvisionerSvc,
		engineTransfers:        engineTransfersSvc,
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
	// Global, unlike RequireCSRF below - even a plain GET that renders a
	// form needs a token already available to embed in it. See csrf.go.
	r.Use(a.ensureCSRFToken)

	// GET /login serves the HTML login form (Dashboard UI Phase 2);
	// POST /login itself now serves two callers - the JSON API/htmx
	// contract (unchanged) and this form's own browser submission - see
	// isFormRequest in login_page.go.
	r.Get("/login", a.handleLoginPage)
	// loginRateLimiter is ordered ahead of RequireCSRF - reject a flood of
	// attempts before doing any other work, same reasoning as the break-
	// glass IP whitelist's own position ahead of RequireCSRF below. Only
	// the POST (the actual credential check) is throttled; GET /login just
	// renders the form, so there's nothing worth rate-limiting there.
	r.With(a.loginRateLimiter.middleware, a.RequireCSRF).Post("/login", a.handleLogin)
	// GET serves the break-glass sign-in form; POST serves both that
	// form's own submission and the existing JSON API contract - same
	// isFormRequest branch handleLogin already established. Both verbs are
	// mounted at a.breakGlassLoginPath (BREAKGLASS_LOGIN_PATH, default
	// "/login/break-glass") rather than a literal - obscuring the route
	// itself, layered on top of the IP gate and rate limiting below, not a
	// substitute for either. Both verbs sit behind a.breakGlassIPWhitelist -
	// the credential being protected is identical either way, so a curl/API
	// caller gets the same IP gate a browser does (BREAKGLASS_ALLOWED_IPS -
	// empty means allow from anywhere, see breakglass_ip_whitelist.go and
	// PLANNING.md's Decisions Log). breakGlassRateLimiter is a separate
	// loginRateLimiter instance from the one guarding /login above, so a
	// burst against one credential never exhausts the other's budget. A
	// BREAKGLASS_LOGIN_PATH colliding with another already-registered route
	// panics here at router-build time (startup), before the server ever
	// serves traffic - already a fail-fast outcome, same as any other
	// startup-time config mistake in this codebase.
	r.With(a.breakGlassIPWhitelist.middleware).Get(a.breakGlassLoginPath, a.handleBreakGlassLoginPage)
	r.With(a.breakGlassIPWhitelist.middleware, a.breakGlassRateLimiter.middleware, a.RequireCSRF).Post(a.breakGlassLoginPath, a.handleBreakGlassLogin)
	r.With(a.RequireCSRF).Post("/logout", a.handleLogout)

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
	// The registration form's own RBAC gate (rbac.CanManageNodes) is
	// checked directly in both handlers - GET to decide whether to show
	// the form at all, POST (via nodes.Service.RegisterNode) as the real
	// enforcement boundary that never trusts what the GET rendered.
	r.With(a.RequireSession).Get("/nodes/register", a.handleRegisterNodeForm)
	r.With(a.RequireSession, a.RequireCSRF).Post("/nodes/register", a.handleRegisterNode)
	r.With(a.RequireSession).Get("/profiles", a.handleModelProfiles)
	// The create/edit form's own RBAC gate (rbac.CanManageProfiles) is
	// checked directly in each handler - GET to decide whether to show
	// the form at all, POST (via profiles.Service.CreateProfile/
	// UpdateProfile) as the real enforcement boundary that never trusts
	// what the GET rendered - same reasoning as node registration.
	r.With(a.RequireSession).Get("/profiles/new", a.handleNewProfileForm)
	r.With(a.RequireSession, a.RequireCSRF).Post("/profiles/new", a.handleCreateProfile)
	r.With(a.RequireSession).Get("/profiles/{id}/edit", a.handleEditProfileForm)
	r.With(a.RequireSession, a.RequireCSRF).Post("/profiles/{id}/edit", a.handleUpdateProfile)
	// Load/Unload (Dashboard UI Phase 11, the fourth and last write/action
	// form) - the RBAC gate (rbac.CanLaunchInstances) is checked inside
	// lifecycle.Service.LoadInstance/UnloadInstance itself, same
	// RequireSession-only-at-the-router-level reasoning as every other
	// write route above.
	r.With(a.RequireSession, a.RequireCSRF).Post("/profiles/{id}/load", a.handleLoadInstance)
	r.With(a.RequireSession, a.RequireCSRF).Post("/instances/{id}/unload", a.handleUnloadInstance)
	r.With(a.RequireSession).Get("/transfers", a.handleTransfers)
	r.With(a.RequireSession).Get("/engine-transfers", a.handleEngineTransfers)
	// The provisioning form's own RBAC gate (rbac.CanManageNodes) is
	// checked directly in both handlers - GET to decide whether to show
	// the form at all, POST (via engineprovision.Service.ProvisionEngine)
	// as the real enforcement boundary that never trusts what the GET
	// rendered - same reasoning as node registration.
	r.With(a.RequireSession).Get("/engine-transfers/new", a.handleProvisionEngineForm)
	r.With(a.RequireSession, a.RequireCSRF).Post("/engine-transfers/new", a.handleProvisionEngine)
	r.With(a.RequireSession).Get("/metrics", a.handleMetrics)
	// The Metrics page's live-update fetch target (web/static/js/metrics.js's
	// sparkyMetricsLiveUpdate) - same Read-only/no-audit posture as /metrics
	// itself, just a JSON response instead of an HTML page.
	r.With(a.RequireSession).Get("/metrics/chart-data", a.handleMetricsChartData)
	// /audit-log's floor is Admin, not Read-only - RequireSession only
	// confirms a session exists; the actual tier check happens inside
	// handleAuditLog via audit.Recorder.List (see its own doc comment for
	// why the RBAC check lives there and not in a second middleware).
	r.With(a.RequireSession).Get("/audit-log", a.handleAuditLog)
	// /users' floor is also Admin, same reasoning as /audit-log - the tier
	// check happens inside handleUsers via rbac.Service.ListUsers.
	r.With(a.RequireSession).Get("/users", a.handleUsers)
	// The tier-change form's own RBAC decision happens inside
	// handleElevateUser via rbac.Service.ElevateTier, same reasoning as
	// every other Admin-floor page above - RequireSession only confirms a
	// session exists.
	r.With(a.RequireSession, a.RequireCSRF).Post("/users/{id}/tier", a.handleElevateUser)
	// /settings' floor is also Admin, same reasoning as /audit-log and
	// /users - the tier check happens inside handleSettings via
	// settings.Service.Get.
	r.With(a.RequireSession).Get("/settings", a.handleSettings)
	// /events is the SSE endpoint (Dashboard UI Phase 11) - session-gated
	// like every Read-only-tier page above, no RBAC beyond that (see
	// handleEvents' own doc comment).
	r.With(a.RequireSession).Get("/events", a.handleEvents)

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
// Identity in the request context, redirecting to /login if it is missing
// or invalid - see respondUnauthenticated. Used by the Dashboard UI's
// read-only pages; future RBAC-gated write actions will register through
// it too.
//
// Once a session's LastVerifiedAt has gone stale past a.authRecheckInterval,
// this also re-verifies the user's AD login-gate group membership before
// letting the request through - see recheckAccessGroup and PLANNING.md's
// mid-session AD group re-validation Decisions Log entry. A SuperAdmin
// (break-glass) session is never rechecked - it isn't AD-backed at all.
func (a *API) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			a.respondUnauthenticated(w, r, "no session")
			return
		}

		sess, err := session.Verify(a.sessionSecret, cookie.Value)
		if err != nil {
			a.respondUnauthenticated(w, r, "invalid or expired session")
			return
		}

		if !sess.IsSuperAdmin && time.Since(sess.LastVerifiedAt) >= a.authRecheckInterval {
			refreshed, ok := a.recheckAccessGroup(w, r, sess)
			if !ok {
				// The response has already been written (a forced logout) -
				// nothing left to do.
				return
			}
			sess = refreshed
		}

		identity := Identity{UserID: sess.UserID, IsSuperAdmin: sess.IsSuperAdmin}
		ctx := context.WithValue(r.Context(), identityContextKey, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recheckAccessGroup re-verifies sess's AD login-gate group membership,
// called by RequireSession once LastVerifiedAt has gone stale. Returns the
// refreshed session and true if the request should proceed; a false second
// return means the response has already been written (a forced logout) and
// the caller must return immediately.
//
// Two distinct failure modes are handled differently on purpose: a
// definitive "no longer a member" answer forces a logout, but LDAP itself
// being transiently unreachable fails open - the request proceeds on the
// existing, still cryptographically valid session unchanged - confirmed
// with the user rather than assumed: an LDAP blip must not force every
// active session off a GPU node someone may be mid-task on.
func (a *API) recheckAccessGroup(w http.ResponseWriter, r *http.Request, sess *session.Session) (*session.Session, bool) {
	ctx := r.Context()

	user, err := a.users.FindByID(ctx, sess.UserID)
	if err != nil {
		a.logger.Printf("httpapi: recheck: look up user %s: %v", sess.UserID, err)
		a.forceLogout(w, r, "session invalid")
		return nil, false
	}
	if user.LDAPDN == nil {
		// No cached DN yet - this user hasn't logged in since ldap_dn
		// started being recorded, so there's nothing to re-verify against.
		// Treated the same as "no longer a member" rather than trusted
		// blindly; self-heals the next time this user does a real login.
		a.forceLogout(w, r, "session needs re-verification")
		return nil, false
	}

	stillMember, err := a.loginService.Recheck(ctx, *user.LDAPDN)
	if err != nil {
		a.logger.Printf("httpapi: recheck access group for user %s: %v", sess.UserID, err)
		return sess, true
	}
	if !stillMember {
		a.forceLogout(w, r, "no longer a member of the access group")
		return nil, false
	}

	refreshed := session.Session{
		UserID:         sess.UserID,
		IsSuperAdmin:   sess.IsSuperAdmin,
		ExpiresAt:      sess.ExpiresAt,
		LastVerifiedAt: time.Now().UTC(),
	}
	cookieValue, err := session.Sign(a.sessionSecret, refreshed)
	if err != nil {
		// A signing failure here is an internal problem (e.g. a bad
		// sessionSecret), not a "no longer a member" one - fail open
		// rather than lock the user out over it, same posture as the
		// LDAP-unreachable case above.
		a.logger.Printf("httpapi: recheck: sign refreshed session for user %s: %v", sess.UserID, err)
		return sess, true
	}
	setSessionCookie(w, r, cookieValue, int(time.Until(sess.ExpiresAt).Seconds()))
	return &refreshed, true
}

// forceLogout clears the session cookie and responds as if the request had
// never been authenticated at all - used when a mid-session recheck
// determines the existing session must not be trusted further.
func (a *API) forceLogout(w http.ResponseWriter, r *http.Request, message string) {
	setSessionCookie(w, r, "", -1)
	a.respondUnauthenticated(w, r, message)
}

// respondUnauthenticated reports a missing/invalid session. A real browser
// navigation (no HX-Request header) is redirected straight to /login - a
// raw JSON 401 there would leave the user staring at an unstyled error
// blob, most commonly triggered by an ordinary session expiring
// mid-browse. An htmx-originated request (HX-Request - same check
// render() already makes) gets an HX-Redirect response header instead:
// htmx processes that header unconditionally, regardless of status code,
// and navigates the whole page itself rather than swapping /login's full
// HTML document into whatever small target element issued the partial
// fetch (confirmed against the vendored htmx.min.js's own source).
// RequireSession gates every Dashboard UI route - the browser/htmx
// frontend is the only consumer of a session cookie (CLAUDE.md API
// Conventions) - so this one header check is correct for every route
// regardless of HTTP method: GET page loads, classic <form method="post">
// submissions, and htmx hx-post actions all arrive with the right header
// already set for how they actually got here.
func (a *API) respondUnauthenticated(w http.ResponseWriter, r *http.Request, message string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", message)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// IdentityFromContext returns the Identity stored by RequireSession, if
// any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(Identity)
	return id, ok
}
