// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var (
	admin      = rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}
	developer  = rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}
	breakGlass = rbac.Actor{IsSuperAdmin: true}
)

// fakeSettings is the settings row in memory, with the same restart rules as
// the SQL (a new schedule or switching on restarts the schedule's clock).
type fakeSettings struct {
	mu       sync.Mutex
	cfg      db.StaleContainerSettings
	clock    func() time.Time
	updates  int
	restored bool
	getErr   error
	markErr  error
}

func newFakeSettings(clock func() time.Time) *fakeSettings {
	return &fakeSettings{clock: clock, cfg: db.StaleContainerSettings{
		Mode: db.StaleContainerModeNone, ScheduleSetAt: clock(), MinAgeHours: 24, ArchiveLines: 2000, UpdatedAt: clock(),
	}}
}

func (f *fakeSettings) Get(context.Context) (*db.StaleContainerSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c := f.cfg
	return &c, nil
}

func (f *fakeSettings) Update(_ context.Context, u db.StaleContainerSettingsUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if string(f.cfg.Schedule) != string(u.Schedule) || f.cfg.Mode == db.StaleContainerModeNone {
		f.cfg.ScheduleSetAt, f.cfg.LastRunAt = f.clock(), nil
	}
	f.cfg.Mode, f.cfg.Schedule, f.cfg.MinAgeHours, f.cfg.ArchiveLines, f.cfg.UpdatedBy = u.Mode, u.Schedule, u.MinAgeHours, u.ArchiveLines, u.UpdatedBy
	return nil
}

func (f *fakeSettings) Restore(_ context.Context, prev *db.StaleContainerSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = true
	f.cfg = *prev
	return nil
}

func (f *fakeSettings) MarkRun(_ context.Context, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.cfg.LastRunAt = &at
	return nil
}

func (f *fakeSettings) SetLastRunSummary(_ context.Context, s json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.LastRunSummary = s
	return nil
}

type fakeNodes struct{ nodes []*db.Node }

func (f *fakeNodes) List(context.Context) ([]*db.Node, error) { return f.nodes, nil }
func (f *fakeNodes) FindByID(_ context.Context, id string) (*db.Node, error) {
	for _, n := range f.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, errors.New("node not found")
}

type fakeInstances struct {
	byID map[string]*db.RunningInstance
	err  error
}

func (f *fakeInstances) FindByIDs(_ context.Context, ids []string) (map[string]*db.RunningInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]*db.RunningInstance{}
	for _, id := range ids {
		if inst, ok := f.byID[id]; ok {
			out[id] = inst
		}
	}
	return out, nil
}

type fakeProfiles struct{ names map[string]string }

func (f *fakeProfiles) FindByID(_ context.Context, id string) (*db.Profile, error) {
	if name, ok := f.names[id]; ok {
		return &db.Profile{ID: id, Name: name}, nil
	}
	return nil, errors.New("profile not found")
}

// fakeAlerts records what the service raised and which keys it closed.
type fakeAlerts struct {
	mu       sync.Mutex
	open     map[string]alerts.RaiseParams // by dedupe key, for keyed alerts
	raised   []alerts.RaiseParams
	resolved []string
	prefixes []string // "<prefix>|<sorted keep>"
}

func newFakeAlerts() *fakeAlerts { return &fakeAlerts{open: map[string]alerts.RaiseParams{}} }

func (f *fakeAlerts) Raise(_ context.Context, p alerts.RaiseParams) (*db.RaiseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	f.raised = append(f.raised, p)
	if p.DedupeKey != "" {
		f.open[p.DedupeKey] = p
	}
	return &db.RaiseResult{Created: true}, nil
}

func (f *fakeAlerts) ResolveCondition(_ context.Context, key string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, key)
	if _, ok := f.open[key]; ok {
		delete(f.open, key)
		return 1, nil
	}
	return 0, nil
}

func (f *fakeAlerts) ResolveConditionsExcept(_ context.Context, prefix string, keep []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := append([]string(nil), keep...)
	sort.Strings(k)
	f.prefixes = append(f.prefixes, prefix+"|"+strings.Join(k, ","))
	keepSet := map[string]bool{}
	for _, key := range keep {
		keepSet[key] = true
	}
	n := 0
	for key := range f.open {
		if strings.HasPrefix(key, prefix) && !keepSet[key] {
			delete(f.open, key)
			n++
		}
	}
	return n, nil
}

func (f *fakeAlerts) isOpen(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.open[key]
	return ok
}

func (f *fakeAlerts) ofKind(kind string) []alerts.RaiseParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alerts.RaiseParams
	for _, p := range f.raised {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

type auditCall struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	objectID   string
	detail     map[string]any
}

