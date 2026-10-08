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
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/inventory"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/session"
)

func (f *fakeInventoryLister) CanImport(rbac.Actor) bool { return f.canImport }

func (f *fakeInventoryLister) StartScan(_ context.Context, _ rbac.Actor, nodeIDs []string) (string, error) {
	f.startedNodes = nodeIDs
	if f.startScanErr != nil {
		return "", f.startScanErr
	}
	return "scan-abc", nil
}

func (f *fakeInventoryLister) ScanResult(_ rbac.Actor, _ string) (*inventory.ScanView, error) {
	return f.scanView, f.scanViewErr
}

func (f *fakeInventoryLister) Import(_ context.Context, _ rbac.Actor, scanID string, items []inventory.ImportItem) (int, []inventory.ImportFailure, error) {
	f.importedScanIDs = append(f.importedScanIDs, scanID)
	f.importedItems = append(f.importedItems, items...)
	if f.importErr != nil {
		return 0, nil, f.importErr
	}
	return len(items) - len(f.importFailures), f.importFailures, nil
}

func newScanTestAPI(t *testing.T, fake *fakeInventoryLister) *API {
	t.Helper()
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	nodes := &fakeNodeLister{nodes: []*db.Node{
		{ID: "node-1", Name: "spark-1", AgentStatus: db.AgentStatusOnline},
		{ID: "node-2", Name: "spark-2", AgentStatus: db.AgentStatusOffline},
	}}
	return newTestDashboardAPIWithInventory(t, nodes, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, users, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, fake)
}

func doScan(api *API, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	return rec
}

func doneView() *inventory.ScanView {
	return &inventory.ScanView{
		ID: "scan-abc", Complete: true,
		Nodes: []inventory.NodeScanView{{
			NodeID: "node-1", Status: inventory.ScanDone, KnownCount: 3,
			Candidates: []inventory.ScanCandidate{
				{ModelRef: "org/st", Format: db.ModelFormatSafetensors, SizeBytes: 3 << 30, PossiblyIncomplete: true},
				{ModelRef: "org/g", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF, SizeBytes: 4096, FileName: "g.Q4_K_M.gguf", PreviouslyRemoved: true},
				{ModelRef: "org/mix", Quantization: "UNKNOWN", Format: db.ModelFormatGGUF, SizeBytes: 1, FileName: "m.gguf", BlockedReason: "shares a directory"},
			},
		}, {
			NodeID: "node-2", Status: inventory.ScanOffline, Error: "node is not connected",
		}},
	}
}

func TestInventoryPage_ScanLinkOnlyForImporters(t *testing.T) {
	for _, canImport := range []bool{true, false} {
		api := newScanTestAPI(t, &fakeInventoryLister{groups: advancedGroupFixture(), canImport: canImport})
		rec := doScan(api, newAuthenticatedRequest(t, http.MethodGet, "/inventory", "user-1"))
		if got := strings.Contains(rec.Body.String(), `href="/inventory/scan"`); got != canImport {
			t.Errorf("canImport=%v: scan link present = %v", canImport, got)
		}
	}
}

func TestScanRoutes_ForbiddenWithoutImportPermission(t *testing.T) {
	fake := &fakeInventoryLister{canImport: false, scanView: doneView()}
	api := newScanTestAPI(t, fake)
	for _, req := range []*http.Request{
		newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan", "user-1"),
		newAuthenticatedFormRequest(t, "/inventory/scan", "user-1", url.Values{"node_id": {"node-1"}}),
		newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan/scan-abc", "user-1"),
		newAuthenticatedFormRequest(t, "/inventory/import", "user-1", url.Values{"scan_id": {"scan-abc"}}),
	} {
		if rec := doScan(api, req); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", req.Method, req.URL.Path, rec.Code)
		}
	}
	if len(fake.startedNodes) != 0 || len(fake.importedItems) != 0 {
		t.Error("a forbidden request must not reach the service")
	}
}

func TestScanRoutes_RequireCSRF(t *testing.T) {
	fake := &fakeInventoryLister{canImport: true, scanView: doneView()}
	api := newScanTestAPI(t, fake)
	for _, path := range []string{"/inventory/scan", "/inventory/import"} {
		cookieValue, err := session.Sign(testSessionSecret, session.New("user-1", sessionDuration))
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"node_id": {"node-1"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
		if rec := doScan(api, req); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF_INVALID") {
			t.Errorf("POST %s without a CSRF token: status = %d body = %s, want 403 CSRF_INVALID", path, rec.Code, rec.Body.String())
		}
	}
	if len(fake.startedNodes) != 0 || len(fake.importedItems) != 0 {
		t.Error("a request without a CSRF token must not reach the service")
	}
}

