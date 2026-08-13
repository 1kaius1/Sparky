// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/nodes"
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

// fakeNodeRegistrar implements nodeRegistrar for tests, recording every
// call.
type fakeNodeRegistrar struct {
	node        *db.Node
	bearerToken string
	err         error
	calls       []registerCall
}

type registerCall struct {
	actor  rbac.Actor
	params nodes.RegisterNodeParams
}

func (f *fakeNodeRegistrar) RegisterNode(_ context.Context, actor rbac.Actor, params nodes.RegisterNodeParams) (*db.Node, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	f.calls = append(f.calls, registerCall{actor, params})
	node := f.node
	if node == nil {
		node = &db.Node{Name: params.Name}
	}
	return node, f.bearerToken, nil
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

// fakeUserRoster implements userRoster for tests.
type fakeUserRoster struct {
	users []*db.User
	err   error
}

func (f *fakeUserRoster) ListUsers(context.Context, rbac.Actor) ([]*db.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.users, nil
}

// fakeUserElevator implements userElevator for tests, recording every
// call.
type fakeUserElevator struct {
	err   error
	calls []elevateCall
}

type elevateCall struct {
	actor        rbac.Actor
	targetUserID string
	toTier       db.Tier
}

func (f *fakeUserElevator) ElevateTier(_ context.Context, actor rbac.Actor, targetUserID string, toTier db.Tier) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, elevateCall{actor, targetUserID, toTier})
	return nil
}

// fakeSettingsViewer implements settingsViewer for tests.
type fakeSettingsViewer struct {
	metricsExport *db.MetricsExportConfig
	auditSettings *db.AuditSettings
	err           error
}

func (f *fakeSettingsViewer) Get(context.Context, rbac.Actor) (*db.MetricsExportConfig, *db.AuditSettings, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.metricsExport, f.auditSettings, nil
}

// fakeMetricsLister implements metricsLister for tests.
type fakeMetricsLister struct {
	latestByNode    []*db.Metric
	latestByNodeErr error
	recent          []*db.Metric
	recentErr       error
}

func (f *fakeMetricsLister) ListLatestByNode(context.Context) ([]*db.Metric, error) {
	if f.latestByNodeErr != nil {
		return nil, f.latestByNodeErr
	}
	return f.latestByNode, nil
}

func (f *fakeMetricsLister) ListRecent(context.Context) ([]*db.Metric, error) {
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.recent, nil
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
	return newTestDashboardAPIWithRoster(t, nodes, profiles, instances, transfers, users, auditLog, &fakeUserRoster{})
}

// newTestDashboardAPIWithRoster adds control over userRoster (the Users &
// permissions page's RBAC-gated roster read) on top of
// newTestDashboardAPIWithAdmin's parameters - kept separate rather than
// widening that function's own signature, so the existing eleven
// newTestDashboardAPI/newTestDashboardAPIWithAdmin call sites (none of
// which touch the Users page) don't need updating.
func newTestDashboardAPIWithRoster(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster) *API {
	t.Helper()
	return newTestDashboardAPIWithSettings(t, nodes, profiles, instances, transfers, users, auditLog, roster, &fakeSettingsViewer{})
}

// newTestDashboardAPIWithSettings is the innermost test constructor,
// adding control over settingsViewer (the Settings page's RBAC-gated
// config read) on top of newTestDashboardAPIWithRoster's parameters -
// same reasoning as that function's own doc comment: kept separate so
// the existing Users-page test call sites don't need updating either.
// newTestDashboardAPIWithSettings adds control over settingsViewer (the
// Settings page's RBAC-gated config read) on top of
// newTestDashboardAPIWithRoster's parameters - same reasoning as that
// function's own doc comment: kept separate so existing Users-page test
// call sites don't need updating either.
func newTestDashboardAPIWithSettings(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, settingsSvc *fakeSettingsViewer) *API {
	t.Helper()
	return newTestDashboardAPIWithMetrics(t, nodes, profiles, instances, transfers, users, auditLog, roster, settingsSvc, &fakeMetricsLister{})
}

