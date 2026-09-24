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
	"github.com/1kaius1/Sparky/internal/inventory"
)

func newTestInventoryAPI(t *testing.T, nodeList *fakeNodeLister, inventoryFake *fakeInventoryLister) *API {
	t.Helper()
	return newTestDashboardAPIWithInventory(t, nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, inventoryFake)
}

func TestHandleInventory_Advanced_ListsGroups(t *testing.T) {
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	inventoryFake := &fakeInventoryLister{groups: []inventory.Group{
		{
			ModelRef: "meta-llama/Llama-3-8B", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
			Entries: []*db.NodeModelInventory{
				{
					NodeID: "node-1", ModelRef: "meta-llama/Llama-3-8B", Quantization: "Q4_K_M",
					Format: db.ModelFormatGGUF, Status: db.InventoryStatusPresent,
					SizeBytes: 1024 * 1024 * 4096, PlacedAt: time.Now(),
				},
			},
		},
	}}
	api := newTestInventoryAPI(t, nodeList, inventoryFake)

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"meta-llama/Llama-3-8B", "Q4_K_M", "gguf", "spark-1", "present", "4096.0 MB"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q: %s", want, body)
		}
	}
}

func TestHandleInventory_Simple_ListsRows(t *testing.T) {
	inventoryFake := &fakeInventoryLister{simple: []inventory.SimpleRow{
		{ModelRef: "meta-llama/Llama-3-8B", Quantizations: []string{"Q4_K_M", "Q8_0"}, TotalSizeBytes: 1024 * 1024},
	}}
	api := newTestInventoryAPI(t, &fakeNodeLister{}, inventoryFake)

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory?view=simple", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"meta-llama/Llama-3-8B", "Q4_K_M, Q8_0", "1.0 MB"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q: %s", want, body)
		}
	}
}

func TestHandleInventory_UnrecognizedView_FallsBackToAdvanced(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{}, &fakeInventoryLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory?view=bogus", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No models downloaded yet.") {
		t.Errorf("response does not show the Advanced empty state: %s", rec.Body.String())
	}
}

func TestHandleInventory_ListGroupedError_InternalError(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{}, &fakeInventoryLister{groupsErr: context.Canceled})

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleInventory_ListGroupedSimpleError_InternalError(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{}, &fakeInventoryLister{simpleErr: context.Canceled})

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory?view=simple", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleInventory_NodeListError_InternalError(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{err: context.Canceled}, &fakeInventoryLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleInventory_Empty_ShowsEmptyState(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{}, &fakeInventoryLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No models downloaded yet.") {
		t.Errorf("response does not show the empty state: %s", rec.Body.String())
	}
}

func TestHandleInventory_Unauthenticated(t *testing.T) {
	api := newTestInventoryAPI(t, &fakeNodeLister{}, &fakeInventoryLister{})

	req := httptest.NewRequest(http.MethodGet, "/inventory", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}
