// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
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
	"github.com/1kaius1/Sparky/internal/engineprovision"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/lifecycle"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/profiles"
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

// fakeProfileEditor implements profileEditor for tests, recording every
// call.
type fakeProfileEditor struct {
	getResult *db.Profile
	getErr    error

	createErr error
	updateErr error
	created   []profiles.CreateParams
	updated   []profiles.UpdateParams
}

func (f *fakeProfileEditor) CreateProfile(_ context.Context, _ rbac.Actor, params profiles.CreateParams) (*db.Profile, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, params)
	return &db.Profile{ID: "profile-1", Name: params.Name}, nil
}

func (f *fakeProfileEditor) UpdateProfile(_ context.Context, _ rbac.Actor, params profiles.UpdateParams) (*db.Profile, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.updated = append(f.updated, params)
	return &db.Profile{ID: params.ID, Name: params.Name}, nil
}

func (f *fakeProfileEditor) GetProfile(_ context.Context, _ string) (*db.Profile, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
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

// fakeInstanceLauncher implements instanceLauncher for tests, recording
// every call.
type fakeInstanceLauncher struct {
	loadErr   error
	unloadErr error
	loaded    []string // profile IDs
	unloaded  []string // instance IDs
}

func (f *fakeInstanceLauncher) LoadInstance(_ context.Context, _ rbac.Actor, params lifecycle.LoadParams) (*db.RunningInstance, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	f.loaded = append(f.loaded, params.ProfileID)
	return &db.RunningInstance{ID: "instance-1", ProfileID: params.ProfileID, Status: db.RunningInstanceStatusStarting}, nil
}

func (f *fakeInstanceLauncher) UnloadInstance(_ context.Context, _ rbac.Actor, instanceID string) (*db.RunningInstance, error) {
	if f.unloadErr != nil {
		return nil, f.unloadErr
	}
	f.unloaded = append(f.unloaded, instanceID)
	return &db.RunningInstance{ID: instanceID, Status: db.RunningInstanceStatusStopping}, nil
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

// fakeEngineTransferLister implements engineTransferLister for tests.
type fakeEngineTransferLister struct {
	transfers []*db.EngineTransfer
	err       error
}

func (f *fakeEngineTransferLister) ListEngineTransfers(context.Context) ([]*db.EngineTransfer, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.transfers, nil
}

// fakeEngineProvisioner implements engineProvisioner for tests, recording
// every call.
type fakeEngineProvisioner struct {
	transfer *db.EngineTransfer
	err      error
	calls    []engineProvisionCall
}

type engineProvisionCall struct {
	actor  rbac.Actor
	params engineprovision.ProvisionEngineParams
}

func (f *fakeEngineProvisioner) ProvisionEngine(_ context.Context, actor rbac.Actor, params engineprovision.ProvisionEngineParams) (*db.EngineTransfer, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, engineProvisionCall{actor, params})
	transfer := f.transfer
	if transfer == nil {
		transfer = &db.EngineTransfer{DestNodeID: params.DestNodeID, EngineType: params.EngineType, Version: params.Version}
	}
	return transfer, nil
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

	latestGPUByNode    []*db.GPUMetric
	latestGPUByNodeErr error
	recentGPU          []*db.GPUMetric
	recentGPUErr       error
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

func (f *fakeMetricsLister) ListLatestGPUByNode(context.Context) ([]*db.GPUMetric, error) {
	if f.latestGPUByNodeErr != nil {
		return nil, f.latestGPUByNodeErr
	}
	return f.latestGPUByNode, nil
}

func (f *fakeMetricsLister) ListRecentGPU(context.Context) ([]*db.GPUMetric, error) {
	if f.recentGPUErr != nil {
		return nil, f.recentGPUErr
	}
	return f.recentGPU, nil
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
// newTestDashboardAPIWithRegistrar adds control over nodeRegistrar (the
// Nodes page's registration form) on top of
// newTestDashboardAPIWithElevator's parameters - same "kept separate"
// reasoning as that function's own doc comment.
func newTestDashboardAPIWithRegistrar(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profileList *fakeProfileLister, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	return newTestDashboardAPIWithProfileEditor(t, nodeList, registrar, profileList, &fakeProfileEditor{}, instances, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc)
}

// newTestDashboardAPIWithProfileEditor is the innermost test
// constructor, adding control over profileEditor (the Model profiles
// page's create/edit form) on top of newTestDashboardAPIWithRegistrar's
// parameters - same "kept separate" reasoning as that function's own doc
// comment.
func newTestDashboardAPIWithProfileEditor(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profileList *fakeProfileLister, profileEditorFake *fakeProfileEditor, instances *fakeInstanceLister, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	return newTestDashboardAPIWithLauncher(t, nodeList, registrar, profileList, profileEditorFake, instances, &fakeInstanceLauncher{}, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc)
}

// newTestDashboardAPIWithLauncher adds control over instanceLauncher (the
// Model profiles page's Load/Unload controls, Dashboard UI Phase 11) on
// top of newTestDashboardAPIWithProfileEditor's parameters - same "kept
// separate" reasoning as that function's own doc comment.
func newTestDashboardAPIWithLauncher(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profileList *fakeProfileLister, profileEditorFake *fakeProfileEditor, instances *fakeInstanceLister, launcher *fakeInstanceLauncher, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister) *API {
	t.Helper()
	return newTestDashboardAPIWithEvents(t, nodeList, registrar, profileList, profileEditorFake, instances, launcher, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc, events.NewBroker())
}

// newTestDashboardAPIWithEvents is the innermost test constructor, adding
// control over eventSource (GET /events, Dashboard UI Phase 11) on top of
// newTestDashboardAPIWithLauncher's parameters - a real *events.Broker,
// not a fake, since Broker.Subscribe already structurally satisfies
// eventSource and a real broker is exactly what tests exercising
// handleEvents need to Publish against.
func newTestDashboardAPIWithEvents(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profileList *fakeProfileLister, profileEditorFake *fakeProfileEditor, instances *fakeInstanceLister, launcher *fakeInstanceLauncher, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister, eventsSrc *events.Broker) *API {
	t.Helper()
	return newTestDashboardAPIWithEngineTransfers(t, nodeList, registrar, profileList, profileEditorFake, instances, launcher, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc, eventsSrc, &fakeEngineProvisioner{}, &fakeEngineTransferLister{})
}

// newTestDashboardAPIWithEngineTransfers is the true innermost test
// constructor, adding control over engineProvisioner/engineTransferLister
// (the Engine transfers page) on top of newTestDashboardAPIWithEvents'
// parameters - kept separate rather than widening that function's own
// signature, same "existing call sites don't need updating" reasoning as
// newTestDashboardAPIWithRoster's own doc comment.
func newTestDashboardAPIWithEngineTransfers(t *testing.T, nodeList *fakeNodeLister, registrar *fakeNodeRegistrar, profileList *fakeProfileLister, profileEditorFake *fakeProfileEditor, instances *fakeInstanceLister, launcher *fakeInstanceLauncher, transfers *fakeTransferLister, users *fakeUserLister, auditLog *fakeAuditLister, roster *fakeUserRoster, elevator *fakeUserElevator, settingsSvc *fakeSettingsViewer, metricsSvc *fakeMetricsLister, eventsSrc *events.Broker, engineProvisionerFake *fakeEngineProvisioner, engineTransfersFake *fakeEngineTransferLister) *API {
	t.Helper()
	svc := NewLoginService(&fakeIdentityProvider{}, newFakeUserStore(), testSessionSecret)
	breakGlassSvc := NewBreakGlassLoginService(newFakeBreakGlassStore(), testSessionSecret)
	api, err := New(svc, breakGlassSvc, newConfiguredFakeBreakGlassStore(), "", testBreakGlassLoginPath, testAuthRateLimitMaxAttempts, testAuthRateLimitWindow, testAuthRecheckInterval, testSessionSecret, nil, nodeList, registrar, profileList, profileEditorFake, instances, launcher, transfers, users, auditLog, roster, elevator, settingsSvc, metricsSvc, eventsSrc, engineProvisionerFake, engineTransfersFake, testLogger())
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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
		{ID: "node-1", Name: "spark-1", Hostname: "spark-1.local", RuntimeBackend: db.RuntimeBackendBareMetal, AgentStatus: db.AgentStatusOnline, GPUMemoryGB: 128, CPUMemoryGB: 128},
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

func TestHandleModelProfiles_ListInstancesFails(t *testing.T) {
	profiles := &fakeProfileLister{profiles: []*db.Profile{{ID: "profile-1", Name: "my-profile"}}}
	instances := &fakeInstanceLister{err: errors.New("database unreachable")}
	api := newTestDashboardAPI(t, &fakeNodeLister{}, profiles, instances, &fakeTransferLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// TestHandleModelProfiles_ShowsInstanceStatusAndLoadControl exercises
// Dashboard UI Phase 11's addition to the Model profiles page: a running
// (non-terminal) instance's status is shown, a Developer-tier viewer sees
// the Load control on a profile with no active instance, and a terminal
// (stopped) instance leaves the profile eligible to load again rather
// than showing a stale status.
func TestHandleModelProfiles_ShowsInstanceStatusAndLoadControl(t *testing.T) {
	profiles := &fakeProfileLister{profiles: []*db.Profile{
		{ID: "profile-1", Name: "loaded-profile"},
		{ID: "profile-2", Name: "idle-profile"},
		{ID: "profile-3", Name: "stopped-profile"},
	}}
	instances := &fakeInstanceLister{instances: []*db.RunningInstance{
		{ID: "instance-1", ProfileID: "profile-1", Status: db.RunningInstanceStatusRunning},
		{ID: "instance-3", ProfileID: "profile-3", Status: db.RunningInstanceStatusStopped},
	}}
	users := newFakeUserLister()
	users.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, profiles, instances, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "status-running") {
		t.Errorf("response does not show the running instance's status: %s", body)
	}
	if !strings.Contains(body, `/instances/instance-1/unload`) {
		t.Errorf("response does not offer Unload for the running instance: %s", body)
	}
	if !strings.Contains(body, `/profiles/profile-2/load`) {
		t.Errorf("response does not offer Load for the idle profile: %s", body)
	}
	if !strings.Contains(body, `/profiles/profile-3/load`) {
		t.Errorf("response does not offer Load for the profile with only a stopped instance: %s", body)
	}
}

func TestHandleModelProfiles_LoadControlHiddenWithoutCanLaunch(t *testing.T) {
	profiles := &fakeProfileLister{profiles: []*db.Profile{{ID: "profile-1", Name: "my-profile"}}}
	users := newFakeUserLister()
	users.byID["ro-1"] = &db.User{ID: "ro-1", Tier: db.TierReadOnly}
	api := newTestDashboardAPIWithAdmin(t, &fakeNodeLister{}, profiles, &fakeInstanceLister{}, &fakeTransferLister{}, users, &fakeAuditLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "ro-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/profiles/profile-1/load") {
		t.Errorf("Read-only viewer should not be offered Load: %s", body)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html - a friendly page, not the old raw JSON 403", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access denied") {
		t.Errorf("body does not contain %q: %s", "Access denied", body)
	}
	if !strings.Contains(body, "developer") {
		t.Errorf("body does not contain the viewer's current tier %q: %s", "developer", body)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html - a friendly page, not the old raw JSON 403", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access denied") {
		t.Errorf("body does not contain %q: %s", "Access denied", body)
	}
	if !strings.Contains(body, "developer") {
		t.Errorf("body does not contain the viewer's current tier %q: %s", "developer", body)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html - a friendly page, not the old raw JSON 403", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access denied") {
		t.Errorf("body does not contain %q: %s", "Access denied", body)
	}
	if !strings.Contains(body, "developer") {
		t.Errorf("body does not contain the viewer's current tier %q: %s", "developer", body)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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
	profiles := &fakeProfileLister{profiles: []*db.Profile{{ID: "profile-1", Name: "llama-70b"}}}
	port := 8000
	instances := &fakeInstanceLister{instances: []*db.RunningInstance{{
		ID: "instance-1", ProfileID: "profile-1", PrimaryNodeID: "node-1",
		Status: db.RunningInstanceStatusRunning, ActualPort: &port,
	}}}
	recordedAt := time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)
	metricsSvc := &fakeMetricsLister{
		latestByNode: []*db.Metric{{
			NodeID: "node-1", RecordedAt: recordedAt,
			CPUUtilizationPct: 10, SystemMemoryUsedMB: 4096, SystemMemoryTotalMB: 16384,
		}},
		recent: []*db.Metric{{NodeID: "node-1", RecordedAt: recordedAt, CPUUtilizationPct: 10}},
		latestGPUByNode: []*db.GPUMetric{{
			NodeID: "node-1", GPUIndex: 0, RecordedAt: recordedAt,
			UtilizationPct: 42.5, MemoryUsedMB: 8192, MemoryTotalMB: 24576,
		}},
		recentGPU: []*db.GPUMetric{{NodeID: "node-1", GPUIndex: 0, RecordedAt: recordedAt, UtilizationPct: 42.5}},
	}
	api := newTestDashboardAPIWithMetrics(t, nodes, profiles, instances, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

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
	if !strings.Contains(body, "llama-70b") || !strings.Contains(body, "8000") {
		t.Errorf("response does not show the running model name and port: %s", body)
	}
	if !strings.Contains(body, `"label":"spark-1 GPU 0"`) {
		t.Errorf("response does not embed the GPU chart series JSON with the resolved node/GPU label: %s", body)
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

func TestHandleMetrics_ListLatestGPUByNodeFails(t *testing.T) {
	metricsSvc := &fakeMetricsLister{latestGPUByNodeErr: errors.New("database unreachable")}
	api := newTestDashboardAPIWithMetrics(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleMetrics_ListRecentGPUFails(t *testing.T) {
	metricsSvc := &fakeMetricsLister{recentGPUErr: errors.New("database unreachable")}
	api := newTestDashboardAPIWithMetrics(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleMetricsChartData_ReturnsJSON(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	recordedAt := time.Now()
	metricsSvc := &fakeMetricsLister{
		recent:    []*db.Metric{{NodeID: "node-1", RecordedAt: recordedAt, CPUUtilizationPct: 10}},
		recentGPU: []*db.GPUMetric{{NodeID: "node-1", GPUIndex: 0, RecordedAt: recordedAt, UtilizationPct: 42.5}},
	}
	api := newTestDashboardAPIWithMetrics(t, nodes, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics/chart-data", "user-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	for _, key := range []string{"gpuUtilization", "gpuMemory", "cpu", "systemMemory"} {
		if _, ok := got[key]; !ok {
			t.Errorf("response missing key %q: %s", key, rec.Body.String())
		}
	}
}

func TestHandleMetricsChartData_ListRecentFails(t *testing.T) {
	metricsSvc := &fakeMetricsLister{recentErr: errors.New("database unreachable")}
	api := newTestDashboardAPIWithMetrics(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeSettingsViewer{}, metricsSvc)

	req := newAuthenticatedRequest(t, http.MethodGet, "/metrics/chart-data", "user-1")
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func TestHandleMetrics_HXRequest_ReturnsPartialOnly(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []*db.Node{{ID: "node-1", Name: "spark-1"}}}
	metricsSvc := &fakeMetricsLister{
		latestByNode:    []*db.Metric{{NodeID: "node-1", RecordedAt: time.Now()}},
		latestGPUByNode: []*db.GPUMetric{{NodeID: "node-1", GPUIndex: 0, RecordedAt: time.Now()}},
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
	addValidCSRF(req)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func registerNodeForm(overrides url.Values) url.Values {
	form := url.Values{
		"name":            {"spark-1"},
		"hostname":        {"spark-1.internal"},
		"ip_address":      {"10.0.0.11"},
		"runtime_backend": {"bare-metal"},
		"gpu_memory_gb":   {"128"},
		"cpu_memory_gb":   {"128"},
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
	if params.Name != "spark-1" || params.Hostname != "spark-1.internal" || params.RuntimeBackend != db.RuntimeBackendBareMetal {
		t.Errorf("RegisterNode params = %+v, want the submitted form values", params)
	}
	if params.GPUMemoryGB != 128 || params.CPUMemoryGB != 128 {
		t.Errorf("RegisterNode params memory = %v/%v, want 128/128", params.GPUMemoryGB, params.CPUMemoryGB)
	}
}

func TestHandleRegisterNode_DockerGPURuntimeBackend(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["admin-1"] = &db.User{ID: "admin-1", Tier: db.TierAdmin}
	registrar := &fakeNodeRegistrar{node: &db.Node{Name: "workstation-1"}, bearerToken: "token"}
	api := newTestDashboardAPIWithRegistrar(t, &fakeNodeLister{}, registrar, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	form := registerNodeForm(url.Values{"runtime_backend": {"podman"}})
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
	if params.RuntimeBackend != db.RuntimeBackendPodman {
		t.Errorf("RuntimeBackend = %v, want podman", params.RuntimeBackend)
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

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func TestHandleModelProfiles_CanManage_ShownForPowerDev(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "pd-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/profiles/new") {
		t.Errorf("response does not show a New profile link for a PowerDev viewer: %s", rec.Body.String())
	}
}

func TestHandleModelProfiles_CanManage_HiddenForDeveloper(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.Contains(rec.Body.String(), "/profiles/new") {
		t.Error("response shows a New profile link for a Developer viewer, want it hidden")
	}
}

func TestHandleNewProfileForm_PowerDevAccess(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/new", "pd-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `name="model_ref"`) {
		t.Errorf("response does not show the profile form: %s", rec.Body.String())
	}
}

func TestHandleNewProfileForm_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/new", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleNewProfileForm_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodGet, "/profiles/new", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want %q", loc, "/login")
	}
}

func TestHandleEditProfileForm_PrefillsExistingValues(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	nodeID := "node-1"
	memGB := 40.0
	quant := "Q4_K_M"
	image := "nvcr.io/nvidia/vllm:26.06-py3"
	profileEditorFake := &fakeProfileEditor{getResult: &db.Profile{
		ID: "profile-1", Name: "llama-70b", ModelRef: "meta-llama/Llama-3-70B", EngineType: db.ProfileEngineVLLM,
		EngineParams: []byte(`{"tensor_parallel_size":2}`), RequiredMemoryGB: &memGB, Quantization: &quant, Image: &image, TargetNodeID: &nodeID, Port: 8001,
	}}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/profile-1/edit", "pd-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "llama-70b") || !strings.Contains(body, "meta-llama/Llama-3-70B") {
		t.Errorf("response does not preserve the existing name/model_ref: %s", body)
	}
	if !strings.Contains(body, `tensor_parallel_size`) {
		t.Errorf("response does not preserve the existing engine_params: %s", body)
	}
	if !strings.Contains(body, "nvcr.io/nvidia/vllm:26.06-py3") {
		t.Errorf("response does not preserve the existing image override: %s", body)
	}
	if !strings.Contains(body, "Q4_K_M") {
		t.Errorf("response does not preserve the existing quantization: %s", body)
	}
	if !strings.Contains(body, "action=\"/profiles/profile-1/edit\"") {
		t.Errorf("response does not post back to the edit URL: %s", body)
	}
}

func TestHandleEditProfileForm_NotFound(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{getErr: db.ErrProfileNotFound}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/does-not-exist/edit", "pd-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleEditProfileForm_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/profile-1/edit", "dev-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func profileForm(overrides url.Values) url.Values {
	form := url.Values{
		"name":           {"llama-70b"},
		"model_ref":      {"meta-llama/Llama-3-70B"},
		"engine_type":    {"vllm"},
		"target_node_id": {"node-1"},
		"port":           {"8001"},
	}
	for k, v := range overrides {
		form[k] = v
	}
	return form
}

func TestHandleCreateProfile_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(nil))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/profiles" {
		t.Errorf("Location header = %q, want %q", got, "/profiles")
	}
	if len(profileEditorFake.created) != 1 {
		t.Fatalf("CreateProfile called %d times, want 1", len(profileEditorFake.created))
	}
	params := profileEditorFake.created[0]
	if params.Name != "llama-70b" || params.ModelRef != "meta-llama/Llama-3-70B" || params.EngineType != db.ProfileEngineVLLM {
		t.Errorf("CreateProfile params = %+v, want the submitted form values", params)
	}
	if params.Port != 8001 || params.TargetNodeID != "node-1" {
		t.Errorf("CreateProfile params port/target = %v/%v, want 8001/node-1", params.Port, params.TargetNodeID)
	}
	if string(params.EngineParams) != "{}" {
		t.Errorf("CreateProfile params.EngineParams = %s, want {} for a blank submission", params.EngineParams)
	}
}

func TestHandleCreateProfile_Quantization(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(url.Values{"engine_type": {"llamacpp"}, "quantization": {"Q4_K_M"}}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if len(profileEditorFake.created) != 1 {
		t.Fatalf("CreateProfile called %d times, want 1", len(profileEditorFake.created))
	}
	params := profileEditorFake.created[0]
	if params.Quantization == nil || *params.Quantization != "Q4_K_M" {
		t.Errorf("CreateProfile params.Quantization = %v, want %q", params.Quantization, "Q4_K_M")
	}
}

func TestHandleCreateProfile_Image(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(url.Values{"image": {"nvcr.io/nvidia/vllm:26.06-py3"}}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if len(profileEditorFake.created) != 1 {
		t.Fatalf("CreateProfile called %d times, want 1", len(profileEditorFake.created))
	}
	params := profileEditorFake.created[0]
	if params.Image == nil || *params.Image != "nvcr.io/nvidia/vllm:26.06-py3" {
		t.Errorf("CreateProfile params.Image = %v, want %q", params.Image, "nvcr.io/nvidia/vllm:26.06-py3")
	}
}

func TestHandleCreateProfile_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	profileEditorFake := &fakeProfileEditor{createErr: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "dev-1", profileForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHandleCreateProfile_InvalidProfile_RedisplaysFormWithError(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{createErr: profiles.ErrInvalidProfile}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "llama-70b") {
		t.Errorf("response does not preserve the submitted name value: %s", body)
	}
	if !strings.Contains(body, "form-error") {
		t.Errorf("response does not show the error message: %s", body)
	}
}

func TestHandleCreateProfile_NonNumericPort_BadRequest(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(url.Values{"port": {"not-a-number"}}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(profileEditorFake.created) != 0 {
		t.Error("CreateProfile was called despite a non-numeric port - input must be validated before the Service call")
	}
}

func TestHandleCreateProfile_Unauthenticated(t *testing.T) {
	api := newTestDashboardAPI(t, &fakeNodeLister{}, &fakeProfileLister{}, &fakeInstanceLister{}, &fakeTransferLister{})

	req := httptest.NewRequest(http.MethodPost, "/profiles/new", strings.NewReader(profileForm(nil).Encode()))
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

func TestHandleUpdateProfile_Success(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/edit", "pd-1", profileForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if len(profileEditorFake.updated) != 1 {
		t.Fatalf("UpdateProfile called %d times, want 1", len(profileEditorFake.updated))
	}
	if profileEditorFake.updated[0].ID != "profile-1" {
		t.Errorf("UpdateProfile params.ID = %q, want %q", profileEditorFake.updated[0].ID, "profile-1")
	}
}

func TestHandleUpdateProfile_NotFound(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	profileEditorFake := &fakeProfileEditor{updateErr: db.ErrProfileNotFound}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/does-not-exist/edit", "pd-1", profileForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleUpdateProfile_Forbidden(t *testing.T) {
	viewer := newFakeUserLister()
	viewer.byID["dev-1"] = &db.User{ID: "dev-1", Tier: db.TierDeveloper}
	profileEditorFake := &fakeProfileEditor{updateErr: rbac.ErrNotPermitted}
	api := newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, profileEditorFake, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})

	req := newAuthenticatedFormRequest(t, "/profiles/profile-1/edit", "dev-1", profileForm(nil))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