func TestHandleScanModelsPage_ListsNodes(t *testing.T) {
	api := newScanTestAPI(t, &fakeInventoryLister{canImport: true})
	rec := doScan(api, newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan", "user-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`hx-post="/inventory/scan"`, `value="node-1" checked`, "spark-2", "offline", `id="scan-results"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `value="node-2" checked`) {
		t.Error("an offline node must not be preselected")
	}
}

func TestHandleStartScan_PassesNodesAndPolls(t *testing.T) {
	fake := &fakeInventoryLister{canImport: true, scanView: &inventory.ScanView{
		ID: "scan-abc", Complete: false, Nodes: []inventory.NodeScanView{{NodeID: "node-1", Status: inventory.ScanPending}},
	}}
	api := newScanTestAPI(t, fake)
	rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/scan", "user-1", url.Values{"node_id": {"node-1", "node-2"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.startedNodes) != 2 {
		t.Errorf("startedNodes = %v", fake.startedNodes)
	}
	body := rec.Body.String()
	for _, want := range []string{`hx-get="/inventory/scan/scan-abc"`, `hx-trigger="load delay:2s"`, "Waiting for nodes", "spark-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("polling partial missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Import selected") {
		t.Error("no import button while a node is still pending")
	}
}

func TestHandleStartScan_ExpectedErrorsRenderInPartial(t *testing.T) {
	for _, err := range []error{inventory.ErrNoScanNodes, inventory.ErrInvalidScanNode, inventory.ErrTooManyScans} {
		api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, startScanErr: err})
		rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/scan", "user-1", url.Values{"node_id": {"node-1"}}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), err.Error()) {
			t.Errorf("%v: status=%d body=%s; want 200 with the message (htmx does not swap 4xx)", err, rec.Code, rec.Body.String())
		}
	}
	api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, startScanErr: context.Canceled})
	if rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/scan", "user-1", url.Values{"node_id": {"node-1"}})); rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected error: status = %d, want 500", rec.Code)
	}
}

func TestHandleScanResult_RendersCandidates(t *testing.T) {
	api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, scanView: doneView()})
	rec := doScan(api, newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan/scan-abc", "user-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`hx-post="/inventory/import"`, `name="scan_id" value="scan-abc"`,
		"spark-1", "org/st", "(whole repo)", "3.0 GB", "Possibly incomplete",
		"org/g", "g.Q4_K_M.gguf", `name="aq_1"`, `placeholder="Q4_K_M"`, "importing restores it",
		"Cannot import as is: shares a directory",
		"3 models already in the inventory", "spark-2", "node is not connected", "Import selected",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("results missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "hx-trigger=\"load delay") {
		t.Error("a complete scan must stop polling")
	}
	// The blocked row cannot be ticked; the others can.
	if !strings.Contains(body, `name="sel_2"`) || !strings.Contains(body, "disabled") {
		t.Errorf("blocked row should render a disabled checkbox: %s", body)
	}
	// Safetensors is whole-repo only, so no quantization override input.
	if strings.Contains(body, `name="aq_0"`) {
		t.Error("safetensors rows must not offer a quantization override")
	}
}

func TestHandleScanResult_EscapesNodeSuppliedValues(t *testing.T) {
	view := &inventory.ScanView{ID: "s", Complete: true, Nodes: []inventory.NodeScanView{{
		NodeID: "node-1", Status: inventory.ScanDone, Error: "<script>alert(1)</script>",
		Candidates: []inventory.ScanCandidate{{ModelRef: `org/"><img src=x onerror=alert(1)>`, Format: db.ModelFormatSafetensors}},
	}}}
	api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, scanView: view})
	body := doScan(api, newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan/s", "user-1")).Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
		t.Errorf("node-supplied text was not escaped: %s", body)
	}
}

