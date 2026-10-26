// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// populate gives node A one of everything: two stale containers, one that is
// too recent, a live one, a dead instance (exited container, instance still
// recorded running), and a running container Sparky has no record of.
func (f *fixture) populate() {
	f.held(nodeA, inst1, "exited", 5*day) // stale, instance stopped
	f.instance(inst1, nodeA, db.RunningInstanceStatusStopped)
	f.held(nodeA, inst2, "exited", 3*time.Hour) // recent
	f.instance(inst2, nodeA, db.RunningInstanceStatusStopped)
	f.held(nodeA, inst3, "running", 0) // live
	f.instance(inst3, nodeA, db.RunningInstanceStatusRunning)
	f.held(nodeA, inst4, "running", 0)    // running with no record: an orphan
	f.held(nodeA, inst5, "exited", 3*day) // exited with no record: stale
	f.held(nodeA, inst6, "exited", 9*day) // exited, but its instance is recorded running: a dead instance
	f.instance(inst6, nodeA, db.RunningInstanceStatusRunning)
	f.agents.online[nodeB] = false
}

func verdicts(ns NodeScan) map[string]Verdict {
	out := map[string]Verdict{}
	for _, it := range ns.Items {
		out[it.InstanceID] = it.Verdict
	}
	return out
}

func TestPreview_ClassifiesEverythingAndOnlyAsksContainerNodes(t *testing.T) {
	f := newFixture(t)
	f.populate()
	res, err := f.svc.Preview(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 || res.Nodes[0].NodeID != nodeA || res.Nodes[1].NodeID != nodeB {
		t.Fatalf("nodes = %+v, want spark-1 and spark-2 and not the bare-metal laptop", res.Nodes)
	}
	if res.MinAge.Hours() != 24 || res.ArchiveLines != 2000 {
		t.Errorf("min age %v lines %d", res.MinAge, res.ArchiveLines)
	}

	a := res.Nodes[0]
	if a.Status != ScanDone || a.NodeName != "spark-1" {
		t.Fatalf("node A = %+v", a)
	}
	want := map[string]Verdict{inst1: VerdictStale, inst2: VerdictRecent, inst3: VerdictLive, inst4: VerdictOrphanLive, inst5: VerdictStale, inst6: VerdictLive}
	got := verdicts(a)
	for id, v := range want {
		if got[id] != v {
			t.Errorf("instance %s: %q, want %q", id[len(id)-1:], got[id], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d containers, want %d", len(got), len(want))
	}
	stale := a.With(VerdictStale)
	if len(stale) != 2 || stale[0].InstanceID != inst1 {
		t.Errorf("stale = %+v, want the oldest first", stale)
	}
	if stale[0].ProfileName != "qwen-8b" || stale[0].InstanceStatus != "stopped" || stale[1].InstanceStatus != "" {
		t.Errorf("profile/status: %+v / %+v", stale[0], stale[1])
	}

	if b := res.Nodes[1]; b.Status != ScanOffline || b.Error == "" || len(b.Items) != 0 {
		t.Errorf("offline node = %+v", b)
	}
	if lists := f.agents.lists(); len(lists) != 1 || lists[0] != "list:"+nodeA {
		t.Errorf("asked %v, want only the one online container node", lists)
	}
	if len(f.agents.removes()) != 0 || len(f.audit.calls) != 0 {
		t.Error("a preview removed or audited something")
	}
}

func TestPreview_AdminOnly(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Preview(context.Background(), developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	if len(f.agents.lists()) != 0 {
		t.Error("a refused preview still asked a node")
	}
}

func TestPreview_ANodeThatMisbehavesIsFailedNotTrusted(t *testing.T) {
	cases := map[string]struct {
		mode string
		want string
	}{
		"never answers (an older agent)": {"silent", "upgrade agents"},
		"reports an error":               {"error", "docker daemon"},
		"is answered by someone else":    {"foreign", "did not answer"},
		"sends its parts out of order":   {"order", "out of order"},
		"claims far too many":            {"huge", "more containers than expected"},
	}
	for name, c := range cases {
		f := newFixture(t)
		f.held(nodeA, inst1, "exited", 5*day)
		f.held(nodeB, inst2, "exited", 5*day)
		f.agents.listMode[nodeA] = c.mode
		res, err := f.svc.Preview(context.Background(), admin)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, b := res.Nodes[0], res.Nodes[1]
		if a.Status != ScanFailed || !strings.Contains(a.Error, c.want) || len(a.Items) != 0 {
			t.Errorf("%s: node A = %q %q with %d items, want failed mentioning %q and nothing from it", name, a.Status, a.Error, len(a.Items), c.want)
		}
		// One bad node never spoils another.
		if b.Status != ScanDone || len(b.Items) != 1 {
			t.Errorf("%s: node B = %q with %d items, want it unaffected", name, b.Status, len(b.Items))
		}
	}
}

// If the database cannot be asked which containers belong to live instances,
// nothing may be called stale: every container would look like an unknown one.
func TestPreview_ALostInstanceLookupMakesNothingStale(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.instances.err = errors.New("db down")
	res, err := f.svc.Preview(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Nodes[0]
	if a.Status != ScanFailed || len(a.Items) != 0 || len(a.With(VerdictStale)) != 0 {
		t.Errorf("node A = %q with %d items; want failed with nothing classified", a.Status, len(a.Items))
	}
}

func TestPreview_ALongListArrivesWhole(t *testing.T) {
	f := newFixture(t)
	n := 2*agentproto.ListContainersChunkSize + 17
	for i := 0; i < n; i++ {
		f.held(nodeA, fmt.Sprintf("20000000-0000-4000-8000-%012d", i), "exited", 5*day)
	}
	f.agents.truncated[nodeA] = true
	res, err := f.svc.Preview(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Nodes[0]; a.Status != ScanDone || len(a.Items) != n || !a.Truncated {
		t.Errorf("node A: %q, %d of %d items, truncated %v", a.Status, len(a.Items), n, a.Truncated)
	}
}

func TestPreview_DropsAndCleansWhatANodeSendsBadly(t *testing.T) {
	f := newFixture(t)
	good := agentproto.ManagedContainer{InstanceID: strings.ToUpper(inst1), ContainerName: "name\x1b[31m\nwith\tcontrolé", State: "EXITED", Labelled: true, ProfileID: "not-a-uuid"}
	f.agents.holdings[nodeA] = []agentproto.ManagedContainer{
		good,
		{InstanceID: "not-a-uuid", State: "exited"},
		{InstanceID: "'; DROP TABLE running_instances; --", State: "exited"},
		{InstanceID: "", State: "exited"},
	}
	res, err := f.svc.Preview(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Nodes[0]
	if len(a.Items) != 1 {
		t.Fatalf("%d items, want only the well-formed one", len(a.Items))
	}
	it := a.Items[0]
	if it.InstanceID != inst1 || it.State != "exited" || it.ProfileID != "" {
		t.Errorf("item = %+v: id lower-cased, state lower-cased, bad profile id dropped", it.Container)
	}
	for _, r := range it.ContainerName {
		if r < 0x20 || r >= 0x7f {
			t.Errorf("name %q holds a non-printable or non-ASCII character", it.ContainerName)
			break
		}
	}
}

func TestHandleResults_IgnoreWhatIsNotWaitedFor(t *testing.T) {
	f := newFixture(t)
	// Nothing is waiting: none of these may panic or block.
	listEnv, _ := agentproto.NewEnvelope(agentproto.TypeListContainersResult, "", agentproto.ListContainersResult{ScanID: "unknown", Final: true})
	f.svc.HandleListContainersResult(nodeA, listEnv)
	rmEnv, _ := agentproto.NewEnvelope(agentproto.TypeRemoveContainerResult, "", agentproto.RemoveContainerResult{RemoveID: "unknown", InstanceID: inst1, Outcome: "removed"})
	f.svc.HandleRemoveContainerResult(nodeA, rmEnv)
	// Malformed payloads are logged and dropped.
	bad := agentproto.Envelope{Type: agentproto.TypeListContainersResult, Payload: []byte(`{"scan_id": 5}`)}
	f.svc.HandleListContainersResult(nodeA, bad)
	f.svc.HandleRemoveContainerResult(nodeA, agentproto.Envelope{Type: agentproto.TypeRemoveContainerResult, Payload: []byte(`[]`)})
	if len(f.svc.listWaits) != 0 || len(f.svc.removeWaits) != 0 {
		t.Error("stray answers left waits behind")
	}
}