type fakeAudit struct {
	mu    sync.Mutex
	calls []auditCall
	err   error
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, superAdmin bool, action, objectType, objectID string, detail map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, auditCall{actorID, superAdmin, action, objectType, objectID, detail})
	return nil
}

func (f *fakeAudit) actions(name string) []auditCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []auditCall
	for _, c := range f.calls {
		if c.action == name {
			out = append(out, c)
		}
	}
	return out
}

type fakeZone struct{ loc *time.Location }

func (z fakeZone) Location(context.Context) (*time.Location, error) { return z.loc, nil }

// fakeAgents is every node's agent in one place. It answers list_containers and
// remove_container the way a real agent would, by calling the Service's
// handlers from another goroutine - unless a test says a node misbehaves.
type fakeAgents struct {
	mu  sync.Mutex
	svc *Service

	online    map[string]bool
	holdings  map[string][]agentproto.ManagedContainer
	truncated map[string]bool

	// listMode per node: "" answers normally; "silent" never answers; "error"
	// answers with an error; "foreign" answers as a different node; "order"
	// sends its parts out of order; "huge" claims far too many containers.
	listMode map[string]string

	// removeOutcome and removeDetail script the answer for an instance;
	// the default is "removed". removeSilent never answers.
	removeOutcome map[string]string
	removeDetail  map[string]string
	removeSilent  map[string]bool
	removeDelay   time.Duration

	sent        []string
	inFlight    int
	maxInFlight int
}

func newFakeAgents() *fakeAgents {
	return &fakeAgents{
		online: map[string]bool{}, holdings: map[string][]agentproto.ManagedContainer{}, truncated: map[string]bool{},
		listMode: map[string]string{}, removeOutcome: map[string]string{}, removeDetail: map[string]string{}, removeSilent: map[string]bool{},
	}
}

func (f *fakeAgents) Connected(nodeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[nodeID]
}

func (f *fakeAgents) Send(_ context.Context, nodeID string, env agentproto.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch env.Type {
	case agentproto.TypeListContainers:
		var req agentproto.ListContainers
		if err := env.DecodePayload(&req); err != nil {
			return err
		}
		f.sent = append(f.sent, "list:"+nodeID)
		go f.answerList(nodeID, req.ScanID)
	case agentproto.TypeRemoveContainer:
		var req agentproto.RemoveContainer
		if err := env.DecodePayload(&req); err != nil {
			return err
		}
		f.sent = append(f.sent, fmt.Sprintf("remove:%s:%s:%d", nodeID, req.InstanceID, req.Lines))
		go f.answerRemove(nodeID, req)
	default:
		return fmt.Errorf("unexpected message %s", env.Type)
	}
	return nil
}

func (f *fakeAgents) answerList(nodeID, scanID string) {
	f.mu.Lock()
	mode, held, truncated := f.listMode[nodeID], append([]agentproto.ManagedContainer(nil), f.holdings[nodeID]...), f.truncated[nodeID]
	f.mu.Unlock()

	deliver := func(from string, part agentproto.ListContainersResult) {
		env, _ := agentproto.NewEnvelope(agentproto.TypeListContainersResult, "", part)
		f.svc.HandleListContainersResult(from, env)
	}
	switch mode {
	case "silent":
		return
	case "error":
		deliver(nodeID, agentproto.ListContainersResult{ScanID: scanID, Final: true, Error: "cannot connect to the docker daemon"})
		return
	case "foreign":
		deliver("some-other-node", agentproto.ListContainersResult{ScanID: scanID, Final: true, Containers: held})
		return
	case "order":
		deliver(nodeID, agentproto.ListContainersResult{ScanID: scanID, Part: 1, Final: true, Containers: held})
		return
	case "huge":
		for p := 0; p < 30; p++ {
			var big []agentproto.ManagedContainer
			for i := 0; i < agentproto.ListContainersChunkSize; i++ {
				big = append(big, agentproto.ManagedContainer{InstanceID: fmt.Sprintf("%08d-0000-4000-8000-%012d", p, i), State: "exited"})
			}
			deliver(nodeID, agentproto.ListContainersResult{ScanID: scanID, Part: p, Final: p == 29, Containers: big})
		}
		return
	}
	for part, off := 0, 0; ; part++ {
		end := min(off+agentproto.ListContainersChunkSize, len(held))
		deliver(nodeID, agentproto.ListContainersResult{ScanID: scanID, Part: part, Final: end == len(held), Containers: held[off:end], Truncated: truncated && end == len(held)})
		if end == len(held) {
			return
		}
		off = end
	}
}

