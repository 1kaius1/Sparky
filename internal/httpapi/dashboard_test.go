// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/session"
)

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// fakeNodeLister implements nodeLister for tests without a real
// nodes.Service/Postgres.
type fakeNodeLister struct {
	nodes []*db.Node
	err   error
}

func (f *fakeNodeLister) ListNodes(context.Context) ([]*db.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodes, nil
}

// fakeProfileLister implements profileLister for tests.
type fakeProfileLister struct {
	profiles []*db.Profile
	err      error
}

func (f *fakeProfileLister) ListProfiles(context.Context) ([]*db.Profile, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.profiles, nil
}

// fakeInstanceLister implements instanceLister for tests.
type fakeInstanceLister struct {
	instances []*db.RunningInstance
	err       error
}

func (f *fakeInstanceLister) ListInstances(context.Context) ([]*db.RunningInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.instances, nil
}

// fakeTransferLister implements transferLister for tests.
type fakeTransferLister struct {
	transfers []*db.ModelTransfer
	err       error
}

func (f *fakeTransferLister) ListTransfers(context.Context) ([]*db.ModelTransfer, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.transfers, nil
}

// fakeUserLister implements userLister for tests.
type fakeUserLister struct {
	byID    map[string]*db.User
	users   []*db.User
	findErr error
	listErr error
}

func newFakeUserLister() *fakeUserLister {
	return &fakeUserLister{byID: make(map[string]*db.User)}
}

func (f *fakeUserLister) FindByID(_ context.Context, id string) (*db.User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, db.ErrUserNotFound
}

func (f *fakeUserLister) List(context.Context) ([]*db.User, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.users, nil
}

// fakeAuditLister implements auditLister for tests.
type fakeAuditLister struct {
	records []*db.AuditRecord
	err     error
}

func (f *fakeAuditLister) List(context.Context, rbac.Actor) ([]*db.AuditRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

// newAuthenticatedRequest builds a GET request carrying a valid session
// cookie for userID, so RequireSession-gated handlers can be exercised
// directly (via httptest.NewRecorder + api.Router()) without a full
// login round trip.
func newAuthenticatedRequest(t *testing.T, method, target, userID string) *http.Request {
	t.Helper()
	cookieValue, err := session.Sign(testSessionSecret, session.New(userID, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	return req
}

func newTestDashboardAPI(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister) *API {
	t.Helper()
	return newTestDashboardAPIWithAdmin(t, nodes, profiles, instances, transfers, newFakeUserLister(), &fakeAuditLister{})
}

func newTestDashboardAPIWithAdmin(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister) *API {
	t.Helper()
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), testSessionSecret, nil, nodes, profiles, instances, transfers, users, auditLog, testLogger())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return api
}

func TestHandleDashboard_FullPage(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{
		{ID: "node-1", Name: "spark-1", AgentStatus: db.AgentStatusOnline},
		{ID: "node-2", Name: "spark-2", AgentStatus: db.AgentStatusOffline},
	}}
	instances := &fakeInstanceLister{instances: []*db.RunningInstance{
		{ID: "instance-1", PrimaryNodeID: "node-1", Status: db.RunningInstanceStatusRunning},
	}}
	api := newTestDashboardAPI(t, nodes, &fakeProfileLister{}, instances, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("response is not a full HTML document, want a full page load to include the base layout")
	}
	if !strings.Contains(body, "spark-1") {
		t.Error("response does not mention node spark-1 anywhere")
	}
	if !strings.Contains(body, ">2<") {
		t.Errorf("response does not show a node count of 2: %s", body)
	}
}

func TestHandleDashboard_HXRequest_ReturnsPartialOnly(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "user-1")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "sidebar") {
		t.Errorf("HX-Request response includes the full page shell, want just the inner content: %s", body)
	}
	if !strings.Contains(body, "Dashboard") {
		t.Error("partial response does not contain the page content")
	}
}

func TestHandleDashboard_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleDashboard_ListNodesFails(t *testing.T) {
	nodes := &fakeNodeLister{err: errors.New("database unreachable")}
	api := newTestDashboardAPI(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/dashboard", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleNodes_ListsRegisteredNodes(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{
		{ID: "node-1", Name: "spark-1", Hostname: "spark-1.local", NodeType: db.NodeTypeSpark, AgentStatus: db.AgentStatusOnline, GPUMemoryGB: 128, CPUMemoryGB: 128},
	}}
	api := newTestDashboardAPI(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spark-1.local") {
		t.Errorf("response does not mention the node's hostname: %s", body)
	}
	if !strings.Contains(body, "online") {
		t.Errorf("response does not mention the node's agent_status: %s", body)
	}
}

func TestHandleNodes_Empty(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No nodes registered yet") {
		t.Error("empty-state message missing from response")
	}
}

func TestHandleModelProfiles_ResolvesTargetNodeName(t *testing.T) {
	nodeID := "node-1"
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: nodeID, Name: "spark-1"}}}
	profiles := &fakeProfileLister{profiles: []*db.Profile{
		{ID: "profile-1", Name: "my-profile", ModelRef: "meta-llama/Llama-3-8B", EngineType: db.ProfileEngineVLLM, TargetNodeID: &nodeID, Port: 8000},
	}}
	api := newTestDashboardAPI(t, nodes, profiles, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "my-profile") || !strings.Contains(body, "meta-llama/Llama-3-8B") {
		t.Errorf("response does not mention the profile's name/model_ref: %s", body)
	}
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not resolve target_node_id to the node's name: %s", body)
	}
}

