// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/lifecycle"
	"github.com/1kaius1/Sparky/internal/rbac"
)

func newTestLaunchAPI(t *testing.T, users *fakeUserLister, launcher *fakeInstanceLauncher) *API {
	t.Helper()
	return newTestDashboardAPIWithLauncher(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, launcher, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})
}

func TestHandleLoadInstance_Success(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/load", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/profiles" {
		t.Errorf("HX-Redirect header = %q, want %q", got, "/profiles")
	}
	if len(launcher.loaded) != 1 || launcher.loaded[0] != "profile-1" {
		t.Errorf("LoadInstance calls = %v, want [profile-1]", launcher.loaded)
	}
}

func TestHandleLoadInstance_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	users.byID["ro-1"] = &db.User{ID: "ro-1", Tier: db.TierReadOnly}
	launcher := &fakeInstanceLauncher{loadErr: rbac.ErrNotPermitted}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/load", "ro-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleLoadInstance_ProfileNotFound(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{loadErr: db.ErrProfileNotFound}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/profiles/does-not-exist/load", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleLoadInstance_AlreadyRunning(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{loadErr: lifecycle.ErrAlreadyRunning}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/load", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestHandleLoadInstance_TargetNodeOffline(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{loadErr: lifecycle.ErrTargetNodeOffline}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/load", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestHandleLoadInstance_Unauthenticated(t *testing.T) {
	api := newTestLaunchAPI(t, newFakeUserLister(), &fakeInstanceLauncher{})

	req := httptest.NewRequest(http.MethodPost, "/profiles/profile-1/load", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleUnloadInstance_Success(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/instances/instance-1/unload", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/profiles" {
		t.Errorf("HX-Redirect header = %q, want %q", got, "/profiles")
	}
	if len(launcher.unloaded) != 1 || launcher.unloaded[0] != "instance-1" {
		t.Errorf("UnloadInstance calls = %v, want [instance-1]", launcher.unloaded)
	}
}

func TestHandleUnloadInstance_NotFound(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{unloadErr: db.ErrRunningInstanceNotFound}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/instances/does-not-exist/unload", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleUnloadInstance_NotRunning(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	launcher := &fakeInstanceLauncher{unloadErr: lifecycle.ErrInstanceNotRunning}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/instances/instance-1/unload", "dev-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestHandleUnloadInstance_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	users.byID["ro-1"] = &db.User{ID: "ro-1", Tier: db.TierReadOnly}
	launcher := &fakeInstanceLauncher{unloadErr: rbac.ErrNotPermitted}
	api := newTestLaunchAPI(t, users, launcher)

	req := newAuthenticatedFormRequest(t, "/instances/instance-1/unload", "ro-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleUnloadInstance_Unauthenticated(t *testing.T) {
	api := newTestLaunchAPI(t, newFakeUserLister(), &fakeInstanceLauncher{})

	req := httptest.NewRequest(http.MethodPost, "/instances/instance-1/unload", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