func TestHandleScanResult_ExpiredScan(t *testing.T) {
	api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, scanViewErr: inventory.ErrScanNotFound})
	rec := doScan(api, newAuthenticatedRequest(t, http.MethodGet, "/inventory/scan/gone", "user-1"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleImportModels_PassesSelectionAndRedirects(t *testing.T) {
	fake := &fakeInventoryLister{canImport: true, scanView: doneView()}
	api := newScanTestAPI(t, fake)
	form := url.Values{
		"scan_id": {"scan-abc"},
		"sel_0":   {encodeScanEntry("node-1", "org/st", "", db.ModelFormatSafetensors)},
		"sel_1":   {encodeScanEntry("node-1", "org/g", "Q4_K_M", db.ModelFormatGGUF)},
		"aq_1":    {"  q4  "},
		"aq_2":    {"ignored: row 2 not selected"},
		"sel_x":   {"junk index"},
		"sel_3":   {"not an entry"},
	}
	rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/import", "user-1", form))
	if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/inventory" {
		t.Fatalf("status = %d redirect = %q: %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body.String())
	}
	if len(fake.importedItems) != 2 || fake.importedScanIDs[0] != "scan-abc" {
		t.Fatalf("items = %+v", fake.importedItems)
	}
	if got := fake.importedItems[0]; got.NodeID != "node-1" || got.ModelRef != "org/st" || got.Format != db.ModelFormatSafetensors || got.AsQuantization != "" {
		t.Errorf("item 0 = %+v", got)
	}
	if got := fake.importedItems[1]; got.Quantization != "Q4_K_M" || got.AsQuantization != "q4" {
		t.Errorf("item 1 = %+v, want trimmed override q4 over scanned Q4_K_M", got)
	}
}

func TestHandleImportModels_NothingSelected(t *testing.T) {
	fake := &fakeInventoryLister{canImport: true, scanView: doneView()}
	api := newScanTestAPI(t, fake)
	rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/import", "user-1", url.Values{"scan_id": {"scan-abc"}}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Select at least one model") || len(fake.importedItems) != 0 {
		t.Errorf("status=%d items=%d body=%s", rec.Code, len(fake.importedItems), rec.Body.String())
	}
}

func TestHandleImportModels_PartialFailureReRenders(t *testing.T) {
	fake := &fakeInventoryLister{canImport: true, scanView: doneView(), importFailures: []inventory.ImportFailure{
		{Item: inventory.ImportItem{NodeID: "node-1", ModelRef: "org/mix"}, Err: inventory.ErrSharedDirectory},
		{Item: inventory.ImportItem{NodeID: "node-2", ModelRef: "org/x"}, Err: context.DeadlineExceeded},
	}}
	api := newScanTestAPI(t, fake)
	form := url.Values{
		"scan_id": {"scan-abc"},
		"sel_0":   {encodeScanEntry("node-1", "org/mix", "UNKNOWN", db.ModelFormatGGUF)},
		"sel_1":   {encodeScanEntry("node-2", "org/x", "", db.ModelFormatSafetensors)},
	}
	rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/import", "user-1", form))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "" {
		t.Fatalf("status = %d redirect = %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	for _, want := range []string{"Not imported", "org/mix on spark-1", "shares a directory", "org/x on spark-2", "could not be imported"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure render missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "deadline exceeded") {
		t.Error("an unexpected error's detail must stay in the log, not the page")
	}
}

func TestHandleImportModels_ServiceErrors(t *testing.T) {
	form := url.Values{"scan_id": {"scan-abc"}, "sel_0": {encodeScanEntry("node-1", "org/st", "", db.ModelFormatSafetensors)}}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{rbac.ErrNotPermitted, http.StatusForbidden},
		{inventory.ErrScanNotFound, http.StatusOK},
		{context.Canceled, http.StatusInternalServerError},
	} {
		api := newScanTestAPI(t, &fakeInventoryLister{canImport: true, scanView: doneView(), importErr: tc.err})
		if rec := doScan(api, newAuthenticatedFormRequest(t, "/inventory/import", "user-1", form)); rec.Code != tc.code {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.code)
		}
	}
}

func TestScanEntryRoundTrip(t *testing.T) {
	enc := encodeScanEntry("n&1", "org/m=odd", "Q4 K", db.ModelFormatGGUF)
	node, ref, q, f, ok := decodeScanEntry(enc)
	if !ok || node != "n&1" || ref != "org/m=odd" || q != "Q4 K" || f != db.ModelFormatGGUF {
		t.Errorf("round trip = %q %q %q %q %v", node, ref, q, f, ok)
	}
	for _, bad := range []string{"", "node=n&ref=r&f=onnx", "ref=r&f=gguf", "node=n&f=gguf"} {
		if _, _, _, _, ok := decodeScanEntry(bad); ok {
			t.Errorf("decodeScanEntry(%q) accepted", bad)
		}
	}
}
