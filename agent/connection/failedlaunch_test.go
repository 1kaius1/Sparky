// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"errors"
	"reflect"
	"testing"
	"time"

	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

const testProfile = "11111111-aaaa-bbbb-cccc-000000000001"

func loadEnvelope(t *testing.T, profileID string) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID: "new-1", ModelRef: "org/model", EngineType: "vllm", Image: "vllm/vllm-openai:latest", Port: 8000,
		RequiresFullGPUResidency: true, ProfileID: profileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (h *archiveHarness) snapshotEvents() []string {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()
	return append([]string(nil), h.rt.events...)
}

// reasons returns the reason of every archive upload started, in order.
func (h *archiveHarness) reasons() []string {
	var out []string
	for _, c := range h.gotChunks() {
		if c.Meta != nil {
			out = append(out, c.Meta.Reason)
		}
	}
	return out
}

// firstResult waits for the first instance_result the agent sends.
func (h *archiveHarness) firstResult(t *testing.T) agentproto.InstanceResult {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env := <-h.msgs:
			if env.Type == agentproto.TypeInstanceResult {
				var r agentproto.InstanceResult
				if err := env.DecodePayload(&r); err != nil {
					t.Fatal(err)
				}
				return r
			}
		case <-deadline:
			t.Fatal("no instance_result")
		}
	}
}

func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

// A launch that fails to start leaves nothing behind: whatever Start created
// is stopped, its log saved, and only then removed.
func TestLoad_StartFailureArchivesAndRemovesWhatWasLeft(t *testing.T) {
	rt := &fakeRuntimeBackend{startErr: errors.New("start container abc: bind: address already in use"), captureResult: agentruntime.Capture{Log: "created, never started\n", LinesKept: 1, State: "created"}}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), nil, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusFailed || r.ErrorMessage == "" {
		t.Errorf("result = %+v, want failed with the start error", r)
	}
	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })
	ev := h.snapshotEvents()
	if want := []string{"start:new-1", "halt:new-1", "capture:new-1", "remove:new-1"}; !reflect.DeepEqual(ev, want) {
		t.Errorf("events = %v, want %v", ev, want)
	}
	if got := h.reasons(); !reflect.DeepEqual(got, []string{"failed_launch"}) {
		t.Errorf("archive reasons = %v, want [failed_launch]", got)
	}
}

// A launch that starts but never becomes ready (a hung or crashed engine) is
// reported failed first, then stopped, archived and removed.
func TestLoad_ReadinessFailureStopsArchivesAndRemoves(t *testing.T) {
	rt := &fakeRuntimeBackend{isRunningResult: false, logsResult: "boom\n", captureResult: agentruntime.Capture{Log: "boom\n", LinesKept: 1, State: "exited"}}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), func(c *Config) { c.InstanceStartupTimeout = fastReadinessTimeout() }, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusFailed {
		t.Errorf("result = %+v, want failed", r)
	}
	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })
	ev := h.snapshotEvents()
	hi, ci, ri := indexOf(ev, "halt:new-1"), indexOf(ev, "capture:new-1"), indexOf(ev, "remove:new-1")
	if hi < 0 || !(hi < ci && ci < ri) {
		t.Errorf("events = %v, want halt, then capture, then remove", ev)
	}
	if got := h.reasons(); !reflect.DeepEqual(got, []string{"failed_launch"}) {
		t.Errorf("archive reasons = %v", got)
	}
}

// If the log cannot be stored, the failed launch's container is kept - the
// no-removal-without-a-stored-log rule applies here too - but the failure is
// still reported.
func TestLoad_FailedLaunchKeepsTheContainerWhenTheArchiveIsNotConfirmed(t *testing.T) {
	rt := &fakeRuntimeBackend{startErr: errors.New("boom"), captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), nil, nil) // never confirmed
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusFailed {
		t.Errorf("result = %+v", r)
	}
	time.Sleep(h.conn.archiveAckTimeout + 200*time.Millisecond)
	if n := h.removeCount(); n != 0 {
		t.Errorf("Remove called %d times without a confirmation", n)
	}
	if len(h.gotChunks()) == 0 {
		t.Error("the log was never uploaded")
	}
}

// An engine that cannot be stopped is left alone; nothing is captured or
// removed behind its back.
func TestLoad_FailedLaunchThatCannotBeStoppedIsLeftAlone(t *testing.T) {
	rt := &fakeRuntimeBackend{startErr: errors.New("boom"), haltErr: errors.New("daemon says no")}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), nil, confirmStored)
	defer h.stop()

	h.firstResult(t)
	waitFor(t, "the stop attempt", func() bool { return indexOf(h.snapshotEvents(), "halt:new-1") >= 0 })
	time.Sleep(100 * time.Millisecond)
	if ev := h.snapshotEvents(); indexOf(ev, "capture:new-1") >= 0 || indexOf(ev, "remove:new-1") >= 0 {
		t.Errorf("events = %v, want no capture or remove after a failed stop", ev)
	}
}