// newTestDashboardAPIWithMetrics adds control over metricsLister (the
// Metrics page's unguarded read) on top of
// newTestDashboardAPIWithSettings's parameters - same "kept separate"
// reasoning as that function's own doc comment.
func newTestDashboardAPIWithMetrics(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	return newTestDashboardAPIWithElevator(t, nodes, profiles, instances, transfers, users, auditLog, roster, &fakeUserElevator{}, settingsSvc, metricsSvc)
}

// newTestDashboardAPIWithElevator adds control over userElevator (the
// Users & permissions page's tier-change form) on top of
// newTestDashboardAPIWithMetrics's parameters - same "kept separate"
// reasoning as that function's own doc comment.
func newTestDashboardAPIWithElevator(t *testing.T, nodes *fakeNodeLister, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	return newTestDashboardAPIWithRegistrar(t, nodes, &fakeNodeRegistrar{}, profiles, instances, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc)
}

// newTestDashboardAPIWithRegistrar is the innermost test constructor,
// adding control over nodeRegistrar (the Nodes page's registration form)
// on top of newTestDashboardAPIWithElevator's parameters - same "kept
// separate" reasoning as that function's own doc comment.
func newTestDashboardAPIWithRegistrar(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profiles *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), testSessionSecret, nil, nodeList, registrar, profiles, instances, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc, testLogger())
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

func TestHandleUsers_AdminAccess_ResolvesElevatedByName(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}
	elevatedBy := "user-1"
	elevatedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	lastLogin := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	roster := &fakeUserRoster{users: []*db.User{
		{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin},
		{ID: "user-2", DisplayName: "Sam Developer", Tier: db.TierDeveloper, LastLoginAt: &lastLogin, ElevatedBy: &elevatedBy, ElevatedAt: &elevatedAt},
	}}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, roster)

	req := newAuthenticatedRequest(t, http.MethodGet, "/users", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sam Developer") {
		t.Errorf("response does not mention user Sam Developer: %s", body)
	}
	if !strings.Contains(body, "Jane Admin") {
		t.Errorf("response does not resolve elevated_by to the elevator's display name: %s", body)
	}
	if !strings.Contains(body, "developer") {
		t.Errorf("response does not show the developer tier: %s", body)
	}
}

func TestHandleUsers_SuperAdminSession_Access(t *testing.T) {
	roster := &fakeUserRoster{users: []*db.User{{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}}}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, roster)

	cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d - a SuperAdmin session must not need a FindByID tier lookup to pass", rec.Code, http.StatusOK)
	}
}

func TestHandleUsers_NonAdmin_Forbidden(t *testing.T) {
	// The real RBAC decision (which tiers may view the roster) is
	// rbac.Service.ListUsers's own responsibility, fully covered by that
	// package's tests (TestService_ListUsers_NotPermittedBelowAdmin).
	// fakeUserRoster doesn't reimplement that decision - it directly
	// returns rbac.ErrNotPermitted here to test handleUsers's own job:
	// translating that error into a 403 response.
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Regular Dev", Tier: db.TierDeveloper}
	roster := &fakeUserRoster{err: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, roster)

	req := newAuthenticatedRequest(t, http.MethodGet, "/users", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleUsers_Empty(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/users", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No users yet") {
		t.Error("empty-state message missing from response")
	}
}

func TestHandleUsers_ListFails(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	roster := &fakeUserRoster{err: errors.New("database unreachable")}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, roster)

	req := newAuthenticatedRequest(t, http.MethodGet, "/users", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleUsers_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleUsers_HXRequest_ReturnsPartialOnly(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	roster := &fakeUserRoster{users: []*db.User{{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}}}
	api := newTestDashboardAPIWithRoster(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, roster)

	req := newAuthenticatedRequest(t, http.MethodGet, "/users", "user-1")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("HX-Request response includes the full base layout, want partial content only")
	}
	if strings.Contains(body, "sidebar-nav") {
		t.Error("HX-Request response includes the sidebar, want it omitted")
	}
	if !strings.Contains(body, "Jane Admin") {
		t.Errorf("response does not mention the roster's own user: %s", body)
	}
}

func TestHandleSettings_AdminAccess_ResolvesUpdatedByName(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Jane Admin", Tier: db.TierAdmin}
	updatedByID := "user-1"
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNFS, UpdatedBy: &updatedByID},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/settings", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "nfs") {
		t.Errorf("response does not mention the nfs backend: %s", body)
	}
	if !strings.Contains(body, "Jane Admin") {
		t.Errorf("response does not resolve metrics export updated_by to the user's display name: %s", body)
	}
	if !strings.Contains(body, "12") {
		t.Errorf("response does not mention the retention_months value: %s", body)
	}
	if !strings.Contains(body, "syslog") {
		t.Errorf("response does not mention the forwarding protocol: %s", body)
	}
}

