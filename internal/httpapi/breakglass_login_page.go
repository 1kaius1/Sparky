// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"errors"
	"net/http"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/session"
)

// breakGlassLoginPageData is the break-glass login page's view model - an
// optional error message only, same shape as loginPageData.
type breakGlassLoginPageData struct {
	Error     string
	CSRFToken string
}

// handleBreakGlassLoginPage serves the HTML break-glass sign-in form. Both
// this route and its POST counterpart sit behind a.breakGlassIPWhitelist's
// middleware (see router.go) - reachability itself, not just the password
// check, is gated. A request that already carries a valid session is sent
// straight to the dashboard, same as handleLoginPage.
func (a *API) handleBreakGlassLoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if _, err := session.Verify(a.sessionSecret, cookie.Value); err == nil {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
	}
	a.renderBreakGlassLoginPage(w, r, "")
}

// renderBreakGlassLoginPage renders breakglass_login.html, optionally with
// an error message - same in-place-re-render convention as
// renderLoginPage (no flash-message mechanism exists in this codebase).
func (a *API) renderBreakGlassLoginPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	t, ok := a.templates["breakglass_login"]
	if !ok {
		a.logger.Printf("httpapi: no template registered for break-glass login page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "breakglass_login", breakGlassLoginPageData{Error: errMsg, CSRFToken: csrfTokenFromContext(r.Context())}); err != nil {
		a.logger.Printf("httpapi: render break-glass login page: %v", err)
	}
}

// handleBreakGlassLoginFormSubmit is POST /login/break-glass's browser-form
// branch - see isFormRequest in login_page.go, reused here rather than
// duplicated. On success it sets the session cookie and redirects to
// /dashboard; the JSON branch (handleBreakGlassLogin in handlers.go) is
// untouched.
func (a *API) handleBreakGlassLoginFormSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderBreakGlassLoginPage(w, r, "invalid form submission")
		return
	}
	password := r.PostFormValue("password")
	if password == "" {
		a.renderBreakGlassLoginPage(w, r, "password is required")
		return
	}

	cookieValue, err := a.breakGlassLoginService.Login(r.Context(), password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		a.renderBreakGlassLoginPage(w, r, "invalid password")
		return
	case err != nil:
		a.logger.Printf("httpapi: break-glass login form submit: %v", err)
		a.renderBreakGlassLoginPage(w, r, "login failed")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}
