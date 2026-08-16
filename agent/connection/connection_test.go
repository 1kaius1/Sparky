// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/enginetransfer"
	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/agent/telemetry"
	"github.com/1kaius1/Sparky/agent/transfer"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// fakeRuntimeBackend implements runtimeBackend, recording calls and
// letting a test control each result - used by the load_instance/
// unload_instance dispatch tests below, and as a working no-op backend
// for tests that don't care about it. Imported as agentruntime, not
// runtime, since many test functions below already use "runtime" as a
// local variable name for an instance of this fake.
type fakeRuntimeBackend struct {
	mu            sync.Mutex
	startCalls    []agentruntime.Spec
	startErr      error
	startID       string
	stopCalls     []string
	stopErr       error
	shutdownErr   error
	shutdownCalls int

	isRunningCalls  []string
	isRunningResult bool
	isRunningErr    error

	// block, if non-nil, is closed by a test to let a blocked Start/Stop
	// call proceed; called, if non-nil, is closed the moment that call is
	// entered - lets a test control exactly when a load/unload "finishes"
	// without a sleep-based poll, same pattern as fakeTransferExecutor's
	// block/started.
	block  chan struct{}
	called chan struct{}
}

func (f *fakeRuntimeBackend) Start(_ context.Context, spec agentruntime.Spec) (string, error) {
	f.mu.Lock()
	f.startCalls = append(f.startCalls, spec)
	f.mu.Unlock()
	if f.called != nil {
		close(f.called)
	}
	if f.block != nil {
		<-f.block
	}
	return f.startID, f.startErr
}

func (f *fakeRuntimeBackend) Stop(_ context.Context, instanceID string) error {
	f.mu.Lock()
	f.stopCalls = append(f.stopCalls, instanceID)
	f.mu.Unlock()
	if f.called != nil {
		close(f.called)
	}
	if f.block != nil {
		<-f.block
	}
	return f.stopErr
}

func (f *fakeRuntimeBackend) Shutdown(_ context.Context) error {
	f.mu.Lock()
	f.shutdownCalls++
	f.mu.Unlock()
	return f.shutdownErr
}

func (f *fakeRuntimeBackend) IsRunning(_ context.Context, instanceID string) (bool, error) {
	f.mu.Lock()
	f.isRunningCalls = append(f.isRunningCalls, instanceID)
	f.mu.Unlock()
	if f.called != nil {
		close(f.called)
	}
	return f.isRunningResult, f.isRunningErr
}

// fakeTransferExecutor implements transferExecutor without a real HTTP
// download - it records each call and, unless told to block, immediately
// reports two progress calls (transferring, then completed) through
// whatever ProgressFunc it's given.
type fakeTransferExecutor struct {
	mu    sync.Mutex
	calls []struct{ modelRef, destDir string }

	// block, if non-nil, is closed by a test to let a blocked Download
	// call proceed - lets a test control exactly when a transfer
	// "finishes" without a sleep-based poll.
	block   chan struct{}
	started chan struct{}
}

func (f *fakeTransferExecutor) Download(ctx context.Context, modelRef, destDir string, progress transfer.ProgressFunc) error {
	f.mu.Lock()
	f.calls = append(f.calls, struct{ modelRef, destDir string }{modelRef, destDir})
	f.mu.Unlock()

	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		<-f.block
	}

	progress(0, 100, transfer.StatusTransferring, "")
	progress(100, 100, transfer.StatusCompleted, "")
	return nil
}

func (f *fakeTransferExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeEngineTransferExecutor implements engineTransferExecutor without a
// real HTTP download or `tar` shell-out - it records each call and, unless
// told to block, immediately reports two progress calls (transferring, then
// completed) through whatever ProgressFunc it's given, mirroring
// fakeTransferExecutor.
type fakeEngineTransferExecutor struct {
	mu    sync.Mutex
	calls []struct{ engineType, version, installRoot string }

	// block/started - same purpose as fakeTransferExecutor's identically
	// named fields.
	block   chan struct{}
	started chan struct{}
}

func (f *fakeEngineTransferExecutor) Provision(ctx context.Context, engineType, version, installRoot string, progress enginetransfer.ProgressFunc) (string, int64, error) {
	f.mu.Lock()
	f.calls = append(f.calls, struct{ engineType, version, installRoot string }{engineType, version, installRoot})
	f.mu.Unlock()

	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		<-f.block
	}

	progress(enginetransfer.Progress{BytesTransferred: 0, BytesTotal: 100, Status: enginetransfer.StatusTransferring})
	installPath := installRoot + "/" + engineType + "/" + version
	progress(enginetransfer.Progress{BytesTransferred: 100, BytesTotal: 100, Status: enginetransfer.StatusCompleted, InstallPath: installPath, InstalledSizeBytes: 100})
	return installPath, 100, nil
}

