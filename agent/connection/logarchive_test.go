// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/netinfo"
	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// archiveHarness runs a Conn against a test central app and records every
// log chunk it sends. confirm decides what the central app answers once the
// final chunk arrives: nil means "never answer".
type archiveHarness struct {
	rt     *fakeRuntimeBackend
	conn   *Conn
	app    *testCentralApp
	msgs   chan agentproto.Envelope
	mu     sync.Mutex
	chunks []agentproto.ContainerLogChunk
	stop   func()
}

func newArchiveHarness(t *testing.T, rt *fakeRuntimeBackend, confirm func(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck) *archiveHarness {
	t.Helper()
	unload, err := agentproto.NewEnvelope(agentproto.TypeUnloadInstance, "", agentproto.UnloadInstance{InstanceID: "instance-1"})
	if err != nil {
		t.Fatal(err)
	}
	return newArchiveHarnessFor(t, rt, unload, nil, confirm)
}

// newArchiveHarnessFor is newArchiveHarness for any first message the central
// app pushes after the handshake; cfg, if set, adjusts the agent's Config.
func newArchiveHarnessFor(t *testing.T, rt *fakeRuntimeBackend, first agentproto.Envelope, cfg func(*Config), confirm func(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck) *archiveHarness {
	t.Helper()
	h := &archiveHarness{rt: rt}
	h.app = newTestCentralApp(true, "")
	h.app.sendAfterAccept = &first
	h.app.receivedMsgs = make(chan agentproto.Envelope, 100)
	h.msgs = h.app.receivedMsgs
	h.app.reply = func(env agentproto.Envelope) *agentproto.Envelope {
		if env.Type != agentproto.TypeContainerLogChunk {
			return nil
		}
		var chunk agentproto.ContainerLogChunk
		if err := env.DecodePayload(&chunk); err != nil {
			t.Errorf("decode chunk: %v", err)
			return nil
		}
		h.mu.Lock()
		h.chunks = append(h.chunks, chunk)
		h.mu.Unlock()
		if !chunk.Final || confirm == nil {
			return nil
		}
		ack := confirm(chunk)
		if ack == nil {
			return nil
		}
		out, _ := agentproto.NewEnvelope(agentproto.TypeContainerLogAck, "", *ack)
		return &out
	}
	srv := httptest.NewServer(h.app)
	conf := Config{CentralURL: wsURL(srv), BearerToken: "t", NodeName: "n", ModelStoragePath: "/models"}
	if cfg != nil {
		cfg(&conf)
	}
	h.conn = New(conf, rt, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	h.conn.listInterfaces = func() ([]netinfo.Interface, error) { return nil, nil }
	h.conn.archiveAckTimeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { h.conn.Run(ctx); close(done) }()
	h.stop = func() { cancel(); <-done; srv.Close() }
	return h
}

func (h *archiveHarness) gotChunks() []agentproto.ContainerLogChunk {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]agentproto.ContainerLogChunk(nil), h.chunks...)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *archiveHarness) removeCount() int {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()
	return len(h.rt.removeCalls)
}

func confirmStored(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck {
	return &agentproto.ContainerLogAck{UploadID: final.UploadID, Stored: true}
}

func reassemble(t *testing.T, chunks []agentproto.ContainerLogChunk) (log string, meta *agentproto.ContainerLogMeta) {
	t.Helper()
	var gz []byte
	for i, c := range chunks {
		if c.Seq != i {
			t.Fatalf("chunk %d has Seq %d", i, c.Seq)
		}
		if (c.Meta != nil) != (i == 0) {
			t.Fatalf("chunk %d: Meta set = %v, want only on the first chunk", i, c.Meta != nil)
		}
		if c.Final != (i == len(chunks)-1) {
			t.Fatalf("chunk %d: Final = %v", i, c.Final)
		}
		gz = append(gz, c.Data...)
	}
	last := chunks[len(chunks)-1]
	sum := sha256.Sum256(gz)
	if last.SHA256 != hex.EncodeToString(sum[:]) || last.TotalBytes != int64(len(gz)) {
		t.Fatalf("final chunk says %d bytes sha %s, reassembled %d bytes sha %x", last.TotalBytes, last.SHA256, len(gz), sum)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(raw), chunks[0].Meta
}

func TestUnload_ArchivesLogThenRemovesOnlyAfterConfirmation(t *testing.T) {
	code := 137
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{
		ContainerID: "abc123", ContainerName: "sparky-p-20261009-010203", State: "exited", ExitCode: &code, OOMKilled: true,
		StartedAt: time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC), Log: "line one\nline two\n", LinesKept: 2,
	}}
	h := newArchiveHarness(t, rt, confirmStored)
	defer h.stop()

	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })

	rt.mu.Lock()
	halts, stops, captures := rt.haltCalls, rt.stopCalls, rt.captureCalls
	rt.mu.Unlock()
	if len(halts) != 1 || len(stops) != 0 || len(captures) != 1 {
		t.Errorf("halt=%v stop=%v capture=%v, want one halt, no stop, one capture", halts, stops, captures)
	}
	log, meta := reassemble(t, h.gotChunks())
	if log != "line one\nline two\n" {
		t.Errorf("archived log = %q", log)
	}
	if meta.InstanceID != "instance-1" || meta.Reason != "unload" || meta.ContainerName != "sparky-p-20261009-010203" || meta.State != "exited" ||
		meta.ExitCode == nil || *meta.ExitCode != 137 || !meta.OOMKilled || meta.LinesKept != 2 || meta.LinesRequested != archiveLogLines ||
		meta.SizeBytes != int64(len(log)) || meta.StartedAt == nil || meta.FinishedAt != nil {
		t.Errorf("meta = %+v", meta)
	}

	// The engine-stopped report is not held up by the archive.
	var sawStopped bool
	for len(h.msgs) > 0 {
		env := <-h.msgs
		if env.Type == agentproto.TypeInstanceResult {
			var r agentproto.InstanceResult
			_ = env.DecodePayload(&r)
			sawStopped = r.Status == agentproto.InstanceStatusStopped
		}
	}
	if !sawStopped {
		t.Error("no instance_result stopped was sent")
	}
}

