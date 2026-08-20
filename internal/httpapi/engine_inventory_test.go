// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
)

func newTestEngineInventoryAPI(t *testing.T, nodeList *fakeNodeLister, inventoryFake *fakeEngineInventoryLister) *API {
	t.Helper()
	return newTestDashboardAPIWithEngineTransfers(t, nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, inventoryFake)
}

func TestHandleEngineInventory_ListsEntries(t *testing.T) {
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	inventoryFake := &fakeEngineInventoryLister{entries: []*db.NodeEngineInventory{
		{
			NodeID: "node-1", EngineType: db.ProfileEngineLlamaCPP, Version: "b4610",
			Status: db.InventoryStatusPresent, InstallPath: "/opt/sparky/serviceloop/engines/llamacpp/b4610",
			SizeBytes: 1024 * 1024 * 50, PlacedAt: time.Now(),
		},
	}}
	api := newTestEngineInventoryAPI(t, nodeList, inventoryFake)

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"spark-1", "llamacpp", "b4610", "/opt/sparky/serviceloop/engines/llamacpp/b4610", "50.0 MB"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q: %s", want, body)
		}
	}
}

func TestHandleEngineInventory_ListError_InternalError(t *testing.T) {
	api := newTestEngineInventoryAPI(t, &fakeNodeLister{}, &fakeEngineInventoryLister{err: context.Canceled})

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleEngineInventory_NodeListError_InternalError(t *testing.T) {
	api := newTestEngineInventoryAPI(t, &fakeNodeLister{err: context.Canceled}, &fakeEngineInventoryLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleEngineInventory_Empty_ShowsEmptyState(t *testing.T) {
	api := newTestEngineInventoryAPI(t, &fakeNodeLister{}, &fakeEngineInventoryLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/engine-inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No engines installed yet.") {
		t.Errorf("response does not show the empty state: %s", rec.Body.String())
	}
}

func TestHandleEngineInventory_Unauthenticated(t *testing.T) {
	api := newTestEngineInventoryAPI(t, &fakeNodeLister{}, &fakeEngineInventoryLister{})

	req := httptest.NewRequest(http.MethodGet, "/engine-inventory", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}