func (f *fakeEngineTransferExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeTelemetryCollector implements telemetryCollector for tests,
// recording calls and letting a test control each result.
type fakeTelemetryCollector struct {
	mu       sync.Mutex
	readErr  error
	reading  telemetry.Reading
	callChan chan struct{} // non-nil: signaled once per Read call
}

func (f *fakeTelemetryCollector) Read(context.Context) (telemetry.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callChan != nil {
		select {
		case f.callChan <- struct{}{}:
		default:
		}
	}
	if f.readErr != nil {
		return telemetry.Reading{}, f.readErr
	}
	return f.reading, nil
}

// testCentralApp is a minimal stand-in for internal/agentconn's real
// handler, just enough to drive Conn through real WebSocket handshakes
// in-process - accepts a connection, reads exactly one hello message,
// and replies according to accept. If sendAfterAccept is set, it's
// written to the agent right after a successful handshake; every message
// read after that is pushed to receivedMessages, if non-nil, instead of
// being silently discarded.
type testCentralApp struct {
	accept          bool
	reason          string
	connectCount    atomic.Int32
	receivedHello   chan agentproto.Hello
	sendAfterAccept *agentproto.Envelope
	receivedMsgs    chan agentproto.Envelope
}

func newTestCentralApp(accept bool, reason string) *testCentralApp {
	return &testCentralApp{accept: accept, reason: reason, receivedHello: make(chan agentproto.Hello, 10)}
}

func (a *testCentralApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.connectCount.Add(1)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var env agentproto.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	var hello agentproto.Hello
	if err := env.DecodePayload(&hello); err == nil {
		a.receivedHello <- hello
	}

	ackEnv, err := agentproto.NewEnvelope(agentproto.TypeHelloAck, env.RequestID, agentproto.HelloAck{Accepted: a.accept, Reason: a.reason})
	if err != nil {
		return
	}
	ackRaw, err := json.Marshal(ackEnv)
	if err != nil {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, ackRaw); err != nil {
		return
	}

	if !a.accept {
		conn.Close(websocket.StatusPolicyViolation, "rejected")
		return
	}

	if a.sendAfterAccept != nil {
		sendRaw, err := json.Marshal(*a.sendAfterAccept)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, sendRaw); err != nil {
			return
		}
	}

	// Accepted: stay open, reading anything further until the client
	// disconnects or the request context ends - forwarded to
	// receivedMsgs if set, otherwise discarded.
	for {
		_, msgRaw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if a.receivedMsgs == nil {
			continue
		}
		var msgEnv agentproto.Envelope
		if err := json.Unmarshal(msgRaw, &msgEnv); err == nil {
			a.receivedMsgs <- msgEnv
		}
	}
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestConn_Run_SuccessfulHandshake_SendsCorrectHello(t *testing.T) {
	app := newTestCentralApp(true, "")
	srv := httptest.NewServer(app)
	defer srv.Close()

	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case hello := <-app.receivedHello:
		if hello.NodeName != "spark-1" {
			t.Errorf("Hello.NodeName = %q, want %q", hello.NodeName, "spark-1")
		}
		if hello.BearerToken != "spk_test-token" {
			t.Errorf("Hello.BearerToken = %q, want %q", hello.BearerToken, "spk_test-token")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for the central app to receive a hello")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

func TestConn_Run_RejectedHandshake_RetriesWithBackoff(t *testing.T) {
	app := newTestCentralApp(false, "invalid credentials")
	srv := httptest.NewServer(app)
	defer srv.Close()

	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_bad-token", NodeName: "spark-1"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn.Run(ctx)

	if got := app.connectCount.Load(); got < 2 {
		t.Errorf("connectCount = %d, want at least 2 (a rejected handshake must retry, not give up)", got)
	}
}

func TestConn_Run_ContextCanceledBeforeDial_ReturnsPromptly(t *testing.T) {
	cfg := Config{CentralURL: "ws://127.0.0.1:1/agent/connect", BearerToken: "spk_x", NodeName: "spark-1"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return promptly for an already-canceled context")
	}
}

func TestConn_Dispatch_StartTransfer_RunsDownloadAndReportsProgress(t *testing.T) {
	startEnv, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "", agentproto.StartTransfer{
		TransferID: "xfer-1",
		ModelRef:   "test-org/test-model",
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &startEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	exec := &fakeTransferExecutor{}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: "/models"}
	conn := New(cfg, &fakeRuntimeBackend{}, exec, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var progressMsgs []agentproto.TransferProgress
	for i := 0; i < 2; i++ {
		select {
		case env := <-app.receivedMsgs:
			if env.Type != agentproto.TypeTransferProgress {
				t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeTransferProgress)
			}
			var p agentproto.TransferProgress
			if err := env.DecodePayload(&p); err != nil {
				t.Fatalf("DecodePayload() error: %v", err)
			}
			progressMsgs = append(progressMsgs, p)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for progress message %d", i+1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if got := exec.callCount(); got != 1 {
		t.Fatalf("Download was called %d times, want 1", got)
	}
	exec.mu.Lock()
	call := exec.calls[0]
	exec.mu.Unlock()
	if call.modelRef != "test-org/test-model" {
		t.Errorf("modelRef = %q, want %q", call.modelRef, "test-org/test-model")
	}
	wantDestDir := "/models/test-org/test-model"
	if call.destDir != wantDestDir {
		t.Errorf("destDir = %q, want %q", call.destDir, wantDestDir)
	}

	if progressMsgs[0].TransferID != "xfer-1" || progressMsgs[0].Status != transfer.StatusTransferring {
		t.Errorf("first progress message = %+v, want TransferID=xfer-1 Status=%q", progressMsgs[0], transfer.StatusTransferring)
	}
	if progressMsgs[1].Status != transfer.StatusCompleted || progressMsgs[1].BytesTransferred != 100 {
		t.Errorf("second progress message = %+v, want Status=%q BytesTransferred=100", progressMsgs[1], transfer.StatusCompleted)
	}
}

func TestConn_Run_WaitsForInFlightTransferOnShutdown(t *testing.T) {
	startEnv, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "", agentproto.StartTransfer{
		TransferID: "xfer-1",
		ModelRef:   "test-org/test-model",
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &startEnv
	srv := httptest.NewServer(app)
	defer srv.Close()

	exec := &fakeTransferExecutor{block: make(chan struct{}), started: make(chan struct{})}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: "/models"}
	conn := New(cfg, &fakeRuntimeBackend{}, exec, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the transfer to start")
	}

	cancel()

	select {
	case <-done:
		t.Fatal("Run() returned before its in-flight transfer finished, want it to wait")
	case <-time.After(200 * time.Millisecond):
	}

	close(exec.block)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return promptly after the in-flight transfer finished")
	}
}

func TestConn_Dispatch_StartEngineTransfer_RunsProvisionAndReportsProgress(t *testing.T) {
	startEnv, err := agentproto.NewEnvelope(agentproto.TypeStartEngineTransfer, "", agentproto.StartEngineTransfer{
		TransferID: "engine-xfer-1",
		EngineType: "llamacpp",
		Version:    "b4610",
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &startEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	exec := &fakeEngineTransferExecutor{}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", EngineInstallPath: "/opt/sparky/serviceloop/engines"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, exec, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var progressMsgs []agentproto.EngineTransferProgress
	for i := 0; i < 2; i++ {
		select {
		case env := <-app.receivedMsgs:
			if env.Type != agentproto.TypeEngineTransferProgress {
				t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeEngineTransferProgress)
			}
			var p agentproto.EngineTransferProgress
			if err := env.DecodePayload(&p); err != nil {
				t.Fatalf("DecodePayload() error: %v", err)
			}
			progressMsgs = append(progressMsgs, p)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for progress message %d", i+1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if got := exec.callCount(); got != 1 {
		t.Fatalf("Provision was called %d times, want 1", got)
	}
	exec.mu.Lock()
	call := exec.calls[0]
	exec.mu.Unlock()
	if call.engineType != "llamacpp" || call.version != "b4610" {
		t.Errorf("call = %+v, want engineType=llamacpp version=b4610", call)
	}
	if call.installRoot != "/opt/sparky/serviceloop/engines" {
		t.Errorf("installRoot = %q, want %q", call.installRoot, "/opt/sparky/serviceloop/engines")
	}

	if progressMsgs[0].TransferID != "engine-xfer-1" || progressMsgs[0].Status != enginetransfer.StatusTransferring {
		t.Errorf("first progress message = %+v, want TransferID=engine-xfer-1 Status=%q", progressMsgs[0], enginetransfer.StatusTransferring)
	}
	if progressMsgs[1].Status != enginetransfer.StatusCompleted || progressMsgs[1].InstallPath == "" {
		t.Errorf("second progress message = %+v, want Status=%q and a non-empty InstallPath", progressMsgs[1], enginetransfer.StatusCompleted)
	}
}

func TestConn_Run_WaitsForInFlightEngineTransferOnShutdown(t *testing.T) {
	startEnv, err := agentproto.NewEnvelope(agentproto.TypeStartEngineTransfer, "", agentproto.StartEngineTransfer{
		TransferID: "engine-xfer-1",
		EngineType: "llamacpp",
		Version:    "b4610",
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &startEnv
	srv := httptest.NewServer(app)
	defer srv.Close()

	exec := &fakeEngineTransferExecutor{block: make(chan struct{}), started: make(chan struct{})}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", EngineInstallPath: "/opt/sparky/serviceloop/engines"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, exec, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the engine transfer to start")
	}

	cancel()

	select {
	case <-done:
		t.Fatal("Run() returned before its in-flight engine transfer finished, want it to wait")
	case <-time.After(200 * time.Millisecond):
	}

	close(exec.block)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return promptly after the in-flight engine transfer finished")
	}
}

func TestConn_ResolveEngineBinaryPath_Unpinned_ReturnsConfiguredPath(t *testing.T) {
	conn := New(Config{
		EngineBinaryPaths: map[string]string{"llamacpp": "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"},
		EngineInstallPath: "/opt/sparky/serviceloop/engines",
	}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())

	got := conn.resolveEngineBinaryPath("llamacpp", "")
	want := "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"
	if got != want {
		t.Errorf("resolveEngineBinaryPath(unpinned) = %q, want %q (unchanged from today's flat lookup)", got, want)
	}
}

func TestConn_ResolveEngineBinaryPath_Pinned_ReconstructsVersionedPath(t *testing.T) {
	conn := New(Config{
		EngineBinaryPaths: map[string]string{"llamacpp": "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"},
		EngineInstallPath: "/opt/sparky/serviceloop/engines",
	}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())

	got := conn.resolveEngineBinaryPath("llamacpp", "b4610")
	want := "/opt/sparky/serviceloop/engines/llamacpp/b4610/llama-server"
	if got != want {
		t.Errorf("resolveEngineBinaryPath(pinned) = %q, want %q", got, want)
	}
}

func TestConn_ResolveEngineBinaryPath_Pinned_ButUnconfigured_ReturnsEmpty(t *testing.T) {
	conn := New(Config{
		EngineBinaryPaths: map[string]string{},
		EngineInstallPath: "/opt/sparky/serviceloop/engines",
	}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())

	// No SPARKY_LLAMACPP_BINARY_PATH configured at all - degrades to the
	// flat lookup's own empty result, letting baremetal.Start's existing
	// "no local binary configured" error fire the same way it always has,
	// rather than guessing a path from nothing.
	got := conn.resolveEngineBinaryPath("llamacpp", "b4610")
	if got != "" {
		t.Errorf("resolveEngineBinaryPath() = %q, want empty when nothing is configured for this engine type", got)
	}
}

func TestConn_ResolveEngineBinaryPath_Pinned_ButNoInstallPath_ReturnsConfiguredPath(t *testing.T) {
	conn := New(Config{
		EngineBinaryPaths: map[string]string{"llamacpp": "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"},
		EngineInstallPath: "",
	}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())

	// EngineInstallPath unset (e.g. a non-bare-metal node, or an agent
	// predating this feature's config) - degrades to the flat lookup
	// rather than constructing a path under an empty root.
	got := conn.resolveEngineBinaryPath("llamacpp", "b4610")
	want := "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"
	if got != want {
		t.Errorf("resolveEngineBinaryPath() = %q, want %q (degrade to configured path)", got, want)
	}
}

func TestConn_Dispatch_LoadInstance_PinnedEngineVersion_ResolvesVersionedBinaryPath(t *testing.T) {
	// RequiresFullGPUResidency true so resolveModelPath just points at
	// ModelStoragePath's ModelRef subdirectory directly, with no .gguf
	// glob involved - irrelevant to what this test actually verifies
	// (engine-version resolution, not model-path resolution).
	loadEnv, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "test-org/test-model",
		EngineType:               "llamacpp",
		EngineVersion:            "b4610",
		Port:                     8001,
		RequiresFullGPUResidency: true,
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &loadEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{startID: "process-1"}
	cfg := Config{
		CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1",
		ModelStoragePath:  t.TempDir(),
		EngineBinaryPaths: map[string]string{"llamacpp": "/opt/sparky/serviceloop/engines/llamacpp/latest/llama-server"},
		EngineInstallPath: "/opt/sparky/serviceloop/engines",
	}

	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case env := <-app.receivedMsgs:
		if env.Type != agentproto.TypeInstanceResult {
			t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeInstanceResult)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if len(runtime.startCalls) != 1 {
		t.Fatalf("Start called %d times, want 1", len(runtime.startCalls))
	}
	wantBinaryPath := "/opt/sparky/serviceloop/engines/llamacpp/b4610/llama-server"
	if runtime.startCalls[0].BinaryPath != wantBinaryPath {
		t.Errorf("BinaryPath = %q, want %q", runtime.startCalls[0].BinaryPath, wantBinaryPath)
	}
}

func TestConn_Dispatch_LoadInstance_FullGPUResidency_StartsContainerAndReportsRunning(t *testing.T) {
	loadEnv, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "test-org/test-model",
		Image:                    "vllm/vllm-openai:latest",
		Args:                     []string{"--tensor-parallel-size", "1"},
		Port:                     8000,
		RequiresFullGPUResidency: true,
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &loadEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{startID: "container-1"}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: "/models"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if env.Type != agentproto.TypeInstanceResult {
			t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeInstanceResult)
		}
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.InstanceID != "instance-1" || result.Status != agentproto.InstanceStatusRunning || result.ActualPort != 8000 {
		t.Errorf("instance_result = %+v, want InstanceID=instance-1 Status=running ActualPort=8000", result)
	}

	if len(runtime.startCalls) != 1 {
		t.Fatalf("Start called %d times, want 1", len(runtime.startCalls))
	}
	spec := runtime.startCalls[0]
	if spec.Image != "vllm/vllm-openai:latest" {
		t.Errorf("Image = %q, want %q", spec.Image, "vllm/vllm-openai:latest")
	}
	if spec.InstanceID != "instance-1" {
		t.Errorf("InstanceID = %q, want %q", spec.InstanceID, "instance-1")
	}
	if spec.Port != 8000 {
		t.Errorf("Port = %d, want 8000", spec.Port)
	}
	// RequiresFullGPUResidency true - --model should point at the whole
	// destDir, not a specific file within it (there is no glob step).
	wantArgs := []string{"--model", "/models/test-org/test-model", "--port", "8000", "--host", "0.0.0.0", "--tensor-parallel-size", "1"}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	wantMounts := []string{"/models:/models:ro"}
	if !reflect.DeepEqual(spec.Mounts, wantMounts) {
		t.Errorf("Mounts = %v, want %v", spec.Mounts, wantMounts)
	}
}

func TestConn_Dispatch_LoadInstance_PartialOffload_NoGGUFFile_ReportsFailed(t *testing.T) {
	// requires_full_gpu_residency false with no matching .gguf file on
	// local storage (nothing was ever downloaded here) - resolveModelPath
	// must fail before ever calling Start.
	loadEnv, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "test-org/test-gguf-model",
		Image:                    "ghcr.io/ggml-org/llama.cpp:server",
		Port:                     8080,
		RequiresFullGPUResidency: false,
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &loadEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: t.TempDir()}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.Status != agentproto.InstanceStatusFailed || result.ErrorMessage == "" {
		t.Errorf("instance_result = %+v, want Status=failed with a non-empty ErrorMessage", result)
	}
	if len(runtime.startCalls) != 0 {
		t.Error("Start was called despite no .gguf file existing to resolve --model to")
	}
}

func TestConn_Dispatch_LoadInstance_StartFails_ReportsFailed(t *testing.T) {
	loadEnv, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "test-org/test-model",
		Image:                    "vllm/vllm-openai:latest",
		Port:                     8000,
		RequiresFullGPUResidency: true,
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &loadEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{startErr: errors.New("image pull failed")}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: "/models"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.Status != agentproto.InstanceStatusFailed || result.ErrorMessage != "image pull failed" {
		t.Errorf("instance_result = %+v, want Status=failed ErrorMessage=%q", result, "image pull failed")
	}
}

func TestConn_Dispatch_UnloadInstance_StopsContainerAndReportsStopped(t *testing.T) {
	unloadEnv, err := agentproto.NewEnvelope(agentproto.TypeUnloadInstance, "", agentproto.UnloadInstance{InstanceID: "instance-1"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &unloadEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.InstanceID != "instance-1" || result.Status != agentproto.InstanceStatusStopped {
		t.Errorf("instance_result = %+v, want InstanceID=instance-1 Status=stopped", result)
	}
	if len(runtime.stopCalls) != 1 || runtime.stopCalls[0] != "instance-1" {
		t.Errorf("stopCalls = %v, want [instance-1]", runtime.stopCalls)
	}
}

func TestConn_Dispatch_CheckInstance_Running_ReportsRunning(t *testing.T) {
	checkEnv, err := agentproto.NewEnvelope(agentproto.TypeCheckInstance, "", agentproto.CheckInstance{InstanceID: "instance-1"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &checkEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{isRunningResult: true}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if env.Type != agentproto.TypeInstanceResult {
			t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeInstanceResult)
		}
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.InstanceID != "instance-1" || result.Status != agentproto.InstanceStatusRunning {
		t.Errorf("instance_result = %+v, want InstanceID=instance-1 Status=running", result)
	}
	if len(runtime.isRunningCalls) != 1 || runtime.isRunningCalls[0] != "instance-1" {
		t.Errorf("isRunningCalls = %v, want [instance-1]", runtime.isRunningCalls)
	}
}

func TestConn_Dispatch_CheckInstance_NotRunning_ReportsStopped(t *testing.T) {
	checkEnv, err := agentproto.NewEnvelope(agentproto.TypeCheckInstance, "", agentproto.CheckInstance{InstanceID: "instance-1"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &checkEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{isRunningResult: false}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var result agentproto.InstanceResult
	select {
	case env := <-app.receivedMsgs:
		if err := env.DecodePayload(&result); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if result.InstanceID != "instance-1" || result.Status != agentproto.InstanceStatusStopped {
		t.Errorf("instance_result = %+v, want InstanceID=instance-1 Status=stopped", result)
	}
}

func TestConn_Dispatch_CheckInstance_IsRunningError_SendsNothing(t *testing.T) {
	checkEnv, err := agentproto.NewEnvelope(agentproto.TypeCheckInstance, "", agentproto.CheckInstance{InstanceID: "instance-1"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &checkEnv
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{isRunningErr: errors.New("docker daemon unreachable")}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case env := <-app.receivedMsgs:
		t.Fatalf("received an unexpected message %+v - an IsRunning error must not report anything back", env)
	case <-time.After(300 * time.Millisecond):
		// Expected - nothing sent, matching runCheckInstance's own
		// "don't guess on a transient check failure" contract.
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if len(runtime.isRunningCalls) != 1 {
		t.Errorf("isRunningCalls = %v, want exactly 1 call despite the error", runtime.isRunningCalls)
	}
}

func TestConn_Run_WaitsForInFlightLoadOnShutdown(t *testing.T) {
	loadEnv, err := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "test-org/test-model",
		Image:                    "vllm/vllm-openai:latest",
		Port:                     8000,
		RequiresFullGPUResidency: true,
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &loadEnv
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{block: make(chan struct{}), called: make(chan struct{})}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1", ModelStoragePath: "/models"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-runtime.called:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Start to be called")
	}

	cancel()

	select {
	case <-done:
		t.Fatal("Run() returned before its in-flight load finished, want it to wait")
	case <-time.After(200 * time.Millisecond):
	}

	close(runtime.block)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return promptly after the in-flight load finished")
	}
}

// TestConn_Run_CallsRuntimeShutdownOnExit confirms Run's exit path stops
// whatever the runtime backend is still tracking (agent/runtime/baremetal's
// whole reason for existing - docs/AGENT.md Signal Handling) after
// in-flight load/unload goroutines have already finished, not before.
func TestConn_Run_CallsRuntimeShutdownOnExit(t *testing.T) {
	app := newTestCentralApp(true, "")
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-app.receivedHello:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for the handshake")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	runtime.mu.Lock()
	shutdownCalls := runtime.shutdownCalls
	runtime.mu.Unlock()
	if shutdownCalls != 1 {
		t.Errorf("Shutdown called %d times, want 1", shutdownCalls)
	}
}

// TestConn_Run_RuntimeShutdownError_Logged confirms a Shutdown error
// doesn't stop Run from returning - shutdownRuntime has nowhere left to
// hand an error back to once the agent is exiting, so it logs and moves
// on rather than blocking process exit.
func TestConn_Run_RuntimeShutdownError_Logged(t *testing.T) {
	app := newTestCentralApp(true, "")
	srv := httptest.NewServer(app)
	defer srv.Close()

	runtime := &fakeRuntimeBackend{shutdownErr: errors.New("stop failed")}
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, runtime, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-app.receivedHello:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for the handshake")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation despite a Shutdown error")
	}
}

func TestConn_SendTelemetry_PushesReading(t *testing.T) {
	app := newTestCentralApp(true, "")
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	reading := telemetry.Reading{
		GPUUtilizationPct: 45, GPUMemoryUsedMB: 8192, GPUMemoryTotalMB: 24576,
		CPUUtilizationPct: 12.5, SystemMemoryUsedMB: 4096, SystemMemoryTotalMB: 16384,
	}
	collector := &fakeTelemetryCollector{reading: reading}
	cfg := Config{
		CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1",
		TelemetryPollInterval: 20 * time.Millisecond,
	}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, collector, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	var got agentproto.Telemetry
	select {
	case env := <-app.receivedMsgs:
		if env.Type != agentproto.TypeTelemetry {
			t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeTelemetry)
		}
		if err := env.DecodePayload(&got); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a telemetry message")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if got.GPUUtilizationPct != reading.GPUUtilizationPct || got.GPUMemoryUsedMB != reading.GPUMemoryUsedMB ||
		got.GPUMemoryTotalMB != reading.GPUMemoryTotalMB || got.CPUUtilizationPct != reading.CPUUtilizationPct ||
		got.SystemMemoryUsedMB != reading.SystemMemoryUsedMB || got.SystemMemoryTotalMB != reading.SystemMemoryTotalMB {
		t.Errorf("telemetry payload = %+v, want values matching %+v", got, reading)
	}
	if got.RecordedAt.IsZero() {
		t.Error("RecordedAt is zero, want the agent's own send-time timestamp")
	}
}

func TestConn_SendTelemetry_ReadFails_NoMessageSent(t *testing.T) {
	app := newTestCentralApp(true, "")
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	collector := &fakeTelemetryCollector{readErr: errors.New("nvidia-smi: executable file not found"), callChan: make(chan struct{}, 10)}
	cfg := Config{
		CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1",
		TelemetryPollInterval: 20 * time.Millisecond,
	}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, collector, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	// Wait for at least one failed Read attempt.
	select {
	case <-collector.callChan:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for a telemetry Read attempt")
	}

	select {
	case env := <-app.receivedMsgs:
		t.Fatalf("received unexpected message %+v, want none after a Read failure", env)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	<-done
}

func TestConn_SendTelemetry_ZeroInterval_DisabledNotPanicked(t *testing.T) {
	app := newTestCentralApp(true, "")
	srv := httptest.NewServer(app)
	defer srv.Close()

	// TelemetryPollInterval left at its zero value.
	cfg := Config{CentralURL: wsURL(srv), BearerToken: "spk_test-token", NodeName: "spark-1"}
	conn := New(cfg, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.minBackoff = 10 * time.Millisecond
	conn.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		conn.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not return - a zero TelemetryPollInterval must not panic or hang")
	}
}

func TestJitter_WithinBounds(t *testing.T) {
	d := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		got := jitter(d)
		if got < d/2 || got >= d {
			t.Fatalf("jitter(%s) = %s, want within [%s, %s)", d, got, d/2, d)
		}
	}
}

func TestJitter_SmallDuration_NoDivideByZero(t *testing.T) {
	for _, d := range []time.Duration{0, 1} {
		if got := jitter(d); got != d {
			t.Errorf("jitter(%s) = %s, want %s unchanged", d, got, d)
		}
	}
}
