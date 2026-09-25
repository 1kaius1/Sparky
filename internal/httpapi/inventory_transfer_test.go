// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/inventory"
	"github.com/1kaius1/Sparky/internal/modelsource"
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
		nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, transfersFake, initiator, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeLocalAccountManager{}, &fakeSelfAccountManager{}, &fakeSettingsViewer{}, &fakeThemeSettingsReader{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, &fakeInventoryLister{}, &fakeSizeEstimator{}, testLogger())
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
	if !strings.Contains(rec.Body.String(), "/inventory/transfer/new") {
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
	if strings.Contains(rec.Body.String(), "/inventory/transfer/new") {
		t.Error("response shows a New transfer link for a Developer viewer, want it hidden")
	}
}

func TestHandleInitiateTransferForm_AdminAccess(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	api := newTestModelTransfersAPI(t, nodeList, &fakeTransferLister{}, &fakeTransferInitiator{permitted: true}, viewer)

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory/transfer/new", "admin-1")
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

	req := newAuthenticatedRequest(t, http.MethodGet, "/inventory/transfer/new", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleInitiateTransferForm_Unauthenticated(t *testing.T) {
	api := newTestModelTransfersAPI(t, &fakeNodeLister{}, &fakeTransferLister{}, &fakeTransferInitiator{}, newFakeUserLister())

	req := httptest.NewRequest(http.MethodGet, "/inventory/transfer/new", nil)
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

	req := newAuthenticatedFormRequest(t, "/inventory/transfer/new", "admin-1", initiateTransferForm(nil))
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

	req := newAuthenticatedFormRequest(t, "/inventory/transfer/new", "dev-1", initiateTransferForm(nil))
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

	req := newAuthenticatedFormRequest(t, "/inventory/transfer/new", "admin-1", initiateTransferForm(nil))
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

	req := newAuthenticatedFormRequest(t, "/inventory/transfer/new", "admin-1", initiateTransferForm(nil))
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

	req := httptest.NewRequest(http.MethodPost, "/inventory/transfer/new", strings.NewReader(initiateTransferForm(nil).Encode()))
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

// transferFormFixture builds an API with every collaborator the transfer
// form's endpoints touch under the test's control.
type transferFormFixture struct {
	api       *API
	initiator *fakeTransferInitiator
	inventory *fakeInventoryLister
	registrar *fakeNodeRegistrar
	estimator *fakeSizeEstimator
}

func newTransferFormFixture(t *testing.T, tier db.Tier, permitted bool) *transferFormFixture {
	t.Helper()
	f := &transferFormFixture{
		initiator: &fakeTransferInitiator{permitted: permitted},
		inventory: &fakeInventoryLister{},
		registrar: &fakeNodeRegistrar{},
		estimator: &fakeSizeEstimator{},
	}
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "dest", Name: "dest-node"}, {ID: "src", Name: "src-node"}}}
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	localSvc := NewLocalLoginService(newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, localSvc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil,
		nodeList, f.registrar, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, f.initiator, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeLocalAccountManager{}, &fakeSelfAccountManager{}, &fakeSettingsViewer{}, &fakeThemeSettingsReader{}, &fakeMetricsLister{}, events.NewBroker(), &fakeEngineProvisioner{}, &fakeEngineTransferLister{}, &fakeEngineInventoryLister{}, f.inventory, f.estimator, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	f.api = api
	return f
}

func (f *transferFormFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, target, "u-1"))
	return rec
}

func (f *transferFormFixture) post(t *testing.T, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.api.Router().ServeHTTP(rec, newAuthenticatedFormRequest(t, target, "u-1", form))
	return rec
}

func TestTransferForm_DefaultsToInternetAndDisablesPeerFields(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	body := f.get(t, "/inventory/transfer/new").Body.String()
	if !strings.Contains(body, `<fieldset id="peer-fields" hidden disabled>`) {
		t.Errorf("the peer fieldset must start hidden and disabled (so its fields are never submitted): %s", body)
	}
	if strings.Contains(body, `<fieldset id="internet-fields" hidden`) {
		t.Error("the internet fieldset must be active by default")
	}
	if !strings.Contains(body, `hx-get="/inventory/transfer/estimate-size"`) {
		t.Error("the model field must wire the size preview")
	}
}

