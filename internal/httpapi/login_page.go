// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/session"
)

// loginPageData is the login page's view model - see
// loadPageTemplates' doc comment on why login.html is parsed standalone.
// Method is which form is currently shown ("ad" or "local");
// ShowMethodPicker is true only when both are available - a lone available
// method renders directly with no picker at all, the common no-LDAP case.
type loginPageData struct {
	Error            string
	CSRFToken        string
	Method           string
	ShowMethodPicker bool
	ADAvailable      bool
}

// defaultLoginMethod picks which form to show when the request carries no
// explicit ?method= - "ad" when LDAP is configured (today's unchanged
// default), "local" otherwise, since that's the only method available at
// all in that case.
func (a *API) defaultLoginMethod() string {
	if a.loginService != nil {
		return "ad"
	}
	return "local"
}

// resolveLoginMethod reads the ?method= query param, falling back to
// defaultLoginMethod for an empty or unrecognized value, then pinning it to
// "local" if the requested method isn't actually available (LDAP
// unconfigured but ?method=ad was requested).
func (a *API) resolveLoginMethod(r *http.Request) string {
	method := r.URL.Query().Get("method")
	if method != "ad" && method != "local" {
		method = a.defaultLoginMethod()
	}
	if method == "ad" && a.loginService == nil {
		method = "local"
	}
	return method
}

// isFormRequest reports whether r is a plain HTML <form method="post">
// submission - the login page's own form - rather than this project's
// usual JSON API/htmx client. Both submit to POST /login; branching here
// keeps that one URL bookmarkable for a browser while leaving the
// existing JSON contract untouched for any other caller, same as
// content negotiation elsewhere in HTTP (not a special-cased identity
// source the way handleBreakGlassLogin's separate endpoint is).
//
// Also matches multipart/form-data (the Settings page's theme-file
// upload form, internal/httpapi/settings.go's handleUploadDefaultTheme) -
// a plain <form enctype="multipart/form-data"> is just as naively
// cross-site-submittable as a urlencoded one, so RequireCSRF (csrf.go)
// must not skip it the way it correctly skips the login JSON API branch.
func isFormRequest(r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	return strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/form-data")
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
	a.renderLoginPage(w, r, "", a.resolveLoginMethod(r))
}

// handleLocalLoginPage serves the HTML login form pinned to the local-
// account method, mounted at GET /login/local - mirrors handleLoginPage's
// own already-authenticated redirect. Unlike GET /login, this ignores
// ?method= entirely: it exists specifically so the local-account form has
// its own stable, bookmarkable URL regardless of whether AD is configured.
func (a *API) handleLocalLoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if _, err := session.Verify(a.sessionSecret, cookie.Value); err == nil {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
	}
	a.renderLoginPage(w, r, "", "local")
}

// renderLoginPage renders login.html for the given method, optionally with
// an error message - used both for the initial GET and to redisplay the
// form after a failed submission (this project has no flash-message/
// session-based mechanism to preserve an error message across a redirect,
// so a failed submission re-renders in place rather than redirecting back
// to a GET route). ShowMethodPicker is only true when AD is configured -
// the common no-LDAP case renders the local form directly with no picker.
func (a *API) renderLoginPage(w http.ResponseWriter, r *http.Request, errMsg, method string) {
	t, ok := a.templates["login"]
	if !ok {
		a.logger.Printf("httpapi: no template registered for login page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := loginPageData{
		Error:            errMsg,
		CSRFToken:        csrfTokenFromContext(r.Context()),
		Method:           method,
		ShowMethodPicker: a.loginService != nil,
		ADAvailable:      a.loginService != nil,
	}
	if err := t.ExecuteTemplate(w, "login", data); err != nil {
		a.logger.Printf("httpapi: render login page: %v", err)
	}
}

// handleLoginFormSubmit is POST /login's browser-form branch - see
// isFormRequest. On success it sets the session cookie and redirects to
// /dashboard, matching a normal HTML form's navigation expectation; the
// JSON branch (handleLogin in handlers.go) is untouched.
func (a *API) handleLoginFormSubmit(w http.ResponseWriter, r *http.Request) {
	if a.loginService == nil {
		a.renderLoginPage(w, r, "AD authentication is not configured", "local")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderLoginPage(w, r, "invalid form submission", "ad")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		a.renderLoginPage(w, r, "username and password are required", "ad")
		return
	}

	_, cookieValue, err := a.loginService.Login(r.Context(), username, password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		a.renderLoginPage(w, r, "invalid username or password", "ad")
		return
	case errors.Is(err, ErrAccessDenied):
		a.renderLoginPage(w, r, "not authorized to access Sparky", "ad")
		return
	case err != nil:
		a.logger.Printf("httpapi: login form submit: %v", err)
		a.renderLoginPage(w, r, "login failed", "ad")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// handleLocalLoginFormSubmit is POST /login/local's browser-form branch -
// same isFormRequest-driven split as handleLoginFormSubmit, the JSON branch
// (handleLocalLogin in handlers.go) is untouched.
func (a *API) handleLocalLoginFormSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderLoginPage(w, r, "invalid form submission", "local")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		a.renderLoginPage(w, r, "username and password are required", "local")
		return
	}

	_, cookieValue, err := a.localLoginService.Login(r.Context(), username, password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		a.renderLoginPage(w, r, "invalid username or password", "local")
		return
	case err != nil:
		a.logger.Printf("httpapi: local login form submit: %v", err)
		a.renderLoginPage(w, r, "login failed", "local")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}
