// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

const (
	staleA = "11111111-1111-1111-1111-111111111111"
	staleB = "22222222-2222-2222-2222-222222222222"
)

func listEnvelope(t *testing.T) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeListContainers, "", agentproto.ListContainers{ScanID: "scan-0001"})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func removeEnvelope(t *testing.T, instanceID string, lines int) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeRemoveContainer, "", agentproto.RemoveContainer{RemoveID: "rm-0001", InstanceID: instanceID, Lines: lines})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// nextMessage waits for the central app to receive a message of the given
// type and returns it, skipping the others (log chunks, instance results).
func nextMessage(t *testing.T, h *archiveHarness, want agentproto.MessageType) agentproto.Envelope {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env := <-h.msgs:
			if env.Type == want {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s message", want)
		}
	}
}

func exitedContainer(instance string, labelled bool) agentruntime.ManagedContainer {
	code := 137
	return agentruntime.ManagedContainer{
		InstanceID: instance, ContainerID: "cid-" + instance[:4], ContainerName: "sparky-p-1", State: "exited",
		Labelled: labelled, ExitCode: &code, FinishedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC),
	}
}

func TestListContainers_DescribesWhatTheRuntimeHoldsAndTouchesNothing(t *testing.T) {
	running := agentruntime.ManagedContainer{InstanceID: staleB, State: "running", Labelled: true, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	rt := &fakeRuntimeBackend{listed: []agentruntime.ManagedContainer{exitedContainer(staleA, true), running}, listTruncated: true}
	h := newArchiveHarnessFor(t, rt, listEnvelope(t), nil, nil)
	defer h.stop()

	var res agentproto.ListContainersResult
	if err := nextMessage(t, h, agentproto.TypeListContainersResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.ScanID != "scan-0001" || res.Part != 0 || !res.Final || !res.Truncated || res.Error != "" || len(res.Containers) != 2 {
		t.Fatalf("result = %+v", res)
	}
	a, b := res.Containers[0], res.Containers[1]
	if a.InstanceID != staleA || a.State != "exited" || !a.Labelled || a.ExitCode == nil || *a.ExitCode != 137 || a.FinishedAt == nil || a.CreatedAt != nil {
		t.Errorf("exited = %+v (finished time present, created absent)", a)
	}
	if b.InstanceID != staleB || b.State != "running" || b.ExitCode != nil || b.FinishedAt != nil || b.CreatedAt == nil {
		t.Errorf("running = %+v", b)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.haltCalls)+len(rt.removeCalls)+len(rt.stopCalls)+len(rt.captureCalls) != 0 {
		t.Errorf("listing changed or read something: halt=%v remove=%v stop=%v capture=%v", rt.haltCalls, rt.removeCalls, rt.stopCalls, rt.captureCalls)
	}
}

func TestListContainers_ARuntimeFailureIsAnsweredNotSwallowed(t *testing.T) {
	rt := &fakeRuntimeBackend{listErr: errors.New("cannot connect to the docker daemon")}
	h := newArchiveHarnessFor(t, rt, listEnvelope(t), nil, nil)
	defer h.stop()

	var res agentproto.ListContainersResult
	if err := nextMessage(t, h, agentproto.TypeListContainersResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.ScanID != "scan-0001" || !res.Final || !strings.Contains(res.Error, "docker daemon") || len(res.Containers) != 0 {
		t.Errorf("result = %+v", res)
	}
}

// readListParts collects the parts of one list_containers answer, in arrival
// order, up to and including the Final one.
func readListParts(t *testing.T, h *archiveHarness) []agentproto.ListContainersResult {
	t.Helper()
	var parts []agentproto.ListContainersResult
	for {
		var res agentproto.ListContainersResult
		if err := nextMessage(t, h, agentproto.TypeListContainersResult).DecodePayload(&res); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, res)
		if res.Final {
			return parts
		}
	}
}

func TestListContainers_ALongListIsAnsweredInNumberedParts(t *testing.T) {
	total := 2*agentproto.ListContainersChunkSize + 15
	var listed []agentruntime.ManagedContainer
	for i := 0; i < total; i++ {
		listed = append(listed, exitedContainer(fmt.Sprintf("%08d-0000-0000-0000-000000000000", i), true))
	}
	rt := &fakeRuntimeBackend{listed: listed, listTruncated: true}
	h := newArchiveHarnessFor(t, rt, listEnvelope(t), nil, nil)
	defer h.stop()

	parts := readListParts(t, h)
	if len(parts) != 3 {
		t.Fatalf("%d parts, want 3", len(parts))
	}
	seen := 0
	for i, p := range parts {
		if p.Part != i || p.ScanID != "scan-0001" || p.Final != (i == len(parts)-1) || p.Truncated != (i == len(parts)-1) {
			t.Errorf("part %d = part %d, final %v, truncated %v", i, p.Part, p.Final, p.Truncated)
		}
		if len(p.Containers) > agentproto.ListContainersChunkSize {
			t.Errorf("part %d holds %d containers, over the %d cap", i, len(p.Containers), agentproto.ListContainersChunkSize)
		}
		for _, c := range p.Containers {
			if want := fmt.Sprintf("%08d-0000-0000-0000-000000000000", seen); c.InstanceID != want {
				t.Fatalf("container %d is %s, want %s: order or content lost across parts", seen, c.InstanceID, want)
			}
			seen++
		}
	}
	if seen != total {
		t.Errorf("%d containers across the parts, want %d", seen, total)
	}
}

func TestListContainers_AnEmptyListIsOneEmptyFinalPart(t *testing.T) {
	h := newArchiveHarnessFor(t, &fakeRuntimeBackend{}, listEnvelope(t), nil, nil)
	defer h.stop()
	parts := readListParts(t, h)
	if len(parts) != 1 || parts[0].Part != 0 || !parts[0].Final || len(parts[0].Containers) != 0 || parts[0].Error != "" {
		t.Errorf("parts = %+v", parts)
	}
}

func TestListContainers_BoundsWhatItSendsWhateverTheRuntimeReports(t *testing.T) {
	c := exitedContainer(staleA, true)
	c.ContainerName = strings.Repeat("é\x01\n", 200)
	c.ContainerID = strings.Repeat("a", 300)
	c.State = "ex\x00ited" + strings.Repeat("x", 100)
	h := newArchiveHarnessFor(t, &fakeRuntimeBackend{listed: []agentruntime.ManagedContainer{c}}, listEnvelope(t), nil, nil)
	defer h.stop()
	got := readListParts(t, h)[0].Containers[0]
	if len(got.ContainerName) != agentproto.MaxContainerNameLen || strings.Trim(got.ContainerName, "?") != "" ||
		len(got.ContainerID) != agentproto.MaxContainerIDLen || len(got.State) != 32 || !strings.HasPrefix(got.State, "ex?ited") {
		t.Errorf("name %q (%d bytes), id %d bytes, state %q: want printable ASCII at the length caps", got.ContainerName, len(got.ContainerName), len(got.ContainerID), got.State)
	}
}

func TestWireTextAndClip(t *testing.T) {
	if got := wireText("sparky-model_1.0", 64); got != "sparky-model_1.0" {
		t.Errorf("a plain name was changed: %q", got)
	}
	if got := wireText("a\"b\\c", 64); got != "a\"b\\c" {
		t.Errorf("printable punctuation was changed: %q", got)
	}
	if got := wireText("é\t", 64); got != "??" {
		t.Errorf("wireText(é, tab) = %q, want ??", got)
	}
	// clip never splits a character, which matters for the free-text detail.
	if got := clip("ééé", 5); got != "éé" || !utf8.ValidString(got) {
		t.Errorf("clip(ééé, 5) = %q", got)
	}
	if got := clip("abc", 10); got != "abc" {
		t.Errorf("clip(abc, 10) = %q", got)
	}
}

func TestRemoveContainer_ArchivesWithTheChosenLinesThenRemovesOnlyAfterConfirmation(t *testing.T) {
	for _, lines := range []int{0, 50, 2000} {
		rt := &fakeRuntimeBackend{
			listed:        []agentruntime.ManagedContainer{exitedContainer(staleA, true)},
			captureResult: agentruntime.Capture{ContainerName: "sparky-p-1", State: "exited", Log: "boom\n", LinesKept: 1},
		}
		h := newArchiveHarnessFor(t, rt, removeEnvelope(t, staleA, lines), nil, confirmStored)
		var res agentproto.RemoveContainerResult
		if err := nextMessage(t, h, agentproto.TypeRemoveContainerResult).DecodePayload(&res); err != nil {
			t.Fatal(err)
		}
		if res.RemoveID != "rm-0001" || res.InstanceID != staleA || res.Outcome != agentproto.RemoveOutcomeRemoved || res.Detail != "" {
			t.Errorf("lines %d: result = %+v", lines, res)
		}
		log, meta := reassemble(t, h.gotChunks())
		if log != "boom\n" || meta.Reason != "stale_cleanup" || meta.LinesRequested != lines || meta.InstanceID != staleA {
			t.Errorf("lines %d: archived %q with meta %+v", lines, log, meta)
		}
		rt.mu.Lock()
		events, captureLines := append([]string(nil), rt.events...), append([]int(nil), rt.captureLines...)
		rt.mu.Unlock()
		h.stop()
		if len(captureLines) != 1 || captureLines[0] != lines {
			t.Errorf("lines %d: Capture asked for %v", lines, captureLines)
		}
		want := []string{"capture:" + staleA, "remove:" + staleA}
		if strings.Join(events, ",") != strings.Join(want, ",") {
			t.Errorf("lines %d: runtime events = %v, want %v (never a halt: the container is already stopped)", lines, events, want)
		}
	}
}

func TestRemoveContainer_AnUnlabelledLegacyContainerIsRemovedOnceExited(t *testing.T) {
	rt := &fakeRuntimeBackend{
		listed:        []agentruntime.ManagedContainer{exitedContainer(staleA, false)},
		captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1},
	}
	h := newArchiveHarnessFor(t, rt, removeEnvelope(t, staleA, 100), nil, confirmStored)
	defer h.stop()
	var res agentproto.RemoveContainerResult
	if err := nextMessage(t, h, agentproto.TypeRemoveContainerResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentproto.RemoveOutcomeRemoved {
		t.Errorf("result = %+v", res)
	}
}

// Every refusal must leave the runtime exactly as it was: nothing read,
// archived, halted or removed.
func TestRemoveContainer_RefusesAnythingThatIsNotAStoppedContainer(t *testing.T) {
	state := func(s string, labelled bool) agentruntime.ManagedContainer {
		c := exitedContainer(staleA, labelled)
		c.State = s
		return c
	}
	cases := map[string]struct {
		id        string
		lines     int
		container agentruntime.ManagedContainer
		active    bool
		mention   string
	}{
		"running":                           {staleA, 10, state("running", true), false, "running"},
		"paused":                            {staleA, 10, state("paused", true), false, "paused"},
		"restarting":                        {staleA, 10, state("restarting", true), false, "restarting"},
		"removing":                          {staleA, 10, state("removing", true), false, "removing"},
		"unlabelled and dead":               {staleA, 10, state("dead", false), false, "unlabelled"},
		"unlabelled and created":            {staleA, 10, state("created", false), false, "unlabelled"},
		"an instance this agent is running": {staleA, 10, state("exited", true), true, "running that instance"},
		"not a uuid":                        {"../../etc", 10, state("exited", true), false, "not a valid instance id"},
		"an injected filter":                {"x\" or label=\"y", 10, state("exited", true), false, "not a valid instance id"},
		"empty id":                          {"", 10, state("exited", true), false, "not a valid instance id"},
		"negative lines":                    {staleA, -1, state("exited", true), false, "negative"},
	}
	for name, c := range cases {
		rt := &fakeRuntimeBackend{listed: []agentruntime.ManagedContainer{c.container}, captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
		conn := New(Config{CentralURL: "ws://unused", BearerToken: "t", NodeName: "n"}, rt, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
		if c.active {
			conn.trackActiveInstance(staleA, 8000, "/models/m", "vllm")
		}
		outcome, detail := conn.removeStale(context.Background(), nil, agentproto.RemoveContainer{RemoveID: "r", InstanceID: c.id, Lines: c.lines})
		if outcome != agentproto.RemoveOutcomeRefused || !strings.Contains(detail, c.mention) {
			t.Errorf("%s: outcome %q detail %q, want refused mentioning %q", name, outcome, detail, c.mention)
		}
		rt.mu.Lock()
		touched := len(rt.haltCalls) + len(rt.removeCalls) + len(rt.stopCalls) + len(rt.captureCalls)
		rt.mu.Unlock()
		if touched != 0 {
			t.Errorf("%s: a refused removal still touched the runtime (halt %v, remove %v, stop %v, capture %v)", name, rt.haltCalls, rt.removeCalls, rt.stopCalls, rt.captureCalls)
		}
	}
}

func TestRemoveContainer_AContainerThatIsNotThereIsGone(t *testing.T) {
	rt := &fakeRuntimeBackend{listed: []agentruntime.ManagedContainer{exitedContainer(staleB, true)}}
	conn := New(Config{CentralURL: "ws://unused", BearerToken: "t", NodeName: "n"}, rt, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	outcome, _ := conn.removeStale(context.Background(), nil, agentproto.RemoveContainer{RemoveID: "r", InstanceID: staleA})
	if outcome != agentproto.RemoveOutcomeGone {
		t.Errorf("outcome = %q, want gone", outcome)
	}
	if len(rt.captureCalls)+len(rt.removeCalls) != 0 {
		t.Error("something was archived or removed for a container that is not there")
	}
}

func TestRemoveContainer_ARuntimeListFailureIsReportedAndTouchesNothing(t *testing.T) {
	rt := &fakeRuntimeBackend{listErr: errors.New("daemon unreachable")}
	conn := New(Config{CentralURL: "ws://unused", BearerToken: "t", NodeName: "n"}, rt, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	outcome, detail := conn.removeStale(context.Background(), nil, agentproto.RemoveContainer{RemoveID: "r", InstanceID: staleA})
	if outcome != agentproto.RemoveOutcomeFailed || !strings.Contains(detail, "daemon unreachable") {
		t.Errorf("outcome %q detail %q", outcome, detail)
	}
}

func TestRemoveContainer_AnUnconfirmedArchiveKeepsTheContainer(t *testing.T) {
	notStored := func(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck {
		return &agentproto.ContainerLogAck{UploadID: final.UploadID, Stored: false, Error: "too large"}
	}
	for name, confirm := range map[string]func(agentproto.ContainerLogChunk) *agentproto.ContainerLogAck{
		"the central app refuses it":    notStored,
		"the central app never answers": nil,
	} {
		rt := &fakeRuntimeBackend{
			listed:        []agentruntime.ManagedContainer{exitedContainer(staleA, true)},
			captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1},
		}
		h := newArchiveHarnessFor(t, rt, removeEnvelope(t, staleA, 100), nil, confirm)
		var res agentproto.RemoveContainerResult
		if err := nextMessage(t, h, agentproto.TypeRemoveContainerResult).DecodePayload(&res); err != nil {
			t.Fatal(err)
		}
		removed := h.removeCount()
		h.stop()
		if res.Outcome != agentproto.RemoveOutcomeKept || removed != 0 {
			t.Errorf("%s: outcome %q, removed %d; want kept and nothing removed", name, res.Outcome, removed)
		}
	}
}

func TestRemoveContainer_ASavedLogButAFailedRemovalIsFailedNotRemoved(t *testing.T) {
	rt := &fakeRuntimeBackend{
		listed:        []agentruntime.ManagedContainer{exitedContainer(staleA, true)},
		captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1},
		removeErr:     errors.New("container is running"),
	}
	h := newArchiveHarnessFor(t, rt, removeEnvelope(t, staleA, 100), nil, confirmStored)
	defer h.stop()
	var res agentproto.RemoveContainerResult
	if err := nextMessage(t, h, agentproto.TypeRemoveContainerResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentproto.RemoveOutcomeFailed || !strings.Contains(res.Detail, "container is running") || !strings.Contains(res.Detail, "log was saved") {
		t.Errorf("result = %+v", res)
	}
}

func TestRemoveContainer_ACaptureThatFindsNothingIsGone(t *testing.T) {
	rt := &fakeRuntimeBackend{
		listed:     []agentruntime.ManagedContainer{exitedContainer(staleA, true)},
		captureErr: agentruntime.ErrNothingToCapture,
	}
	h := newArchiveHarnessFor(t, rt, removeEnvelope(t, staleA, 100), nil, confirmStored)
	defer h.stop()
	var res agentproto.RemoveContainerResult
	if err := nextMessage(t, h, agentproto.TypeRemoveContainerResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentproto.RemoveOutcomeGone {
		t.Errorf("result = %+v, want gone", res)
	}
}