func TestHandleSettings_SuperAdminSession_Access(t *testing.T) {
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone},
		auditSettings: &db.AuditSettings{RetentionMonths: 12},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	cookieValue, err := session.Sign(testSessionSecret, session.NewSuperAdmin(sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d - a SuperAdmin session must not need a FindByID tier lookup to pass", rec.Code, http.StatusOK)
	}
}

func TestHandleSettings_NonAdmin_Forbidden(t *testing.T) {
	// The real RBAC decision (which tiers may view Settings) is
	// settings.Service.Get's own responsibility, fully covered by that
	// package's tests (TestService_Get_NotPermittedBelowAdmin).
	// fakeSettingsViewer doesn't reimplement that decision - it directly
	// returns rbac.ErrNotPermitted here to test handleSettings's own job:
	// translating that error into a 403 response.
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", DisplayName: "Regular Dev", Tier: db.TierDeveloper}
	settingsSvc := &fakeSettingsViewer{err: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/settings", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleSettings_GetFails(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	settingsSvc := &fakeSettingsViewer{err: errors.New("database unreachable")}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/settings", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleSettings_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleSettings_NoUpdatesYet_ShowsPlaceholder(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendNone},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingSyslog},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/settings", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Count(body, "<td>-</td>") < 2 {
		t.Errorf("response does not show the empty-updated-by placeholder for either config row: %s", body)
	}
}

func TestHandleSettings_HXRequest_ReturnsPartialOnly(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["user-1"] = &db.User{ID: "user-1", Tier: db.TierAdmin}
	settingsSvc := &fakeSettingsViewer{
		metricsExport: &db.MetricsExportConfig{BackendType: db.MetricsExportBackendS3},
		auditSettings: &db.AuditSettings{RetentionMonths: 12, ForwardingProtocol: db.AuditForwardingGELF},
	}
	api := newTestDashboardAPIWithSettings(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, settingsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/settings", "user-1")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("HX-Request response includes the full base layout, want partial content only")
	}
	if strings.Contains(body, "sidebar-nav") {
		t.Error("HX-Request response includes the sidebar, want it omitted")
	}
	if !strings.Contains(body, "s3") {
		t.Errorf("response does not mention the s3 backend: %s", body)
	}
}

func TestHandleMetrics_FullPage_ShowsNodeSummaryAndChartData(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	recordedAt := time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)
	metricsSvc := &fakeMetricsLister{
		latestByNode: []*db.Metric{{
			NodeID: "node-1", RecordedAt: recordedAt,
			GPUUtilizationPct: 42.5, GPUMemoryUsedMB: 8192, GPUMemoryTotalMB: 24576,
			CPUUtilizationPct: 10, SystemMemoryUsedMB: 4096, SystemMemoryTotalMB: 16384,
		}},
		recent: []*db.Metric{{NodeID: "node-1", RecordedAt: recordedAt, GPUUtilizationPct: 42.5}},
	}
	api := newTestDashboardAPIWithMetrics(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not resolve node_id to the node's name: %s", body)
	}
	if !strings.Contains(body, "42.5%") {
		t.Errorf("response does not show the GPU utilization percentage: %s", body)
	}
	if !strings.Contains(body, `"nodeName":"spark-1"`) {
		t.Errorf("response does not embed the chart series JSON with the resolved node name: %s", body)
	}
}

func TestHandleMetrics_Empty(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No telemetry reported yet") {
		t.Error("empty-state message missing from response")
	}
}

