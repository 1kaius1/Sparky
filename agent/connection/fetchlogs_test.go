// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

func fetchEnvelope(t *testing.T, lines int) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeFetchLogs, "", agentproto.FetchLogs{FetchID: "fetch-0001", InstanceID: "instance-1", Lines: lines})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestFetchLogs_AnswersWithATaggedChunkSeriesAndTouchesNothing(t *testing.T) {
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{ContainerName: "sparky-p-1", State: "running", ExitCode: nil, Log: "one\ntwo\nthree\n", LinesKept: 3}}
	h := newArchiveHarnessFor(t, rt, fetchEnvelope(t, 100), nil, nil)
	defer h.stop()

	waitFor(t, "the reply", func() bool {
		cs := h.gotChunks()
		return len(cs) > 0 && cs[len(cs)-1].Final
	})
	chunks := h.gotChunks()
	for i, c := range chunks {
		if c.FetchID != "fetch-0001" || c.UploadID != "fetch-0001" {
			t.Errorf("chunk %d: fetch id %q upload id %q, want both echoing the request", i, c.FetchID, c.UploadID)
		}
	}
	log, meta := reassemble(t, chunks)
	if log != "one\ntwo\nthree\n" || meta.Reason != "live" || meta.State != "running" || meta.LinesKept != 3 || meta.LinesRequested != 100 || meta.InstanceID != "instance-1" {
		t.Errorf("log %q meta %+v", log, meta)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.haltCalls)+len(rt.removeCalls)+len(rt.stopCalls) != 0 {
		t.Errorf("a live read must not stop or remove anything: halt=%v remove=%v stop=%v", rt.haltCalls, rt.removeCalls, rt.stopCalls)
	}
}

func TestFetchLogs_ClampsTheLineCount(t *testing.T) {
	for in, want := range map[int]int{0: defaultLiveLogLines, -5: defaultLiveLogLines, 100: 100, 99999: maxLiveLogLines} {
		rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
		h := newArchiveHarnessFor(t, rt, fetchEnvelope(t, in), nil, nil)
		waitFor(t, "the capture", func() bool {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return len(rt.captureLines) == 1
		})
		rt.mu.Lock()
		got := rt.captureLines[0]
		rt.mu.Unlock()
		h.stop()
		if got != want {
			t.Errorf("Lines %d: Capture asked for %d, want %d", in, got, want)
		}
	}
}

func TestFetchLogs_AnInstanceWithNothingToReadIsAnsweredWithAnError(t *testing.T) {
	rt := &fakeRuntimeBackend{captureErr: agentruntime.ErrNothingToCapture}
	h := newArchiveHarnessFor(t, rt, fetchEnvelope(t, 100), nil, nil)
	defer h.stop()

	waitFor(t, "the reply", func() bool { return len(h.gotChunks()) > 0 })
	c := h.gotChunks()[0]
	if c.FetchID != "fetch-0001" || !c.Final || c.Error == "" || len(c.Data) != 0 || c.Meta != nil {
		t.Errorf("chunk = %+v, want a single final error chunk", c)
	}
}

// A runtime error is answered promptly, without handing the raw error (which
// can name paths or sockets) to the browser.
func TestFetchLogs_ARuntimeErrorIsAnsweredWithAGenericError(t *testing.T) {
	rt := &fakeRuntimeBackend{captureErr: errors.New("dial unix /run/user/1000/podman/podman.sock: connect: permission denied")}
	h := newArchiveHarnessFor(t, rt, fetchEnvelope(t, 100), nil, nil)
	defer h.stop()

	waitFor(t, "the reply", func() bool { return len(h.gotChunks()) > 0 })
	c := h.gotChunks()[0]
	if c.Error == "" || strings.Contains(c.Error, "podman.sock") || strings.Contains(c.Error, "permission") {
		t.Errorf("error = %q, want a generic message", c.Error)
	}
}

func TestFetchLogs_MalformedPayloadIsIgnored(t *testing.T) {
	rt := &fakeRuntimeBackend{}
	bad := agentproto.Envelope{Type: agentproto.TypeFetchLogs, Payload: []byte(`{"unknown":1}`)}
	h := newArchiveHarnessFor(t, rt, bad, nil, nil)
	defer h.stop()
	time.Sleep(150 * time.Millisecond)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.captureCalls) != 0 {
		t.Error("a malformed fetch_logs reached the runtime")
	}
}
