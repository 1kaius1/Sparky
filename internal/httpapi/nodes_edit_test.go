// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/rbac"
)

func newNodeEditAPI(t *testing.T, tier db.Tier, registrar *fakeNodeRegistrar) *API {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
	return newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})
}

func TestHandleEditNodeForm_ShowsIdentityAndInterfaces(t *testing.T) {
	speed := 10000
	pub, host, def := "ssh-ed25519 AAAAclient", "ssh-ed25519 AAAAhost", "eth1"
	registrar := &fakeNodeRegistrar{
		getNode: &db.Node{ID: "node-1", Name: "spark-1", SSHPublicKey: &pub, SSHHostPublicKey: &host, DefaultTransferInterface: &def},
		interfaces: []*db.NodeNetworkInterface{
			{InterfaceName: "eth0", IPAddress: "10.0.0.5", LinkSpeedMbps: &speed},
			{InterfaceName: "eth1", IPAddress: "10.0.1.5"},
		},
	}
	api := newNodeEditAPI(t, db.TierAdmin, registrar)

	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/nodes/node-1/edit", "u-1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"spark-1", pub, host, "10.0.0.5", "10000 Mbps", "unknown", `value="eth1" selected`, `hx-post="/nodes/node-1/rescan-interfaces"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestHandleEditNodeForm_NoKeysYet(t *testing.T) {
	api := newNodeEditAPI(t, db.TierAdmin, &fakeNodeRegistrar{getNode: &db.Node{ID: "node-1", Name: "spark-1"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/nodes/node-1/edit", "u-1"))
	if !strings.Contains(rec.Body.String(), "Not reported yet") || !strings.Contains(rec.Body.String(), "No interfaces reported yet.") {
		t.Errorf("empty states missing: %s", rec.Body.String())
	}
}

func TestHandleEditNodeForm_ForbiddenAndNotFound(t *testing.T) {
	forbidden := newNodeEditAPI(t, db.TierDeveloper, &fakeNodeRegistrar{getNode: &db.Node{ID: "node-1"}})
	rec := httptest.NewRecorder()
	forbidden.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/nodes/node-1/edit", "u-1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("developer status = %d, want 403", rec.Code)
	}

	missing := newNodeEditAPI(t, db.TierAdmin, &fakeNodeRegistrar{})
	rec = httptest.NewRecorder()
	missing.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/nodes/nope/edit", "u-1"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing node status = %d, want 404", rec.Code)
	}
}

func TestHandleUpdateNode(t *testing.T) {
	tests := []struct {
		name     string
		tier     db.Tier
		err      error
		wantCode int
		wantCall bool
	}{
		{"ok", db.TierAdmin, nil, http.StatusSeeOther, true},
		{"developer forbidden before service", db.TierDeveloper, nil, http.StatusForbidden, false},
		{"unknown interface", db.TierAdmin, nodes.ErrUnknownInterface, http.StatusBadRequest, true},
		{"not found", db.TierAdmin, db.ErrNodeNotFound, http.StatusNotFound, true},
		{"not permitted from service", db.TierAdmin, rbac.ErrNotPermitted, http.StatusForbidden, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registrar := &fakeNodeRegistrar{setDefaultErr: tt.err, getNode: &db.Node{ID: "node-1", Name: "spark-1"}}
			api := newNodeEditAPI(t, tt.tier, registrar)
			req := newAuthenticatedFormRequest(t, "/nodes/node-1/edit", "u-1", url.Values{"default_transfer_interface": {"eth0"}})
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if (len(registrar.setDefaultCalls) > 0) != tt.wantCall {
				t.Errorf("calls = %v, want called=%v", registrar.setDefaultCalls, tt.wantCall)
			}
			if tt.wantCall && len(registrar.setDefaultCalls) > 0 && registrar.setDefaultCalls[0] != "node-1|eth0" {
				t.Errorf("call = %q", registrar.setDefaultCalls[0])
			}
		})
	}
}

func TestHandleRescanInterfaces(t *testing.T) {
	tests := []struct {
		name     string
		tier     db.Tier
		err      error
		wantCode int
	}{
		{"ok", db.TierAdmin, nil, http.StatusNoContent},
		{"forbidden", db.TierDeveloper, nil, http.StatusForbidden},
		{"offline", db.TierAdmin, nodes.ErrNodeNotConnected, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registrar := &fakeNodeRegistrar{rescanErr: tt.err}
			api := newNodeEditAPI(t, tt.tier, registrar)
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, newAuthenticatedFormRequest(t, "/nodes/node-1/rescan-interfaces", "u-1", url.Values{}))
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestHandleNodes_EditLinkOnlyForAdmin(t *testing.T) {
	for tier, want := range map[db.Tier]bool{db.TierAdmin: true, db.TierDeveloper: false} {
		viewer := newFakeUserLister()
		viewer.byID["u-1"] = &db.User{ID: "u-1", Tier: tier}
		nodeList := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
		api := newTestDashboardAPIWithRegistrar(t, nodeList, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, newAuthenticatedRequest(t, http.MethodGet, "/nodes", "u-1"))
		if got := strings.Contains(rec.Body.String(), `href="/nodes/node-1/edit"`); got != want {
			t.Errorf("tier %s: edit link present = %v, want %v", tier, got, want)
		}
	}
}