func TestTransferForm_ReplicateLinkPrefillsPeerMode(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	entry := encodeEntry("org/m", "Q4_K_M", db.ModelFormatGGUF)
	rec := f.get(t, "/inventory/transfer/new?"+url.Values{"source_node_id": {"src"}, "entry": {entry}}.Encode())
	body := rec.Body.String()
	if !strings.Contains(body, `<fieldset id="internet-fields" hidden disabled>`) || strings.Contains(body, `<fieldset id="peer-fields" hidden`) {
		t.Errorf("a source_node_id/entry link must open peer mode: %s", body)
	}
	if !strings.Contains(body, `<option value="src" selected>`) {
		t.Error("the source node must be preselected")
	}
	if !strings.Contains(body, `name="prefill_entry" value="`+template.HTMLEscapeString(entry)+`"`) {
		t.Error("the entry must be carried in the prefill field for the options fragment")
	}
}

func TestTransferForm_ForbiddenWithoutCapability(t *testing.T) {
	f := newTransferFormFixture(t, db.TierDeveloper, false)
	for _, target := range []string{
		"/inventory/transfer/new", "/inventory/transfer/estimate-size?model_ref=org/m",
		"/inventory/transfer/peer-options?source_node_id=src", "/inventory/transfer/check-result?id=x",
	} {
		if rec := f.get(t, target); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", target, rec.Code)
		}
	}
	if rec := f.post(t, "/inventory/transfer/check-connectivity", url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("check-connectivity = %d, want 403", rec.Code)
	}
	if len(f.estimator.calls) != 0 || len(f.initiator.checkCalls) != 0 {
		t.Error("an unpermitted viewer must never trigger an outbound estimate or a node command")
	}
}

func TestOldTransfersNewRouteIsGone(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	if rec := f.get(t, "/transfers/new"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /transfers/new = %d, want 404 (moved to /inventory/transfer/new)", rec.Code)
	}
}