func TestHandleMetrics_ListLatestByNodeFails(t *testing.T) {
	metricsSvc := &fakeMetricsLister{latestByNodeErr: errors.New("database unreachable")}
	api := newTestDashboardAPIWithMetrics(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleMetrics_ListRecentFails(t *testing.T) {
	metricsSvc := &fakeMetricsLister{recentErr: errors.New("database unreachable")}
	api := newTestDashboardAPIWithMetrics(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleMetrics_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleMetrics_HXRequest_ReturnsPartialOnly(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	metricsSvc := &fakeMetricsLister{
		latestByNode: []*db.Metric{{NodeID: "node-1", RecordedAt: time.Now()}},
	}
	api := newTestDashboardAPIWithMetrics(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("HX-Request response includes the full base layout, want partial content only")
	}
	if strings.Contains(body, "sidebar-nav") {
		t.Error("HX-Request response includes the sidebar, want it omitted")
	}
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not mention the node's name: %s", body)
	}
}

func TestReachableTiers(t *testing.T) {
	tests := []struct {
		name  string
		actor rbac.Actor
		from  db.Tier
		want  []string
	}{
		{"admin from read_only", rbac.Actor{Tier: db.TierAdmin}, db.TierReadOnly, []string{"developer"}},
		{"admin from developer", rbac.Actor{Tier: db.TierAdmin}, db.TierDeveloper, []string{"read_only", "power_dev"}},
		{"admin from power_dev", rbac.Actor{Tier: db.TierAdmin}, db.TierPowerDev, []string{"developer"}},
		{"admin from admin - no authority over another Admin", rbac.Actor{Tier: db.TierAdmin}, db.TierAdmin, nil},
		{"developer actor has no elevation authority at all", rbac.Actor{Tier: db.TierDeveloper}, db.TierReadOnly, nil},
		{"superadmin from developer - every other tier reachable", rbac.Actor{IsSuperAdmin: true}, db.TierDeveloper, []string{"read_only", "power_dev", "admin"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reachableTiers(tt.actor, tt.from)
			if len(got) != len(tt.want) {
				t.Fatalf("reachableTiers() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("reachableTiers()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsKnownTier(t *testing.T) {
	for _, tier := range allTiers {
		if !isKnownTier(tier) {
			t.Errorf("isKnownTier(%q) = false, want true", tier)
		}
	}
	if isKnownTier(db.Tier("not_a_real_tier")) {
		t.Error("isKnownTier(\"not_a_real_tier\") = true, want false")
	}
}

// newAuthenticatedFormRequest builds a POST request with a
// form-urlencoded body and a valid session cookie for userID - the
// tier-change form's own submission shape (a real <form>, not JSON).
func newAuthenticatedFormRequest(t *testing.T, target, userID string, form url.Values) *http.Request {
	t.Helper()
	cookieValue, err := session.Sign(testSessionSecret, session.New(userID, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	return req
}

func TestHandleElevateUser_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	elevator := &fakeUserElevator{}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/target-1/tier", "admin-1", url.Values{"tier": {"developer"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/users" {
		t.Errorf("HX-Redirect header = %q, want %q", got, "/users")
	}
	if len(elevator.calls) != 1 {
		t.Fatalf("ElevateTier called %d times, want 1", len(elevator.calls))
	}
	call := elevator.calls[0]
	if call.targetUserID != "target-1" || call.toTier != db.TierDeveloper {
		t.Errorf("ElevateTier call = %+v, want targetUserID=target-1 toTier=developer", call)
	}
	if call.actor.UserID != "admin-1" || call.actor.Tier != db.TierAdmin {
		t.Errorf("ElevateTier actor = %+v, want the resolved viewer's own actor", call.actor)
	}
}

func TestHandleElevateUser_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	elevator := &fakeUserElevator{err: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/target-1/tier", "dev-1", url.Values{"tier": {"power_dev"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleElevateUser_UserNotFound(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	elevator := &fakeUserElevator{err: db.ErrUserNotFound}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/does-not-exist/tier", "admin-1", url.Values{"tier": {"developer"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleElevateUser_InvalidTier(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	elevator := &fakeUserElevator{}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/target-1/tier", "admin-1", url.Values{"tier": {"super_ultra_admin"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(elevator.calls) != 0 {
		t.Error("ElevateTier was called despite an invalid tier value - input must be validated before the Service call")
	}
}

func TestHandleElevateUser_MissingTier(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	elevator := &fakeUserElevator{}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/target-1/tier", "admin-1", url.Values{})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleElevateUser_GenericFailure(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	elevator := &fakeUserElevator{err: errors.New("database unreachable")}
	api := newTestDashboardAPIWithElevator(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, elevator, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/users/target-1/tier", "admin-1", url.Values{"tier": {"developer"}})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleElevateUser_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodPost, "/users/target-1/tier", strings.NewReader(url.Values{"tier": {"developer"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleNodes_CanRegister_ShownForAdmin(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/nodes/register") {
		t.Errorf("response does not show a Register node link for an Admin viewer: %s", rec.Body.String())
	}
}

func TestHandleNodes_CanRegister_HiddenForDeveloper(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.Contains(rec.Body.String(), "/nodes/register") {
		t.Error("response shows a Register node link for a Developer viewer, want it hidden")
	}
}

func TestHandleRegisterNodeForm_AdminAccess(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes/register", "admin-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `name="gpu_memory_gb"`) {
		t.Errorf("response does not show the registration form: %s", rec.Body.String())
	}
}

func TestHandleRegisterNodeForm_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/nodes/register", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleRegisterNodeForm_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/nodes/register", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func registerNodeForm(overrides url.Values) url.Values {
	form := url.Values{
		"name":          {"spark-1"},
		"hostname":      {"spark-1.internal"},
		"ip_address":    {"10.0.0.11"},
		"node_type":     {"spark"},
		"gpu_memory_gb": {"128"},
		"cpu_memory_gb": {"128"},
	}
	for k, v := range overrides {
		form[k] = v
	}
	return form
}

func TestHandleRegisterNode_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	registrar := &fakeNodeRegistrar{node: &db.Node{Name: "spark-1"}, bearerToken: "plaintext-token-shown-once"}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/nodes/register", "admin-1", registerNodeForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "plaintext-token-shown-once") {
		t.Errorf("response does not show the bearer token: %s", body)
	}
	if len(registrar.calls) != 1 {
		t.Fatalf("RegisterNode called %d times, want 1", len(registrar.calls))
	}
	params := registrar.calls[0].params
	if params.Name != "spark-1" || params.Hostname != "spark-1.internal" || params.NodeType != db.NodeTypeSpark {
		t.Errorf("RegisterNode params = %+v, want the submitted form values", params)
	}
	if params.GPUMemoryGB != 128 || params.CPUMemoryGB != 128 {
		t.Errorf("RegisterNode params memory = %v/%v, want 128/128", params.GPUMemoryGB, params.CPUMemoryGB)
	}
	if params.ContainerRuntime != nil {
		t.Errorf("ContainerRuntime = %v, want nil for a spark node with no container_runtime submitted", *params.ContainerRuntime)
	}
}

func TestHandleRegisterNode_DockerGPUWithContainerRuntime(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	registrar := &fakeNodeRegistrar{node: &db.Node{Name: "workstation-1"}, bearerToken: "token"}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	form := registerNodeForm(url.Values{"node_type": {"docker-gpu"}, "container_runtime": {"podman"}})
	req := newAuthenticatedFormRequest(t, "/nodes/register", "admin-1", form)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(registrar.calls) != 1 {
		t.Fatalf("RegisterNode called %d times, want 1", len(registrar.calls))
	}
	params := registrar.calls[0].params
	if params.ContainerRuntime == nil || *params.ContainerRuntime != db.ContainerRuntimePodman {
		t.Errorf("ContainerRuntime = %v, want podman", params.ContainerRuntime)
	}
}

func TestHandleRegisterNode_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	registrar := &fakeNodeRegistrar{err: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/nodes/register", "dev-1", registerNodeForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleRegisterNode_InvalidNode_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	registrar := &fakeNodeRegistrar{err: nodes.ErrInvalidNode}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/nodes/register", "admin-1", registerNodeForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spark-1") {
		t.Errorf("response does not preserve the submitted name value: %s", body)
	}
	if !strings.Contains(body, "form-error") {
		t.Errorf("response does not show the error message: %s", body)
	}
}

func TestHandleRegisterNode_NonNumericMemory_BadRequest(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	registrar := &fakeNodeRegistrar{}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/nodes/register", "admin-1", registerNodeForm(url.Values{"gpu_memory_gb": {"not-a-number"}}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(registrar.calls) != 0 {
		t.Error("RegisterNode was called despite a non-numeric gpu_memory_gb - input must be validated before the Service call")
	}
}

func TestHandleRegisterNode_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodPost, "/nodes/register", strings.NewReader(registerNodeForm(nil).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
