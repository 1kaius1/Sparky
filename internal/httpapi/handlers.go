// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/1kaius1/Sparky/internal/auth"
)

const sessionCookieName = "sparky_session"

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	DisplayName string `json:"display_name"`
	Tier        string `json:"tier"`
}

// errorResponse matches CLAUDE.md API Conventions' error response format.
type errorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:     message,
		Code:      code,
		RequestID: middleware.GetReqID(r.Context()),
	})
}

// isSecureRequest reports whether the original client request was HTTPS.
// sparky-server itself receives plain HTTP from the reverse proxy that
// terminates TLS - see ARCHITECTURE.md Request Lifecycle - so this checks
// the standard X-Forwarded-Proto header a proxy sets, not r.TLS.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if isFormRequest(r) {
		a.handleLoginFormSubmit(w, r)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "request body must be valid JSON")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "username and password are required")
		return
	}

	user, cookieValue, err := a.loginService.Login(r.Context(), req.Username, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, r, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid username or password")
		return
	case errors.Is(err, ErrAccessDenied):
		writeError(w, r, http.StatusForbidden, "ACCESS_DENIED", "not authorized to access Sparky")
		return
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "login failed")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(loginResponse{
		DisplayName: user.DisplayName,
		Tier:        string(user.Tier),
	})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	setSessionCookie(w, r, "", -1)
	// Inert for a plain API/curl caller - only htmx (the sidebar's logout
	// control) reads this header, and does a full client-side navigation
	// to /login in response. htmx's own noSwapStatusCodes default already
	// skips attempting to swap a 204's (empty) body.
	w.Header().Set("HX-Redirect", "/login")
	w.WriteHeader(http.StatusNoContent)
}

type breakGlassLoginRequest struct {
	Password string `json:"password"`
}

// handleBreakGlassLogin is a distinct endpoint from handleLogin, not a
// special case within it - see PLANNING.md Decisions Log: the SuperAdmin
// is not an AD/LDAP identity, and a separate path avoids any risk of
// colliding with a real AD username.
func (a *API) handleBreakGlassLogin(w http.ResponseWriter, r *http.Request) {
	var req breakGlassLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "request body must be valid JSON")
		return
	}
	if req.Password == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "password is required")
		return
	}

	cookieValue, err := a.breakGlassLoginService.Login(r.Context(), req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, r, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid password")
		return
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "login failed")
		return
	}

	setSessionCookie(w, r, cookieValue, int(sessionDuration.Seconds()))
	w.WriteHeader(http.StatusNoContent)
}