func TestInitiateTransfer_PeerSuccessDecodesEntry(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	rec := f.post(t, "/inventory/transfer/new", url.Values{
		"source_type": {"peer_node"}, "dest_node_id": {"dest"}, "source_node_id": {"src"},
		"entry": {encodeEntry("org/m", "Q4_K_M", db.ModelFormatGGUF)}, "source_interface": {"eth1"},
		"model_ref": {"ignored/internet-field"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/transfers" {
		t.Fatalf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(f.initiator.calls) != 1 {
		t.Fatalf("InitiateTransfer called %d times", len(f.initiator.calls))
	}
	got := f.initiator.calls[0].params
	want := transfers.InitiateTransferParams{
		DestNodeID: "dest", ModelRef: "org/m", Quantization: "Q4_K_M", SourceType: db.TransferSourcePeerNode,
		SourceNodeID: "src", Format: db.ModelFormatGGUF, SourceInterface: "eth1",
	}
	if got != want {
		t.Errorf("params = %+v, want %+v (the internet model_ref field must be ignored in peer mode)", got, want)
	}
}

func TestInitiateTransfer_InternetIgnoresPeerFields(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	f.post(t, "/inventory/transfer/new", url.Values{
		"source_type": {"internet"}, "dest_node_id": {"dest"}, "model_ref": {"org/m"}, "quantization": {"Q8_0"},
		"source_node_id": {"src"}, "entry": {encodeEntry("x/y", "", db.ModelFormatGGUF)},
	})
	got := f.initiator.calls[0].params
	if got.SourceType != "" || got.SourceNodeID != "" || got.ModelRef != "org/m" || got.Quantization != "Q8_0" {
		t.Errorf("params = %+v, want a plain internet download", got)
	}
}

func TestInitiateTransfer_PeerErrorsRedisplayTheForm(t *testing.T) {
	for name, err := range map[string]error{
		"source offline": transfers.ErrSourceNodeOffline, "dest offline": transfers.ErrDestNodeOffline,
		"source lacks model": transfers.ErrSourceNotPresent, "not ready": fmt.Errorf("%w: no ssh identity", transfers.ErrPeerNotReady),
		"invalid": transfers.ErrInvalidTransfer,
	} {
		f := newTransferFormFixture(t, db.TierAdmin, true)
		f.initiator.err = err
		rec := f.post(t, "/inventory/transfer/new", url.Values{
			"source_type": {"peer_node"}, "dest_node_id": {"dest"}, "source_node_id": {"src"}, "entry": {encodeEntry("org/m", "", db.ModelFormatSafetensors)},
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "form-error") {
			t.Errorf("%s: status %d, want 400 with the error shown", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `<option value="src" selected>`) {
			t.Errorf("%s: the redisplayed form lost the chosen source node", name)
		}
	}
}

func TestInitiateTransfer_BadEntryAndUnknownSourceType(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	for name, form := range map[string]url.Values{
		"no entry":       {"source_type": {"peer_node"}, "dest_node_id": {"dest"}, "source_node_id": {"src"}},
		"garbage entry":  {"source_type": {"peer_node"}, "dest_node_id": {"dest"}, "source_node_id": {"src"}, "entry": {"%zz"}},
		"bad format":     {"source_type": {"peer_node"}, "dest_node_id": {"dest"}, "source_node_id": {"src"}, "entry": {"ref=a%2Fb&f=onnx"}},
		"unknown source": {"source_type": {"ftp"}, "dest_node_id": {"dest"}},
	} {
		if rec := f.post(t, "/inventory/transfer/new", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if len(f.initiator.calls) != 0 {
		t.Error("nothing may be initiated for a malformed submission")
	}
}

func TestEstimateSize(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		err     error
		target  string
		want    string
		wantRef string
	}{
		{"gb", 5 * 1024 * 1024 * 1024, nil, "/inventory/transfer/estimate-size?model_ref=org/m&quantization=Q4_K_M", "Estimated size: ~5.0 GB", "org/m|Q4_K_M"},
		{"mb", 300 * 1024 * 1024, nil, "/inventory/transfer/estimate-size?model_ref=org/m", "Estimated size: ~300.0 MB", "org/m|"},
		{"unsupported", 0, modelsource.ErrUnsupported, "/inventory/transfer/estimate-size?model_ref=https://civitai.com/x", "not available", "https://civitai.com/x|"},
		{"unknown", 0, modelsource.ErrUnknown, "/inventory/transfer/estimate-size?model_ref=org/m", "Size unknown.", "org/m|"},
		{"other error", 0, context.DeadlineExceeded, "/inventory/transfer/estimate-size?model_ref=org/m", "Size unknown.", "org/m|"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTransferFormFixture(t, db.TierAdmin, true)
			f.estimator.size, f.estimator.err = tt.size, tt.err
			rec := f.get(t, tt.target)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("status %d body %q, want it to contain %q", rec.Code, rec.Body.String(), tt.want)
			}
			if len(f.estimator.calls) != 1 || f.estimator.calls[0] != tt.wantRef {
				t.Errorf("estimator calls = %v, want %q", f.estimator.calls, tt.wantRef)
			}
			if strings.Contains(rec.Body.String(), "<html") {
				t.Error("a fragment must not include the page shell")
			}
		})
	}
	f := newTransferFormFixture(t, db.TierAdmin, true)
	if rec := f.get(t, "/inventory/transfer/estimate-size?model_ref="); rec.Body.Len() > 1 || len(f.estimator.calls) != 0 {
		t.Errorf("an empty model reference must produce nothing and make no request (%q, %v)", rec.Body.String(), f.estimator.calls)
	}
}

func peerFixtureWithInventory(t *testing.T, tier db.Tier) *transferFormFixture {
	t.Helper()
	f := newTransferFormFixture(t, tier, true)
	def := "eth1"
	speed := 10000
	f.registrar.getNode = &db.Node{ID: "src", Name: "src-node", DefaultTransferInterface: &def}
	f.registrar.interfaces = []*db.NodeNetworkInterface{
		{InterfaceName: "eth0", IPAddress: "10.0.0.5", LinkSpeedMbps: &speed},
		{InterfaceName: "eth1", IPAddress: "10.0.1.5"},
	}
	f.inventory.byNode = map[string][]*db.NodeModelInventory{"src": {
		{ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF, Status: db.InventoryStatusPresent, SizeBytes: 4 << 20},
		{ModelRef: "org/gone", Quantization: "FP16", Format: db.ModelFormatSafetensors, Status: db.InventoryStatusRemoved},
	}}
	return f
}

func TestPeerOptions_ListsPresentEntriesAndInterfaces(t *testing.T) {
	f := peerFixtureWithInventory(t, db.TierAdmin)
	body := f.get(t, "/inventory/transfer/peer-options?source_node_id=src").Body.String()
	for _, want := range []string{"org/m - Q4_K_M (gguf), 4.0 MB", "eth0 (10.0.0.5, 10000 Mbps)", "eth1 (10.0.1.5, speed unknown)", "Node default (eth1)", `hx-post="/inventory/transfer/check-connectivity"`, `hx-post="/nodes/src/rescan-interfaces"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "org/gone") {
		t.Error("removed inventory entries must not be offered as a source")
	}
}

func TestPeerOptions_RescanOnlyForNodeAdmins(t *testing.T) {
	f := peerFixtureWithInventory(t, db.TierPowerDev)
	body := f.get(t, "/inventory/transfer/peer-options?source_node_id=src").Body.String()
	if strings.Contains(body, "rescan-interfaces") {
		t.Error("a viewer who cannot manage nodes must not be offered Rescan")
	}
	if !strings.Contains(body, "check-connectivity") {
		t.Error("Check Destination is part of initiating a transfer and must stay available")
	}
}

func TestPeerOptions_KeepsUserChoiceOverPrefill(t *testing.T) {
	f := peerFixtureWithInventory(t, db.TierAdmin)
	entry := encodeEntry("org/m", "Q4_K_M", db.ModelFormatGGUF)
	body := f.get(t, "/inventory/transfer/peer-options?"+url.Values{
		"source_node_id": {"src"}, "prefill_entry": {entry}, "source_interface": {"eth0"}, "prefill_source_interface": {"eth1"},
	}.Encode()).Body.String()
	if !strings.Contains(body, `value="`+template.HTMLEscapeString(entry)+`" selected`) {
		t.Errorf("the prefilled entry was not selected: %s", body)
	}
	if !strings.Contains(body, `value="eth0" selected`) || strings.Contains(body, `value="eth1" selected`) {
		t.Errorf("an explicit interface choice must win over the prefill: %s", body)
	}
	body = f.get(t, "/inventory/transfer/peer-options?"+url.Values{"source_node_id": {"src"}, "prefill_source_interface": {"eth1"}}.Encode()).Body.String()
	if !strings.Contains(body, `value="eth1" selected`) {
		t.Error("with no explicit choice the prefill must apply")
	}
	body = f.get(t, "/inventory/transfer/peer-options?"+url.Values{"source_node_id": {"src"}, "source_interface": {""}, "prefill_source_interface": {"eth1"}}.Encode()).Body.String()
	if strings.Contains(body, `value="eth1" selected`) {
		t.Error("choosing the default/Fastest option (empty value) must not fall back to the prefill")
	}
}

func TestPeerOptions_NoSourceUnknownSourceAndEmptyInventory(t *testing.T) {
	f := peerFixtureWithInventory(t, db.TierAdmin)
	if body := f.get(t, "/inventory/transfer/peer-options").Body.String(); !strings.Contains(body, "Choose a source node") {
		t.Errorf("no source: %s", body)
	}
	f.registrar.getNode = nil
	if body := f.get(t, "/inventory/transfer/peer-options?source_node_id=nope").Body.String(); !strings.Contains(body, "Choose a source node") {
		t.Errorf("unknown source: %s", body)
	}
	g := peerFixtureWithInventory(t, db.TierAdmin)
	g.inventory.byNode = nil
	if body := g.get(t, "/inventory/transfer/peer-options?source_node_id=src").Body.String(); !strings.Contains(body, "no models in its inventory") {
		t.Errorf("empty inventory: %s", body)
	}
}

func TestPeerOptions_EscapesNodeAndRepoSuppliedText(t *testing.T) {
	f := peerFixtureWithInventory(t, db.TierAdmin)
	f.registrar.interfaces = []*db.NodeNetworkInterface{{InterfaceName: `"><script>alert(1)</script>`, IPAddress: "10.0.0.5"}}
	f.inventory.byNode["src"][0].ModelRef = `<img src=x onerror=alert(1)>`
	body := f.get(t, "/inventory/transfer/peer-options?source_node_id=src").Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
		t.Errorf("unescaped node- or repo-supplied text reached the page: %s", body)
	}
}

func TestCheckConnectivity_StartsAndPolls(t *testing.T) {
	f := newTransferFormFixture(t, db.TierAdmin, true)
	f.initiator.checkID = "abc123"
	rec := f.post(t, "/inventory/transfer/check-connectivity", url.Values{"dest_node_id": {"dest"}, "source_node_id": {"src"}, "source_interface": {"eth0"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `data-check-state="pending"`) || !strings.Contains(body, `hx-get="/inventory/transfer/check-result?id=abc123&amp;n=1"`) {
		t.Errorf("start: %d %s", rec.Code, body)
	}
	if len(f.initiator.checkCalls) != 1 || f.initiator.checkCalls[0] != "dest|src|eth0" {
		t.Errorf("checkCalls = %v", f.initiator.checkCalls)
	}
}

func TestCheckConnectivity_ExpectedFailuresAreShownNotErrors(t *testing.T) {
	for name, err := range map[string]error{
		"dest offline": transfers.ErrDestNodeOffline, "not ready": fmt.Errorf("%w: no interface", transfers.ErrPeerNotReady), "invalid": transfers.ErrInvalidTransfer,
	} {
		f := newTransferFormFixture(t, db.TierAdmin, true)
		f.initiator.checkErr = err
		rec := f.post(t, "/inventory/transfer/check-connectivity", url.Values{"dest_node_id": {"dest"}, "source_node_id": {"src"}})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-check-state="error"`) {
			t.Errorf("%s: %d %s - htmx does not swap 4xx, so the reason must come back as a 200 fragment", name, rec.Code, rec.Body.String())
		}
	}
	f := newTransferFormFixture(t, db.TierAdmin, true)
	f.initiator.checkErr = context.Canceled
	if rec := f.post(t, "/inventory/transfer/check-connectivity", url.Values{}); rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected error status = %d, want 500", rec.Code)
	}
}

func TestCheckResult_States(t *testing.T) {
	tests := []struct {
		name   string
		known  bool
		result *transfers.ConnectivityResult
		target string
		want   string
	}{
		{"passed", true, &transfers.ConnectivityResult{Reachable: true, LatencyMs: 4}, "id=x&n=1", `data-check-state="passed"`},
		{"failed with reason", true, &transfers.ConnectivityResult{Reason: "connection refused"}, "id=x&n=1", "connection refused"},
		{"pending polls again", true, nil, "id=x&n=3", `check-result?id=x&amp;n=4`},
		{"gives up", true, nil, "id=x&n=12", `data-check-state="failed"`},
		{"unknown", false, nil, "id=x&n=1", "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTransferFormFixture(t, db.TierAdmin, true)
			f.initiator.checkKnown, f.initiator.checkResult = tt.known, tt.result
			body := f.get(t, "/inventory/transfer/check-result?"+tt.target).Body.String()
			if !strings.Contains(body, tt.want) {
				t.Errorf("body %q does not contain %q", body, tt.want)
			}
			if tt.name == "failed with reason" && strings.Contains(body, `data-check-state="passed"`) {
				t.Error("a failed check must never look passed - it gates the submit button")
			}
		})
	}
}

func TestInventoryPage_DownloadAndReplicateLinksFollowCapability(t *testing.T) {
	for _, permitted := range []bool{true, false} {
		f := newTransferFormFixture(t, db.TierAdmin, permitted)
		f.inventory.groups = []inventory.Group{{
			ModelRef: "org/m", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
			Entries: []*db.NodeModelInventory{{NodeID: "src", Status: db.InventoryStatusPresent, PlacedAt: time.Now()}},
		}}
		body := f.get(t, "/inventory").Body.String()
		wantEntry := url.Values{"source_node_id": {"src"}, "entry": {encodeEntry("org/m", "Q4_K_M", db.ModelFormatGGUF)}}.Encode()
		if got := strings.Contains(body, template.HTMLEscapeString("/inventory/transfer/new?"+wantEntry)); got != permitted {
			t.Errorf("permitted=%v: Replicate link present = %v", permitted, got)
		}
		if got := strings.Contains(body, "Download or copy a model"); got != permitted {
			t.Errorf("permitted=%v: header link present = %v", permitted, got)
		}
	}
}

func TestEncodeDecodeEntryRoundTrip(t *testing.T) {
	for _, ref := range []string{"org/m", "weird name&x=1/y", "a=b"} {
		got, q, f, ok := decodeEntry(encodeEntry(ref, "Q4 K&M", db.ModelFormatGGUF))
		if !ok || got != ref || q != "Q4 K&M" || f != db.ModelFormatGGUF {
			t.Errorf("round trip of %q = %q, %q, %q, %v", ref, got, q, f, ok)
		}
	}
	for _, bad := range []string{"", "ref=&f=gguf", "ref=a&f=x", "ref=a", "%zz"} {
		if _, _, _, ok := decodeEntry(bad); ok {
			t.Errorf("decodeEntry(%q) accepted", bad)
		}
	}
}
