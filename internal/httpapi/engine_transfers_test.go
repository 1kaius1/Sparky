// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engineprovision"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/rbac"
)

func newTestEngineTransfersAPI(t *testing.T, nodeList *fakeNodeLister, transfersFake *fakeEngineTransferLister, provisioner *fakeEngineProvisioner, viewer *fakeUserLister) *API {
	t.Helper()
	return newTestDashboardAPIWithEngineTransfers(t, nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), provisioner, transfersFake, &fakeEngineInventoryLister{})
}

func TestHandleEngineTransfers_ListsTransfers(t *testing.T) {
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	errMsg := "checksum mismatch"
	transfersFake := &fakeEngineTransferLister{transfers: []*db.EngineTransfer{
		{DestNodeID: "node-1", EngineType: db.ProfileEngineLlamaCPP, Version: "b4610", Status: db.EngineTransferStatusFailed, ErrorMessage: &errMsg},
	}}
	api := newTestEngineTransfersAPI(t, nodeList, transfersFake, &fakeEngineProvisioner{}, newFakeUserLister())

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spark-1") || !strings.Contains(body, "llamacpp") || !strings.Contains(body, "b4610") || !strings.Contains(body, "checksum mismatch") {
		t.Errorf("response does not show the transfer row: %s", body)
	}
}

func TestHandleEngineTransfers_ListError_InternalError(t *testing.T) {
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{err: context.Canceled}, &fakeEngineProvisioner{}, newFakeUserLister())

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleEngineTransfers_CanProvision_ShownForAdmin(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/engine-transfers/new") {
		t.Errorf("response does not show a Provision engine link for an Admin viewer: %s", rec.Body.String())
	}
}

func TestHandleEngineTransfers_CanProvision_HiddenForDeveloper(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.Contains(rec.Body.String(), "/engine-transfers/new") {
		t.Error("response shows a Provision engine link for a Developer viewer, want it hidden")
	}
}

func TestHandleProvisionEngineForm_AdminAccess(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	api := newTestEngineTransfersAPI(t, nodeList, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers/new", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="version"`) || !strings.Contains(body, "spark-1") {
		t.Errorf("response does not show the provisioning form with the node option: %s", body)
	}
}

func TestHandleProvisionEngineForm_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-transfers/new", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleProvisionEngineForm_Unauthenticated(t *testing.T) {
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, newFakeUserLister())

	req := httptest.NewRequest(http.MethodGet, "/engine-transfers/new", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func provisionEngineForm(overrides url.Values) url.Values {
	form := url.Values{
		"dest_node_id": {"node-1"},
		"version":      {"b4610"},
	}
	for k, v := range overrides {
		form[k] = v
	}
	return form
}

func TestHandleProvisionEngine_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	provisioner := &fakeEngineProvisioner{transfer: &db.EngineTransfer{ID: "transfer-1"}}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, provisioner, viewer)

	req := newAuthenticatedFormRequest(t, "/engine-transfers/new", "admin-1", provisionEngineForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/engine-transfers" {
		t.Errorf("Location = %q, want %q", loc, "/engine-transfers")
	}
	if len(provisioner.calls) != 1 {
		t.Fatalf("ProvisionEngine called %d times, want 1", len(provisioner.calls))
	}
	params := provisioner.calls[0].params
	if params.DestNodeID != "node-1" || params.Version != "b4610" {
		t.Errorf("ProvisionEngine params = %+v, want the submitted form values", params)
	}
	if params.EngineType != db.ProfileEngineLlamaCPP {
		t.Errorf("EngineType = %q, want %q - the form always fixes this", params.EngineType, db.ProfileEngineLlamaCPP)
	}
}

func TestHandleProvisionEngine_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	provisioner := &fakeEngineProvisioner{err: rbac.ErrNotPermitted}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, provisioner, viewer)

	req := newAuthenticatedFormRequest(t, "/engine-transfers/new", "dev-1", provisionEngineForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleProvisionEngine_InvalidParams_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	provisioner := &fakeEngineProvisioner{err: engineprovision.ErrInvalidProvisionRequest}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	api := newTestEngineTransfersAPI(t, nodeList, &fakeEngineTransferLister{}, provisioner, viewer)

	req := newAuthenticatedFormRequest(t, "/engine-transfers/new", "admin-1", provisionEngineForm(nil))
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

func TestHandleProvisionEngine_DestNodeOffline_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	provisioner := &fakeEngineProvisioner{err: engineprovision.ErrDestNodeOffline}
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, provisioner, viewer)

	req := newAuthenticatedFormRequest(t, "/engine-transfers/new", "admin-1", provisionEngineForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "not connected") {
		t.Errorf("response does not show the offline error: %s", rec.Body.String())
	}
}

func TestHandleProvisionEngine_Unauthenticated(t *testing.T) {
	api := newTestEngineTransfersAPI(t, &fakeNodeLister{}, &fakeEngineTransferLister{}, &fakeEngineProvisioner{}, newFakeUserLister())

	req := httptest.NewRequest(http.MethodPost, "/engine-transfers/new", strings.NewReader(provisionEngineForm(nil).Encode()))
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
