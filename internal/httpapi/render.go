// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/1kaius1/Sparky/web"
)

// pageData wraps a page's own view model with the shell-level fields the
// base layout needs (nav highlighting, tab title) - see
// web/templates/layouts/base.html.
type pageData struct {
	Title         string
	ActiveSection string
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
	pages := []string{"dashboard", "nodes", "profiles", "transfers", "metrics", "audit", "users", "settings", "register_node", "node_registered"}
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, pageData{Title: title, ActiveSection: page, Data: data}); err != nil {
		a.logger.Printf("httpapi: render %s: %v", page, err)
	}
}