func TestUnload_NoConfirmationKeepsTheContainerButStillReportsStopped(t *testing.T) {
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
	h := newArchiveHarness(t, rt, nil) // the central app never answers
	defer h.stop()

	var result agentproto.InstanceResult
	deadline := time.After(2 * time.Second)
wait:
	for {
		select {
		case env := <-h.msgs:
			if env.Type == agentproto.TypeInstanceResult {
				_ = env.DecodePayload(&result)
				break wait
			}
		case <-deadline:
			t.Fatal("no instance_result")
		}
	}
	if result.Status != agentproto.InstanceStatusStopped {
		t.Errorf("status = %q, want stopped even though the archive is unconfirmed", result.Status)
	}
	// Wait past the shortened ack timeout, then confirm nothing was removed.
	time.Sleep(h.conn.archiveAckTimeout + 200*time.Millisecond)
	if n := h.removeCount(); n != 0 {
		t.Errorf("Remove called %d times without a confirmation, want 0", n)
	}
	if len(h.gotChunks()) == 0 {
		t.Error("the log was never uploaded")
	}
}

func TestUnload_RejectedArchiveKeepsTheContainer(t *testing.T) {
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
	h := newArchiveHarness(t, rt, func(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck {
		return &agentproto.ContainerLogAck{UploadID: final.UploadID, Stored: false, Error: "too large"}
	})
	defer h.stop()

	waitFor(t, "the upload", func() bool { return len(h.gotChunks()) > 0 })
	time.Sleep(200 * time.Millisecond)
	if n := h.removeCount(); n != 0 {
		t.Errorf("Remove called %d times after Stored=false, want 0", n)
	}
}

func TestUnload_AckForADifferentUploadDoesNotConfirm(t *testing.T) {
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
	h := newArchiveHarness(t, rt, func(final agentproto.ContainerLogChunk) *agentproto.ContainerLogAck {
		return &agentproto.ContainerLogAck{UploadID: "someone-else", Stored: true}
	})
	defer h.stop()

	waitFor(t, "the upload", func() bool { return len(h.gotChunks()) > 0 })
	time.Sleep(h.conn.archiveAckTimeout + 200*time.Millisecond)
	if n := h.removeCount(); n != 0 {
		t.Errorf("Remove called %d times on an ack for another upload, want 0", n)
	}
}

func TestUnload_LargeLogIsSplitIntoChunksUnderTheMessageLimit(t *testing.T) {
	// Random bytes do not compress, so the gzip stream is about as large as
	// the log and must span several chunks.
	rng := rand.New(rand.NewSource(1))
	var sb strings.Builder
	for sb.Len() < 100_000 {
		fmt.Fprintf(&sb, "%016x%016x\n", rng.Uint64(), rng.Uint64())
	}
	big := sb.String()
	rt := &fakeRuntimeBackend{captureResult: agentruntime.Capture{Log: big, LinesKept: strings.Count(big, "\n")}}
	h := newArchiveHarness(t, rt, confirmStored)
	defer h.stop()

	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })
	chunks := h.gotChunks()
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want several for a 100 KB incompressible log", len(chunks))
	}
	for i, c := range chunks {
		env, _ := agentproto.NewEnvelope(agentproto.TypeContainerLogChunk, "", c)
		if raw := len(env.Payload); raw >= 32768 {
			t.Errorf("chunk %d payload is %d bytes, over the message limit", i, raw)
		}
		if len(c.Data) > agentproto.ContainerLogChunkSize {
			t.Errorf("chunk %d holds %d bytes, over ContainerLogChunkSize", i, len(c.Data))
		}
	}
	if log, _ := reassemble(t, chunks); log != big {
		t.Error("reassembled log differs from the original")
	}
}

