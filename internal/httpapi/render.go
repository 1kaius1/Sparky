// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"html/template"
	"net/http"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/web"
)

// pageData wraps a page's own view model with the shell-level fields the
// base layout needs (nav highlighting, tab title, which Admin-floor nav
// links to show) - see web/templates/layouts/base.html.
type pageData struct {
	Title         string
	ActiveSection string
	CSRFToken     string
	ShowAdminNav  bool
	Data          any
}

// loadPageTemplates parses each page template together with the base
// layout into its own isolated *template.Template. Each page's .html file
// defines a block named "content" (web/templates/pages/*.html) - parsing
// every page together (e.g. one ParseGlob across all of them) would make
// every page's "content" block collide in one shared template set, since
// html/template keeps a flat, file-independent namespace, not a per-file
// one; the last one parsed would silently win for every page. Parsing
// base+one-page per entry keeps each page's "content" definition private
// to its own set.
func loadPageTemplates() (map[string]*template.Template, error) {
	pages := []string{"dashboard", "nodes", "profiles", "transfers", "engine_transfers", "metrics", "audit", "users", "settings", "register_node", "node_registered", "profile_form", "provision_engine", "forbidden"}
	result := make(map[string]*template.Template, len(pages)+1)
	for _, name := range pages {
		t, err := template.ParseFS(web.FS, "templates/layouts/base.html", "templates/pages/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", name, err)
		}
		result[name] = t
	}

	// login.html is a standalone document, not part of the authenticated
	// app shell - it defines its own <html>/<head>/<body>, so it's parsed
	// alone rather than together with base.html (there is no sidebar to
	// render before a session exists).
	loginTmpl, err := template.ParseFS(web.FS, "templates/pages/login.html")
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
	}
	result["login"] = loginTmpl

	// breakglass_login.html is also a standalone document, same reasoning
	// as login.html above - it's the SuperAdmin's own sign-in form, not
	// part of the authenticated app shell.
	breakGlassLoginTmpl, err := template.ParseFS(web.FS, "templates/pages/breakglass_login.html")
	if err != nil {
		return nil, fmt.Errorf("parse break-glass login template: %w", err)
	}
	result["breakglass_login"] = breakGlassLoginTmpl

	return result, nil
}

// render writes page's full HTML document, or - if the request came from
// htmx (the HX-Request header htmx sets on every request it makes) - just
// that page's inner content block, leaving the sidebar/shell already on
// the browser's page untouched. See CLAUDE.md Frontend Conventions:
// "clicking a section swaps only the main pane... the sidebar... never
// reload[s]."
func (a *API) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	t, ok := a.templates[page]
	if !ok {
		a.logger.Printf("httpapi: no template registered for page %q", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	name := "base"
	if r.Header.Get("HX-Request") == "true" {
		name = "content"
	}

	pd := pageData{Title: title, ActiveSection: page, CSRFToken: csrfTokenFromContext(r.Context()), Data: data}
	if name == "base" {
		// The sidebar only exists in the full page shell - an htmx partial
		// swap never re-renders it (CLAUDE.md Frontend Conventions: "the
		// sidebar... never reload[s]") - so this extra tier lookup is paid
		// once per full page load, not on every section change.
		pd.ShowAdminNav = a.canViewAdminNav(r.Context())
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, pd); err != nil {
		a.logger.Printf("httpapi: render %s: %v", page, err)
	}
}

// canViewAdminNav reports whether the current viewer should see the
// Admin-floor sidebar links (Users & permissions, Audit log, Settings) -
// see PLANNING.md's Decisions Log for why these were previously shown to
// every viewer regardless of tier. Any failure to resolve the viewer's
// tier (no identity in context, a lookup error) hides the links rather
// than failing the page render - this is a display nicety, not the actual
// security boundary, which remains each Admin-floor handler's own RBAC
// check (unchanged by this).
func (a *API) canViewAdminNav(ctx context.Context) bool {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for sidebar nav: %v", err)
		return false
	}
	return rbac.CanViewAuditLog(actor)
}

// forbiddenPageData is the "access denied" page's view model.
type forbiddenPageData struct {
	CurrentTier string
}

// renderForbidden renders a friendly "access denied" HTML page in place of
// writeError's raw JSON 403 - used by the Admin-floor pages (Audit log,
// Users & permissions, Settings) when a non-Admin viewer's own RBAC check
// refuses them. Costs no extra DB lookup of its own: each caller has
// already resolved the actor's tier for its own RBAC gate by the time this
// is called - see actorFromIdentity. render's own canViewAdminNav call
// resolves the actor a second time for the sidebar - see its doc comment.
func (a *API) renderForbidden(w http.ResponseWriter, r *http.Request, currentTier db.Tier) {
	w.WriteHeader(http.StatusForbidden)
	a.render(w, r, "forbidden", "Access denied", forbiddenPageData{CurrentTier: string(currentTier)})
}
