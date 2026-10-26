// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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

// sidebarUserLine returns the text of the sidebar's signed-in line, or "" when
// it is absent, so the tests below can assert on it directly.
func sidebarUserLine(body string) string {
	re := regexp.MustCompile(`(?s)<div class="sidebar-user"[^>]*>.*?<span class="sidebar-user-name">(.*?)</span>`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestSidebarCurrentUser_ShowsEachViewersOwnNameAboveMyAccount is the point of
// the feature: windows signed in as different people can be told apart.
func TestSidebarCurrentUser_ShowsEachViewersOwnNameAboveMyAccount(t *testing.T) {
	users := newFakeUserLister()
	users.byID["u-dev"] = &db.User{ID: "u-dev", Tier: db.TierDeveloper, DisplayName: "Dev One"}
	users.byID["u-admin"] = &db.User{ID: "u-admin", Tier: db.TierAdmin, DisplayName: "Admin"}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	for id, want := range map[string]string{"u-dev": "Dev One", "u-admin": "Admin"} {
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/dashboard", id))
		body := rec.Body.String()
		if got := sidebarUserLine(body); got != want {
			t.Errorf("viewer %s: sidebar shows %q, want %q", id, got, want)
		}
		if !strings.Contains(body, "Signed in as") {
			t.Errorf("viewer %s: the caption is missing", id)
		}
		// Above the My account link, as asked.
		if strings.Index(body, `class="sidebar-user"`) > strings.Index(body, `href="/account"`) {
			t.Errorf("viewer %s: the name is not above the My account link", id)
		}
	}
}

func TestSidebarCurrentUser_IsEscaped(t *testing.T) {
	users := newFakeUserLister()
	users.byID["u-1"] = &db.User{ID: "u-1", Tier: db.TierReadOnly, DisplayName: `<script>alert(1)</script> & "co"`}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "u-1"))
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a display name was rendered as live markup")
	}
	if !strings.Contains(sidebarUserLine(body), "&lt;script&gt;") {
		t.Errorf("the name is not shown escaped: %q", sidebarUserLine(body))
	}
}

// The break-glass SuperAdmin is not a Users row, so it has no display name; it
// is labelled instead, and no user lookup is needed to do so.
func TestSidebarCurrentUser_SuperAdminIsLabelled(t *testing.T) {
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{})

	cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if got := sidebarUserLine(rec.Body.String()); got != "Break-glass administrator" {
		t.Errorf("sidebar shows %q for the SuperAdmin", got)
	}
}

// If the name cannot be resolved the line is hidden and the page still renders,
// like the other sidebar niceties.
func TestSidebarCurrentUser_HiddenWhenLookupFails(t *testing.T) {
	users := newFakeUserLister()
	users.findErr = db.ErrUserNotFound
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "ghost-user"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `class="sidebar-user"`) {
		t.Error("the signed-in line is shown although the user could not be resolved")
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
