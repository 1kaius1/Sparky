// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/session"
)

// TestSidebarAdminNav_HiddenForNonAdmin covers PLANNING.md's Known Issues
// row: the Admin-floor sidebar links (Users & permissions, Audit log,
// Settings) must not appear for a viewer who cannot actually use them.
func TestSidebarAdminNav_HiddenForNonAdmin(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/users", "/audit-log", "/settings"} {
		if strings.Contains(body, `href="`+want+`"`) {
			t.Errorf("Developer-tier viewer's sidebar should not link to %s: %s", want, body)
		}
	}
}

// TestSidebarAdminNav_ShownForAdmin is the counterpart to
// TestSidebarAdminNav_HiddenForNonAdmin - an Admin-tier viewer must still
// see all three links.
func TestSidebarAdminNav_ShownForAdmin(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/users", "/audit-log", "/settings"} {
		if !strings.Contains(body, `href="`+want+`"`) {
			t.Errorf("Admin-tier viewer's sidebar should link to %s: %s", want, body)
		}
	}
}

// TestSidebarAdminNav_ShownForSuperAdmin confirms the SuperAdmin
// (break-glass) session sees the same links as an Admin, without needing a
// FindByID lookup to get there - actorFromIdentity short-circuits on
// IsSuperAdmin before ever touching the user store.
func TestSidebarAdminNav_ShownForSuperAdmin(t *testing.T) {
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{})

	cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/users", "/audit-log", "/settings"} {
		if !strings.Contains(body, `href="`+want+`"`) {
			t.Errorf("SuperAdmin session's sidebar should link to %s: %s", want, body)
		}
	}
}

// TestSidebarAdminNav_HiddenWhenTierLookupFails is the fail-safe path: if
// the viewer's tier can't be resolved at all (e.g. a session outlived its
// own user row), the sidebar hides the Admin-floor links rather than
// erroring the whole page - this is a display nicety, not the real
// security boundary (each Admin-floor handler still enforces its own RBAC
// check regardless of what the sidebar shows).
func TestSidebarAdminNav_HiddenWhenTierLookupFails(t *testing.T) {
	users := newFakeUserLister()
	users.findErr = db.ErrUserNotFound
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "ghost-user")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d - a tier lookup failure must not break the page: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/users", "/audit-log", "/settings"} {
		if strings.Contains(body, `href="`+want+`"`) {
			t.Errorf("sidebar should hide %s when the viewer's tier can't be resolved: %s", want, body)
		}
	}
}

// TestSidebarAdminNav_HXRequestNeverIncludesSidebar confirms an htmx
// partial swap never re-renders the sidebar at all, for any tier - CLAUDE.md
// Frontend Conventions: "the sidebar... never reload[s]." This is also
// what keeps the extra tier lookup off the hot path for ordinary in-app
// navigation - see render's own doc comment.
func TestSidebarAdminNav_HXRequestNeverIncludesSidebar(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "admin-1")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); strings.Contains(body, "sidebar-nav") {
		t.Errorf("HX-Request response should not include the sidebar at all: %s", body)
	}
}