func TestHandleModelProfiles_ListProfilesFails(t *testing.T) {
	profiles := &fakeProfileLister{err: errors.New("database unreachable")}
	api := newTestDashboardAPI(t, &fakeNodeLister{}, profiles, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleIndex_RedirectsToDashboard(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want %q", got, "/dashboard")
	}
}

func TestStaticAssets_Served(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/static/css/main.css", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "sidebar") {
		t.Error("served CSS does not look like main.css")
	}
}

func TestHandleTransfers_ResolvesDestNodeName(t *testing.T) {
	nodeID := "node-1"
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: nodeID, Name: "spark-1"}}}
	transfers := &fakeTransferLister{transfers: []*db.ModelTransfer{
		{
			ID: "transfer-1", DestNodeID: nodeID, ModelRef: "meta-llama/Llama-3-8B",
			SourceType: db.TransferSourceInternet, Status: db.TransferStatusTransferring,
			BytesTransferred: 1048576, BytesTotal: 2097152,
		},
	}}
	api := newTestDashboardAPI(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, transfers)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "meta-llama/Llama-3-8B") {
		t.Errorf("response does not mention the transfer's model_ref: %s", body)
	}
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not resolve dest_node_id to the node's name: %s", body)
	}
	if !strings.Contains(body, "transferring") {
		t.Errorf("response does not mention the transfer's status: %s", body)
	}
	if !strings.Contains(body, "1.0 MB / 2.0 MB") {
		t.Errorf("response does not show formatted progress: %s", body)
	}
}

func TestHandleTransfers_Empty(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No transfers yet") {
		t.Error("empty-state message missing from response")
	}
}

func TestHandleTransfers_ListTransfersFails(t *testing.T) {
	transfers := &fakeTransferLister{err: errors.New("database unreachable")}
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, transfers)

	req := newAuthenticatedRequest(t, http.MethodGet, "/transfers", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleTransfers_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/transfers", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleAuditLog_AdminAccess_ResolvesActorName(t *testing.T) {
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}
	users.users = []*db.User{
		{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin},
		{ID: "user-2", DisplayName: "Sam Developer", Tier: db.TierDeveloper},
	}
	actorID := "user-2"
	auditLog := &fakeAuditLister{records: []*db.AuditRecord{
		{ID: "audit-1", ActorID: &actorID, Action: "elevated_user", ObjectType: "user", ObjectID: "user-3"},
	}}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, auditLog)

	req := newAuthenticatedRequest(t, http.MethodGet, "/audit-log", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sam Developer") {
		t.Errorf("response does not resolve actor_id to the user's display name: %s", body)
	}
	if !strings.Contains(body, "elevated_user") {
		t.Errorf("response does not mention the record's action: %s", body)
	}
}

func TestHandleAuditLog_SuperAdminAction_ShowsSuperAdminLabel(t *testing.T) {
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}
	auditLog := &fakeAuditLister{records: []*db.AuditRecord{
		{ID: "audit-1", IsSuperAdminAction: true, Action: "set_superadmin_password", ObjectType: "break_glass_credential", ObjectID: "n/a"},
	}}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, auditLog)

	req := newAuthenticatedRequest(t, http.MethodGet, "/audit-log", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "SuperAdmin") {
		t.Errorf("response does not show the SuperAdmin actor label for an is_superadmin_action record: %s", rec.Body.String())
	}
}

func TestHandleAuditLog_SuperAdminSession_Access(t *testing.T) {
	auditLog := &fakeAuditLister{records: []*db.AuditRecord{{ID: "audit-1", Action: "elevated_user"}}}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), auditLog)

	cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d - a SuperAdmin session must not need a FindByID tier lookup to pass", rec.Code, http.StatusOK)
	}
}

func TestHandleAuditLog_NonAdmin_Forbidden(t *testing.T) {
	// The real RBAC decision (which tiers may view the audit log) is
	// internal/audit.Recorder.List's own responsibility, fully covered by
	// that package's tests (TestRecorder_List_NotPermittedBelowAdmin).
	// fakeAuditLister doesn't reimplement that decision - it directly
	// returns rbac.ErrNotPermitted here to test handleAuditLog's own job:
	// translating that error into a 403 response.
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Regular Dev", Tier: db.TierDeveloper}
	auditLog := &fakeAuditLister{err: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, auditLog)

	req := newAuthenticatedRequest(t, http.MethodGet, "/audit-log", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleAuditLog_Empty(t *testing.T) {
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/audit-log", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No audit records yet") {
		t.Error("empty-state message missing from response")
	}
}

func TestHandleAuditLog_ListFails(t *testing.T) {
	users := newFakeUserLister()
	users.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	auditLog := &fakeAuditLister{err: errors.New("database unreachable")}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, users, auditLog)

	req := newAuthenticatedRequest(t, http.MethodGet, "/audit-log", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleAuditLog_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