func (f *fakeAgents) answerRemove(nodeID string, req agentproto.RemoveContainer) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	outcome, detail, silent, delay := f.removeOutcome[req.InstanceID], f.removeDetail[req.InstanceID], f.removeSilent[req.InstanceID], f.removeDelay
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if delay > 0 {
		time.Sleep(delay)
	}
	if silent {
		return
	}
	if outcome == "" {
		outcome = agentproto.RemoveOutcomeRemoved
	}
	if outcome == agentproto.RemoveOutcomeRemoved {
		// A removed container is no longer held, so the next scan does not see it.
		f.mu.Lock()
		kept := f.holdings[nodeID][:0:0]
		for _, c := range f.holdings[nodeID] {
			if c.InstanceID != req.InstanceID {
				kept = append(kept, c)
			}
		}
		f.holdings[nodeID] = kept
		f.mu.Unlock()
	}
	env, _ := agentproto.NewEnvelope(agentproto.TypeRemoveContainerResult, "", agentproto.RemoveContainerResult{
		RemoveID: req.RemoveID, InstanceID: req.InstanceID, Outcome: outcome, Detail: detail,
	})
	f.svc.HandleRemoveContainerResult(nodeID, env)
}

func (f *fakeAgents) removes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		if strings.HasPrefix(s, "remove:") {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeAgents) lists() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		if strings.HasPrefix(s, "list:") {
			out = append(out, s)
		}
	}
	return out
}

// fixture wires a Service to fakes, with two container nodes and one
// bare-metal node, at a fixed clock.
type fixture struct {
	svc       *Service
	settings  *fakeSettings
	nodes     *fakeNodes
	instances *fakeInstances
	alerts    *fakeAlerts
	audit     *fakeAudit
	agents    *fakeAgents
	clock     time.Time
}

const (
	nodeA     = "aaaaaaaa-0000-4000-8000-00000000000a"
	nodeB     = "aaaaaaaa-0000-4000-8000-00000000000b"
	nodeBare  = "aaaaaaaa-0000-4000-8000-00000000000c"
	inst1     = "10000000-0000-4000-8000-000000000001"
	inst2     = "10000000-0000-4000-8000-000000000002"
	inst3     = "10000000-0000-4000-8000-000000000003"
	inst4     = "10000000-0000-4000-8000-000000000004"
	inst5     = "10000000-0000-4000-8000-000000000005"
	inst6     = "10000000-0000-4000-8000-000000000006"
	profileID = "99999999-0000-4000-8000-000000000001"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clock: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.clock }
	f.settings = newFakeSettings(clock)
	f.nodes = &fakeNodes{nodes: []*db.Node{
		{ID: nodeA, Name: "spark-1", RuntimeBackend: db.RuntimeBackendDocker},
		{ID: nodeB, Name: "spark-2", RuntimeBackend: db.RuntimeBackendPodman},
		{ID: nodeBare, Name: "laptop", RuntimeBackend: db.RuntimeBackendBareMetal},
	}}
	f.instances = &fakeInstances{byID: map[string]*db.RunningInstance{}}
	f.alerts = newFakeAlerts()
	f.audit = &fakeAudit{}
	f.agents = newFakeAgents()
	f.agents.online[nodeA], f.agents.online[nodeB], f.agents.online[nodeBare] = true, true, true
	f.svc = NewService(f.settings, f.nodes, f.instances, &fakeProfiles{names: map[string]string{profileID: "qwen-8b"}}, f.agents, f.alerts, f.audit,
		fakeZone{loc: time.UTC}, log.New(io.Discard, "", 0))
	f.svc.now = clock
	f.svc.listTimeout = 200 * time.Millisecond
	f.svc.removeTimeout = 200 * time.Millisecond
	f.agents.svc = f.svc
	return f
}

func (f *fixture) ago(d time.Duration) time.Time { return f.clock.Add(-d) }

// held adds a container to a node's runtime.
func (f *fixture) held(nodeID, instanceID, state string, finishedAgo time.Duration) {
	mc := agentproto.ManagedContainer{
		InstanceID: instanceID, ProfileID: profileID, ContainerID: "cid-" + instanceID[:8], ContainerName: "sparky-qwen-" + instanceID[:8],
		State: state, Labelled: true,
	}
	if finishedAgo > 0 {
		t := f.ago(finishedAgo)
		mc.FinishedAt = &t
		code := 137
		mc.ExitCode = &code
	}
	f.agents.holdings[nodeID] = append(f.agents.holdings[nodeID], mc)
}

// instance records a running_instances row for a node.
func (f *fixture) instance(id, nodeID string, status db.RunningInstanceStatus) {
	f.instances.byID[id] = &db.RunningInstance{ID: id, ProfileID: profileID, Status: status, PrimaryNodeID: nodeID}
}

const day = 24 * time.Hour