// A load that succeeds archives and removes nothing.
func TestLoad_SuccessfulLaunchLeavesEverythingAlone(t *testing.T) {
	_, port := newFakeEngineServer(t)
	env, _ := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID: "new-1", ModelRef: "org/model", EngineType: "vllm", Image: "img", Port: port, RequiresFullGPUResidency: true, ProfileID: testProfile,
	})
	rt := &fakeRuntimeBackend{isRunningResult: true}
	h := newArchiveHarnessFor(t, rt, env, func(c *Config) { c.InstanceStartupTimeout = 5 * time.Second }, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusRunning {
		t.Fatalf("result = %+v, want running", r)
	}
	if ev := h.snapshotEvents(); indexOf(ev, "halt:new-1") >= 0 || indexOf(ev, "remove:new-1") >= 0 || len(h.gotChunks()) != 0 {
		t.Errorf("events = %v chunks = %d, want a plain start and nothing archived", ev, len(h.gotChunks()))
	}
}

// Replace-on-launch: every older container of the profile is stopped, archived
// and removed before the new one is started.
func TestLoad_ReplacesTheProfilesOlderContainersBeforeStarting(t *testing.T) {
	rt := &fakeRuntimeBackend{
		startErr:      errors.New("stop here"), // the outcome of the new launch is not the point
		forProfile:    map[string][]string{testProfile: {"old-1", "old-2"}},
		captureResult: agentruntime.Capture{Log: "old log\n", LinesKept: 1, State: "exited"},
	}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, testProfile), nil, confirmStored)
	defer h.stop()

	waitFor(t, "both old containers removed", func() bool { return h.removeCount() >= 2 })
	ev := h.snapshotEvents()
	want := []string{"halt:old-1", "capture:old-1", "remove:old-1", "halt:old-2", "capture:old-2", "remove:old-2", "start:new-1"}
	if len(ev) < len(want) || !reflect.DeepEqual(ev[:len(want)], want) {
		t.Errorf("events = %v, want it to begin %v", ev, want)
	}
	reasons := h.reasons()
	if len(reasons) < 2 || reasons[0] != "replaced" || reasons[1] != "replaced" {
		t.Errorf("archive reasons = %v, want the first two to be replaced", reasons)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !reflect.DeepEqual(rt.forProfileCalls, []string{testProfile}) {
		t.Errorf("InstancesForProfile asked for %v", rt.forProfileCalls)
	}
	if len(rt.startCalls) != 1 || rt.startCalls[0].ProfileID != testProfile {
		t.Errorf("Start got %+v, want the spec to carry the profile id", rt.startCalls)
	}
}

// The agent never touches an instance it is itself tracking as running, nor
// the one being launched.
func TestLoad_ReplaceSkipsTrackedAndCurrentInstances(t *testing.T) {
	rt := &fakeRuntimeBackend{
		startErr:      errors.New("stop here"),
		forProfile:    map[string][]string{testProfile: {"live-1", "new-1", "old-1"}},
		captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1},
	}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, testProfile), nil, confirmStored)
	defer h.stop()
	h.conn.trackActiveInstance("live-1", 9000, "/m", "vllm")

	waitFor(t, "old-1 removed", func() bool { return h.removeCount() >= 1 })
	ev := h.snapshotEvents()
	for _, bad := range []string{"halt:live-1", "capture:live-1", "remove:live-1"} {
		if indexOf(ev, bad) >= 0 {
			t.Errorf("events = %v: a tracked running instance was touched (%s)", ev, bad)
		}
	}
	if indexOf(ev, "remove:old-1") < 0 {
		t.Errorf("events = %v, want old-1 replaced", ev)
	}
}

// A stale container whose log cannot be stored is kept, but the new launch
// still goes ahead.
func TestLoad_ReplaceFailureKeepsTheOldContainerButStillLaunches(t *testing.T) {
	rt := &fakeRuntimeBackend{
		startErr:      errors.New("stop here"),
		forProfile:    map[string][]string{testProfile: {"old-1"}},
		captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1},
	}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, testProfile), nil, nil) // the old log is never confirmed
	defer h.stop()

	waitFor(t, "the launch to be attempted", func() bool { return indexOf(h.snapshotEvents(), "start:new-1") >= 0 })
	if indexOf(h.snapshotEvents(), "remove:old-1") >= 0 {
		t.Error("the old container was removed without a stored log")
	}
}

func TestLoad_NoProfileIDSkipsReplacement(t *testing.T) {
	rt := &fakeRuntimeBackend{startErr: errors.New("stop here"), forProfile: map[string][]string{"": {"old-1"}}}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), nil, confirmStored)
	defer h.stop()

	h.firstResult(t)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.forProfileCalls) != 0 {
		t.Errorf("InstancesForProfile called for an empty profile id: %v", rt.forProfileCalls)
	}
}

func TestLoad_ReplaceLookupFailureDoesNotBlockTheLaunch(t *testing.T) {
	rt := &fakeRuntimeBackend{startErr: errors.New("stop here"), forProfileErr: errors.New("daemon unreachable")}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, testProfile), nil, confirmStored)
	defer h.stop()

	h.firstResult(t)
	if indexOf(h.snapshotEvents(), "start:new-1") < 0 {
		t.Error("the launch was blocked by a failed stale-container lookup")
	}
}
