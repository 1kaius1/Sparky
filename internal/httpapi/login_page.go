// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/session"
)

// loginPageData is the login page's view model - just an optional error
// message, since the page has no shell/nav state to track (see
// loadPageTemplates' doc comment on why login.html is parsed standalone).
type loginPageData struct {
	Error string
}

// isFormRequest reports whether r is a plain HTML <form method="post">
// submission - the login page's own form - rather than this project's
// usual JSON API/htmx client. Both submit to POST /login; branching here
// keeps that one URL bookmarkable for a browser while leaving the
// existing JSON contract untouched for any other caller, same as
// content negotiation elsewhere in HTTP (not a special-cased identity
// source the way handleBreakGlassLogin's separate endpoint is).
func isFormRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
}

// handleLoginPage serves the HTML login form. A request that already
// carries a valid session is sent straight to the dashboard instead of
// being shown a login form it doesn't need.
func (a *API) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if _, err := session.Verify(a.sessionSecret, cookie.Value); err == nil {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
	}
	a.renderLoginPage(w, r, "")
}

// renderLoginPage renders login.html, optionally with an error message -
// used both for the initial GET and to redisplay the form after a failed
// submission (this project has no flash-message/session-based mechanism
// to preserve an error message across a redirect, so a failed submission
// re-renders in place rather than redirecting back to GET /login).
func (a *API) renderLoginPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	t, ok := a.templates["login"]
	if !ok {
		a.logger.Printf("httpapi: no template registered for login page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "login", loginPageData{Error: errMsg}); err != nil {
		a.logger.Printf("httpapi: render login page: %v", err)
	}
}

// handleLoginFormSubmit is POST /login's browser-form branch - see
// isFormRequest. On success it sets the session cookie and redirects to
// /dashboard, matching a normal HTML form's navigation expectation; the
// JSON branch (handleLogin in handlers.go) is untouched.
func (a *API) handleLoginFormSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderLoginPage(w, r, "invalid form submission")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		a.renderLoginPage(w, r, "username and password are required")
		return
	}

	_, cookieValue, err := a.loginService.Login(r.Context(), username, password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		a.renderLoginPage(w, r, "invalid username or password")
		return
	case errors.Is(err, ErrAccessDenied):
		a.renderLoginPage(w, r, "not authorized to access Sparky")
		return
	case err != nil:
		a.logger.Printf("httpapi: login form submit: %v", err)
		a.renderLoginPage(w, r, "login failed")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}
