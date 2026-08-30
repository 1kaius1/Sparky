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
	"github.com/1kaius1/Sparky/internal/transfers"
)

// newTestModelTransfersAPI constructs an *API with a caller-controlled
// transferInitiator fake - a standalone constructor rather than extending
// the newTestDashboardAPIWith* chain (dashboard_test.go), since that
// chain's innermost layer already hardcodes a fresh fakeTransferInitiator,
// same reasoning as newTestAPIWithLocalAccounts in local_accounts_test.go.
func newTestModelTransfersAPI(t *testing.T, nodeList *fakeNodeLister, transfersFake *fakeTransferLister, initiator *fakeTransferInitiator, viewer *fakeUserLister) *API {
	t.Helper()
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	localSvc := NewLocalLoginService(newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, localSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil,
		nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, transfersFake, initiator, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeLocalAccountManager{}, &fakeSelfAccountManager{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return api
}

func TestHandleTransfers_CanInitiate_ShownForAdmin(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{permitted: true}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/transfers/new") {
		t.Errorf("response does not show a New transfer link for a permitted viewer: %s", rec.Body.String())
	}
}

func TestHandleTransfers_CanInitiate_HiddenForDeveloper(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{permitted: false}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.Contains(rec.Body.String(), "/transfers/new") {
		t.Error("response shows a New transfer link for a Developer viewer, want it hidden")
	}
}

func TestHandleInitiateTransferForm_AdminAccess(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	api := newTestModelTransfersAPI(t, nodeList, &fakeTransferLister{}, &fakeTransferInitiator{permitted: true}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers/new", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="model_ref"`) || !strings.Contains(body, "spark-1") {
		t.Errorf("response does not show the initiate-transfer form with the node option: %s", body)
	}
}

func TestHandleInitiateTransferForm_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{permitted: false}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers/new", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleInitiateTransferForm_Unauthenticated(t *testing.T) {
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{}, newFakeUserLister())

	req := httptest.NewRequest(http.MethodGet, "/transfers/new", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func initiateTransferForm(overrides url.Values) url.Values {
	form := url.Values{
		"dest_node_id": {"node-1"},
		"model_ref":    {"RedHatAI/Qwen2-0.5B-Instruct-FP8"},
	}
	for k, v := range overrides {
		form[k] = v
	}
	return form
}

func TestHandleInitiateTransfer_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	initiator := &fakeTransferInitiator{permitted: true, transfer: &db.ModelTransfer{ID: "transfer-1"}}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, initiator, viewer)

	req := newAuthenticatedFormRequest(t, "/transfers/new", "admin-1", initiateTransferForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/transfers" {
		t.Errorf("Location = %q, want %q", loc, "/transfers")
	}
	if len(initiator.calls) != 1 {
		t.Fatalf("InitiateTransfer called %d times, want 1", len(initiator.calls))
	}
	params := initiator.calls[0].params
	if params.DestNodeID != "node-1" || params.ModelRef != "RedHatAI/Qwen2-0.5B-Instruct-FP8" {
		t.Errorf("InitiateTransfer params = %+v, want the submitted form values", params)
	}
}

func TestHandleInitiateTransfer_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	initiator := &fakeTransferInitiator{err: rbac.ErrNotPermitted}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, initiator, viewer)

	req := newAuthenticatedFormRequest(t, "/transfers/new", "dev-1", initiateTransferForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleInitiateTransfer_InvalidParams_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	initiator := &fakeTransferInitiator{err: transfers.ErrInvalidTransfer}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	api := newTestModelTransfersAPI(t, nodeList, &fakeTransferLister{}, initiator, viewer)

	req := newAuthenticatedFormRequest(t, "/transfers/new", "admin-1", initiateTransferForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "form-error") {
		t.Errorf("response does not show the error message: %s", body)
	}
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not preserve the destination node dropdown options: %s", body)
	}
}

func TestHandleInitiateTransfer_DestNodeOffline_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	initiator := &fakeTransferInitiator{err: transfers.ErrDestNodeOffline}
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, initiator, viewer)

	req := newAuthenticatedFormRequest(t, "/transfers/new", "admin-1", initiateTransferForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "not connected") {
		t.Errorf("response does not show the offline error: %s", rec.Body.String())
	}
}

func TestHandleInitiateTransfer_Unauthenticated(t *testing.T) {
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{}, newFakeUserLister())

	req := httptest.NewRequest(http.MethodPost, "/transfers/new", strings.NewReader(initiateTransferForm(nil).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}
