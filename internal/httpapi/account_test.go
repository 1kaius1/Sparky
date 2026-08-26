// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

func TestHandleAccountPage_LocalAccount(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierReadOnly}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, &fakeSelfAccountManager{})

	req := newAuthenticatedLocalRequest(t, http.MethodGet, "/account", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleUpdateDisplayName_Success(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Old Name", Tier: db.TierReadOnly}
	selfAccount := &fakeSelfAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	req := newAuthenticatedLocalFormRequest(t, "/account/display-name", "user-1", url.Values{"display_name": {"New Name"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(selfAccount.updateDisplayNameCalls) != 1 {
		t.Fatalf("UpdateDisplayName called %d times, want 1", len(selfAccount.updateDisplayNameCalls))
	}
	call := selfAccount.updateDisplayNameCalls[0]
	if call.actor.UserID != "user-1" || call.displayName != "New Name" {
		t.Errorf("UpdateDisplayName call = %+v, want actor.UserID=user-1 displayName=\"New Name\"", call)
	}
}

func TestHandleUpdateDisplayName_NotLocalSession_Forbidden(t *testing.T) {
	users := newFakeUserLister()
	selfAccount := &fakeSelfAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	// An AD-backed session (session.New, not session.NewLocal) must be
	// rejected before ever reaching selfAccount - IsLocalAccount is false.
	req := newAuthenticatedFormRequest(t, "/account/display-name", "user-1", url.Values{"display_name": {"New Name"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(selfAccount.updateDisplayNameCalls) != 0 {
		t.Error("UpdateDisplayName was called for a non-local session")
	}
}

func TestHandleUpdateDisplayName_MissingField(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Old Name", Tier: db.TierReadOnly}
	selfAccount := &fakeSelfAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	req := newAuthenticatedLocalFormRequest(t, "/account/display-name", "user-1", url.Values{"display_name": {""}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (redisplayed page, body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(selfAccount.updateDisplayNameCalls) != 0 {
		t.Error("UpdateDisplayName was called despite a missing display name")
	}
}

func TestHandleChangeOwnPassword_Success(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierReadOnly}
	selfAccount := &fakeSelfAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	form := url.Values{"current_password": {"old-pw"}, "new_password": {"new-pw"}, "confirm_password": {"new-pw"}}
	req := newAuthenticatedLocalFormRequest(t, "/account/password", "user-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(selfAccount.changePasswordCalls) != 1 {
		t.Fatalf("ChangeOwnPassword called %d times, want 1", len(selfAccount.changePasswordCalls))
	}
	call := selfAccount.changePasswordCalls[0]
	if call.currentPassword != "old-pw" || call.newPassword != "new-pw" {
		t.Errorf("ChangeOwnPassword call = %+v, want currentPassword=old-pw newPassword=new-pw", call)
	}
}

func TestHandleChangeOwnPassword_MismatchedConfirmation(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierReadOnly}
	selfAccount := &fakeSelfAccountManager{}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	form := url.Values{"current_password": {"old-pw"}, "new_password": {"new-pw"}, "confirm_password": {"different"}}
	req := newAuthenticatedLocalFormRequest(t, "/account/password", "user-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (redisplayed page, body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(selfAccount.changePasswordCalls) != 0 {
		t.Error("ChangeOwnPassword was called despite a mismatched confirmation")
	}
}

func TestHandleChangeOwnPassword_WrongCurrentPassword(t *testing.T) {
	users := newFakeUserLister()
	username := "jsmith"
	users.byID["user-1"] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierReadOnly}
	selfAccount := &fakeSelfAccountManager{changePasswordErr: rbac.ErrWrongPassword}
	api := newTestAPIWithLocalAccounts(t, users, &fakeLocalAccountManager{}, selfAccount)

	form := url.Values{"current_password": {"wrong"}, "new_password": {"new-pw"}, "confirm_password": {"new-pw"}}
	req := newAuthenticatedLocalFormRequest(t, "/account/password", "user-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (redisplayed page with error, body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}
