// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/session"
)

// newTestAPIWithLocalAccounts constructs an *API with caller-controlled
// localAccountManager/selfAccountManager fakes - a standalone constructor
// rather than extending the newTestDashboardAPIWith* chain (dashboard_test.go),
// since that chain's innermost layer already hardcodes fresh fakes for both.
func newTestAPIWithLocalAccounts(t *testing.T, users *fakeUserLister, localAccounts *fakeLocalAccountManager, selfAccount *fakeSelfAccountManager) *API {
	t.Helper()
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	localSvc := NewLocalLoginService(newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, localSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil,
		&fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, &fakeTransferInitiator{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, localAccounts, selfAccount, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return api
}

func newAuthenticatedLocalRequest(t *testing.T, method, target, userID string) *http.Request {
	t.Helper()
	cookieValue, err := session.Sign(testSessionSecret, session.NewLocal(userID, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	return req
}

func newAuthenticatedLocalFormRequest(t *testing.T, target, userID string, form url.Values) *http.Request {
	t.Helper()
	cookieValue, err := session.Sign(testSessionSecret, session.NewLocal(userID, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	addValidCSRF(req)
	return req
}

func TestHandleNewLocalAccountForm_AdminAccess(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, &fakeSelfAccountManager{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/users/local/new", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleNewLocalAccountForm_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, &fakeSelfAccountManager{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/users/local/new", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleCreateLocalAccount_Success(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	localAccounts := &fakeLocalAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"username": {"jsmith"}, "display_name": {"Jane Smith"}, "password": {"hunter2"}, "tier": {"developer"}}
	req := newAuthenticatedFormRequest(t, "/users/local/new", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusFound, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/users" {
		t.Errorf("Location = %q, want %q", loc, "/users")
	}
	if len(localAccounts.createCalls) != 1 {
		t.Fatalf("CreateLocalAccount called %d times, want 1", len(localAccounts.createCalls))
	}
	call := localAccounts.createCalls[0]
	if call.username != "jsmith" || call.displayName != "Jane Smith" || call.password != "hunter2" || call.tier != db.TierDeveloper {
		t.Errorf("CreateLocalAccount call = %+v, want username=jsmith displayName=\"Jane Smith\" password=hunter2 tier=developer", call)
	}
}

func TestHandleCreateLocalAccount_DuplicateUsername_RedisplaysFormWithError(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	localAccounts := &fakeLocalAccountManager{createErr: db.ErrLocalUsernameTaken}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"username": {"jsmith"}, "display_name": {"Jane Smith"}, "password": {"hunter2"}, "tier": {"developer"}}
	req := newAuthenticatedFormRequest(t, "/users/local/new", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleCreateLocalAccount_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	localAccounts := &fakeLocalAccountManager{createErr: rbac.ErrNotPermitted}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"username": {"jsmith"}, "display_name": {"Jane Smith"}, "password": {"hunter2"}, "tier": {"developer"}}
	req := newAuthenticatedFormRequest(t, "/users/local/new", "dev-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleCreateLocalAccount_MissingFields(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	localAccounts := &fakeLocalAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"username": {""}, "display_name": {"Jane Smith"}, "password": {"hunter2"}, "tier": {"developer"}}
	req := newAuthenticatedFormRequest(t, "/users/local/new", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(localAccounts.createCalls) != 0 {
		t.Error("CreateLocalAccount was called despite a missing required field")
	}
}

func TestHandleResetLocalAccountPassword_Success(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	localAccounts := &fakeLocalAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"password": {"new-password"}}
	req := newAuthenticatedFormRequest(t, "/users/target-1/local/reset-password", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if len(localAccounts.resetCalls) != 1 {
		t.Fatalf("ResetLocalAccountPassword called %d times, want 1", len(localAccounts.resetCalls))
	}
	if localAccounts.resetCalls[0].targetUserID != "target-1" || localAccounts.resetCalls[0].newPassword != "new-password" {
		t.Errorf("ResetLocalAccountPassword call = %+v, want targetUserID=target-1 newPassword=new-password", localAccounts.resetCalls[0])
	}
}

func TestHandleResetLocalAccountPassword_NotLocalAccount(t *testing.T) {
	users := newFakeUserLister()
	users.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	localAccounts := &fakeLocalAccountManager{resetErr: rbac.ErrNotLocalAccount}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"password": {"new-password"}}
	req := newAuthenticatedFormRequest(t, "/users/target-1/local/reset-password", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleResetLocalAccountPassword_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	localAccounts := &fakeLocalAccountManager{resetErr: rbac.ErrNotPermitted}
	api := newTestAPIWithLocalAccounts(t, users, localAccounts, &fakeSelfAccountManager{})

	form := url.Values{"password": {"new-password"}}
	req := newAuthenticatedFormRequest(t, "/users/target-1/local/reset-password", "dev-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