func TestUnload_NothingToCaptureClearsWithoutUploading(t *testing.T) {
	rt := &fakeRuntimeBackend{captureErr: agentruntime.ErrNothingToCapture}
	h := newArchiveHarness(t, rt, confirmStored)
	defer h.stop()

	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })
	if n := len(h.gotChunks()); n != 0 {
		t.Errorf("uploaded %d chunks for an instance with nothing to capture", n)
	}
}

func TestUnload_CaptureFailureKeepsTheContainer(t *testing.T) {
	rt := &fakeRuntimeBackend{captureErr: errors.New("daemon unreachable")}
	h := newArchiveHarness(t, rt, confirmStored)
	defer h.stop()

	waitFor(t, "the capture attempt", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.captureCalls) == 1
	})
	time.Sleep(100 * time.Millisecond)
	if n := h.removeCount(); n != 0 {
		t.Errorf("Remove called %d times after a failed capture, want 0", n)
	}
}

func TestUnload_HaltFailureReportsFailedAndArchivesNothing(t *testing.T) {
	rt := &fakeRuntimeBackend{haltErr: errors.New("cannot stop")}
	h := newArchiveHarness(t, rt, confirmStored)
	defer h.stop()

	var result agentproto.InstanceResult
	deadline := time.After(2 * time.Second)
wait:
	for {
		select {
		case env := <-h.msgs:
			if env.Type == agentproto.TypeInstanceResult {
				_ = env.DecodePayload(&result)
				break wait
			}
		case <-deadline:
			t.Fatal("no instance_result")
		}
	}
	if result.Status != agentproto.InstanceStatusFailed || result.ErrorMessage != "cannot stop" {
		t.Errorf("result = %+v, want failed: cannot stop", result)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.captureCalls) != 0 || len(rt.removeCalls) != 0 {
		t.Errorf("capture=%v remove=%v after a failed halt, want neither", rt.captureCalls, rt.removeCalls)
	}
}

func TestBeginArchive_RefusesASecondArchiveOfTheSameInstance(t *testing.T) {
	c := New(Config{}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	if !c.beginArchive("i-1") {
		t.Fatal("first beginArchive refused")
	}
	if c.beginArchive("i-1") {
		t.Error("second beginArchive for the same instance was allowed")
	}
	if !c.beginArchive("i-2") {
		t.Error("a different instance was refused")
	}
	c.endArchive("i-1")
	if !c.beginArchive("i-1") {
		t.Error("beginArchive refused after endArchive")
	}
}

func TestDeliverArchiveAck_IgnoresAnUploadNobodyWaitsFor(t *testing.T) {
	c := New(Config{}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	c.deliverArchiveAck(agentproto.ContainerLogAck{UploadID: "nobody", Stored: true}) // must not panic or block
	ch := c.registerArchiveWait("u-1")
	c.deliverArchiveAck(agentproto.ContainerLogAck{UploadID: "u-1", Stored: true})
	c.deliverArchiveAck(agentproto.ContainerLogAck{UploadID: "u-1", Stored: false}) // a duplicate must not block
	if ack := <-ch; !ack.Stored {
		t.Errorf("first ack = %+v", ack)
	}
}
