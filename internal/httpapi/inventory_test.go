// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/inventory"
	"github.com/1kaius1/Sparky/internal/rbac"
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

func advancedGroupFixture() []inventory.Group {
	return []inventory.Group{{
		ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
		Entries: []*db.NodeModelInventory{{NodeID: "node-1", Status: db.InventoryStatusPresent, PlacedAt: time.Now()}},
	}}
}

func TestHandleInventory_DeleteButtonOnlyWhenPermitted(t *testing.T) {
	for _, canDelete := range []bool{true, false} {
		fake := &fakeInventoryLister{groups: advancedGroupFixture(), canDelete: canDelete}
		users := newFakeUserLister()
		users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
		api := newTestDashboardAPIWithInventory(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
		req := newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1")
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if got := strings.Contains(rec.Body.String(), `hx-post="/inventory/delete"`); got != canDelete {
			t.Errorf("canDelete=%v: delete form present = %v", canDelete, got)
		}
	}
}

func TestHandleDeleteInventoryEntry(t *testing.T) {
	tests := []struct {
		name       string
		form       url.Values
		deleteErr  error
		wantStatus int
		wantCalled bool
	}{
		{"ok", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "quantization": {"Q4_K_M"}, "format": {"gguf"}}, nil, http.StatusNoContent, true},
		{"missing model_ref", url.Values{"node_id": {"node-1"}, "format": {"gguf"}}, nil, http.StatusBadRequest, false},
		{"bad format", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"onnx"}}, nil, http.StatusBadRequest, false},
		{"not permitted", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"gguf"}}, rbac.ErrNotPermitted, http.StatusForbidden, true},
		{"not found", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"gguf"}}, db.ErrNodeModelInventoryNotFound, http.StatusNotFound, true},
		{"in use", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"gguf"}}, inventory.ErrModelInUse, http.StatusConflict, true},
		{"node offline", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"gguf"}}, inventory.ErrNodeOffline, http.StatusConflict, true},
		{"internal", url.Values{"node_id": {"node-1"}, "model_ref": {"org/m"}, "format": {"gguf"}}, context.Canceled, http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeInventoryLister{deleteErr: tt.deleteErr}
			users := newFakeUserLister()
			users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
			api := newTestDashboardAPIWithInventory(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
			req := newAuthenticatedFormRequest(t, "/inventory/delete", "user-1", tt.form)
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if (len(fake.deleteCalled) > 0) != tt.wantCalled {
				t.Errorf("Delete called = %v, want %v", fake.deleteCalled, tt.wantCalled)
			}
		})
	}
}

func TestInventoryPage_ShowsRemovingAndFailedDeleteStates(t *testing.T) {
	group := func() []inventory.Group {
		return []inventory.Group{{
			ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
			Entries: []*db.NodeModelInventory{{NodeID: "node-1", Status: db.InventoryStatusPresent, PlacedAt: time.Now()}},
		}}
	}
	render := func(states map[string][2]string) string {
		fake := &fakeInventoryLister{groups: group(), canDelete: true, deleteStates: states}
		users := newFakeUserLister()
		users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
		api := newTestDashboardAPIWithInventory(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1"))
		return rec.Body.String()
	}

	normal := render(nil)
	if !strings.Contains(normal, `hx-post="/inventory/delete"`) || strings.Contains(normal, "removing...") {
		t.Error("an ordinary row must offer Delete and not claim to be removing")
	}

	removing := render(map[string][2]string{"node-1|org/m": {inventory.DeleteRemoving, ""}})
	if !strings.Contains(removing, "removing...") || !strings.Contains(removing, "to remove the files") {
		t.Errorf("a pending delete must be visible on the page: %s", removing)
	}
	if strings.Contains(removing, `hx-post="/inventory/delete"`) {
		t.Error("the Delete button must be replaced while a delete is in flight (no double submit)")
	}

	failed := render(map[string][2]string{"node-1|org/m": {inventory.DeleteFailed, "device busy"}})
	if !strings.Contains(failed, "Delete failed: device busy") || !strings.Contains(failed, "Retry delete") {
		t.Errorf("a failed delete must show why and offer a retry: %s", failed)
	}
}

func TestHandleDeleteInventoryEntry_InProgressIsAConflict(t *testing.T) {
	fake := &fakeInventoryLister{deleteErr: inventory.ErrDeleteInProgress}
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithInventory(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedFormRequest(t, "/inventory/delete", "user-1", url.Values{"node_id": {"n"}, "model_ref": {"org/m"}, "format": {"gguf"}}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "DELETE_IN_PROGRESS") {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestInventoryPage_DeleteFailureDetailIsOnlyForViewersWhoCanDelete(t *testing.T) {
	fake := &fakeInventoryLister{
		canDelete: false,
		groups: []inventory.Group{{
			ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
			Entries: []*db.NodeModelInventory{{NodeID: "node-1", Status: db.InventoryStatusPresent, PlacedAt: time.Now()}},
		}},
		deleteStates: map[string][2]string{"node-1|org/m": {inventory.DeleteFailed, "stat /opt/sparky/serviceloop/models/org/m: no such file"}},
	}
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithInventory(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1"))
	if strings.Contains(rec.Body.String(), "/opt/sparky") {
		t.Error("a node's filesystem path in a failure reason must not be shown to a viewer who cannot manage models")
	}
}

func TestInventoryPage_IncompleteEntriesAreLabelledAndDeletable(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	f.inventory.canDelete = true
	f.inventory.groups = []inventory.Group{{
		ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
		Entries: []*db.NodeModelInventory{{NodeID: "node-1", Status: db.InventoryStatusIncomplete, SizeBytes: 3 << 20, PlacedAt: time.Now()}},
	}}
	f.inventory.simple = []inventory.SimpleRow{{ModelRef: "org/m", IncompleteCount: 2}}

	body := f.get(t, "/inventory").Body.String()
	for _, want := range []string{`status-incomplete`, "Partial data from a cancelled or failed transfer", "Delete the incomplete download of", `hx-post="/inventory/delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Advanced view missing %q", want)
		}
	}
	if strings.Contains(body, "Replicate to...") {
		t.Error("partial data must not be offered as a copy source (the viewer can transfer, so a usable entry would show the link)")
	}
	if simple := f.get(t, "/inventory?view=simple").Body.String(); !strings.Contains(simple, "2 incomplete") {
		t.Errorf("Simple view must flag incomplete copies: %s", simple)
	}
}
