// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

func newTestConnForReadiness(cfg Config, rt *fakeRuntimeBackend) *Conn {
	return New(cfg, rt, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
}

func TestWaitForReady_Success_OnceCompletionProbeSucceeds(t *testing.T) {
	_, port := newFakeEngineServer(t)
	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: fastReadinessTimeout()}, rt)

	if err := conn.waitForReady(context.Background(), "instance-1", "vllm", port, "/models/test-org/test-model"); err != nil {
		t.Fatalf("waitForReady() error: %v", err)
	}
}

// newFakeReasoningEngineServer builds a fake engine whose completions
// response carries empty Content and all its real text in the given
// reasoning field name ("reasoning_content" - llama.cpp's default - or
// "reasoning" - current vLLM/Aphrodite) - reproducing the real response
// shape a reasoning-tuned model returns under a small max_tokens budget
// (see completionProbeResponse's own doc comment).
func newFakeReasoningEngineServer(t *testing.T, reasoningField string) (srv *httptest.Server, port int) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"","` + reasoningField + `":"thinking it over"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake engine server URL: %v", err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse fake engine server port: %v", err)
	}
	return srv, port
}

func TestWaitForReady_Success_EmptyContentButReasoningContentPresent(t *testing.T) {
	// llama.cpp's default field name - confirmed against a real instance
	// serving a reasoning-tuned model that spent its entire probe token
	// budget on the chain-of-thought preamble.
	_, port := newFakeReasoningEngineServer(t, "reasoning_content")
	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: fastReadinessTimeout()}, rt)

	if err := conn.waitForReady(context.Background(), "instance-1", "llamacpp", port, "/models/test-org/test-model"); err != nil {
		t.Fatalf("waitForReady() error: %v, want success from non-empty reasoning_content alone", err)
	}
}

func TestWaitForReady_Success_EmptyContentButReasoningPresent(t *testing.T) {
	// Current vLLM/Aphrodite field name, renamed from reasoning_content.
	_, port := newFakeReasoningEngineServer(t, "reasoning")
	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: fastReadinessTimeout()}, rt)

	if err := conn.waitForReady(context.Background(), "instance-1", "vllm", port, "/models/test-org/test-model"); err != nil {
		t.Fatalf("waitForReady() error: %v, want success from non-empty reasoning alone", err)
	}
}

func TestWaitForReady_AllContentFieldsEmpty_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: fastReadinessTimeout()}, rt)

	err := conn.waitForReady(context.Background(), "instance-1", "vllm", port, "/models/test-org/test-model")
	if err == nil {
		t.Fatal("waitForReady() succeeded despite content and both reasoning fields being empty")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error = %q, want a timeout message", err.Error())
	}
}

func TestWaitForReady_ProcessExitsBeforeReady_FailsFast(t *testing.T) {
	rt := &fakeRuntimeBackend{isRunningResult: false} // never running - exited immediately
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: 5 * time.Minute}, rt)

	start := time.Now()
	err := conn.waitForReady(context.Background(), "instance-1", "vllm", 1, "/models/test-org/test-model")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForReady() succeeded despite the process never running")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to mention the process exiting", err.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("waitForReady() took %s to fail, want it to fail immediately rather than waiting out the 5-minute timeout", elapsed)
	}
}

func TestWaitForReady_UnknownEngineType_FailsImmediately(t *testing.T) {
	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{}, rt)

	err := conn.waitForReady(context.Background(), "instance-1", "some-future-engine", 1, "/models/test-org/test-model")
	if err == nil {
		t.Fatal("waitForReady() succeeded for an engine type with no known readiness probe")
	}
}

func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return port
}

func TestWaitForReady_NeverBecomesReady_TimesOut(t *testing.T) {
	// A real server that always 404s - the API layer never even responds
	// to the models-list check, so this should time out, not hang.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: fastReadinessTimeout()}, rt)

	err := conn.waitForReady(context.Background(), "instance-1", "vllm", port, "/models/test-org/test-model")
	if err == nil {
		t.Fatal("waitForReady() succeeded despite the engine never responding successfully")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error = %q, want a timeout message", err.Error())
	}
}

func TestWaitForReady_ContextCanceled_ReturnsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	rt := &fakeRuntimeBackend{isRunningResult: true}
	conn := newTestConnForReadiness(Config{InstanceStartupTimeout: 5 * time.Minute}, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := conn.waitForReady(ctx, "instance-1", "vllm", port, "/models/test-org/test-model")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForReady() succeeded despite context cancellation")
	}
	if elapsed > 3*time.Second {
		t.Errorf("waitForReady() took %s to return after context cancellation, want it to return promptly", elapsed)
	}
}

func TestReadMetrics_StripsLabelsBeforeMatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# HELP vllm:num_requests_running foo\n" +
			`vllm:num_requests_running{engine="0",model_name="/models/test-org/test-model"} 2` + "\n" +
			`vllm:num_requests_waiting{engine="0",model_name="/models/test-org/test-model"} 0` + "\n" +
			"an_unrelated_metric 99\n"))
	}))
	defer srv.Close()

	client := &http.Client{}
	detail := readMetrics(context.Background(), client, srv.URL)
	if detail["num_requests_running"] != 2 {
		t.Errorf("num_requests_running = %v, want 2", detail["num_requests_running"])
	}
	if detail["num_requests_waiting"] != 0 {
		t.Errorf("num_requests_waiting = %v, want 0", detail["num_requests_waiting"])
	}
	if _, ok := detail["an_unrelated_metric"]; ok {
		t.Error("readMetrics() picked up an unrelated metric it doesn't know about")
	}
}

func TestReadMetrics_UnreachableURL_ReturnsNil(t *testing.T) {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	detail := readMetrics(context.Background(), client, "http://127.0.0.1:1/metrics")
	if detail != nil {
		t.Errorf("readMetrics() = %v, want nil for an unreachable URL", detail)
	}
}

func TestTrackAndUntrackActiveInstance(t *testing.T) {
	rt := &fakeRuntimeBackend{}
	conn := newTestConnForReadiness(Config{}, rt)

	conn.trackActiveInstance("instance-1", 8000, "/models/test-org/test-model", "vllm")
	snapshot := conn.snapshotActiveInstances()
	if len(snapshot) != 1 {
		t.Fatalf("snapshotActiveInstances() = %v, want 1 entry", snapshot)
	}
	got := snapshot["instance-1"]
	if got.Port != 8000 || got.ModelPath != "/models/test-org/test-model" || got.EngineType != "vllm" {
		t.Errorf("tracked instance = %+v, want Port=8000 ModelPath=/models/test-org/test-model EngineType=vllm", got)
	}

	conn.untrackActiveInstance("instance-1")
	if snapshot := conn.snapshotActiveInstances(); len(snapshot) != 0 {
		t.Errorf("snapshotActiveInstances() after untrack = %v, want empty", snapshot)
	}
}

// dialTestAgentConn dials and hand shakes a real client-side
// *websocket.Conn against a testCentralApp-backed server - the minimal
// setup sendInstanceHealth needs (it only writes envelopes onto whatever
// *websocket.Conn it's given, with no handshake awareness of its own),
// without going through Conn.Run's full reconnect loop.
func dialTestAgentConn(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close(websocket.StatusNormalClosure, "test done") })

	helloEnv, err := agentproto.NewEnvelope(agentproto.TypeHello, "", agentproto.Hello{NodeName: "spark-1", BearerToken: "spk_test-token"})
	if err != nil {
		t.Fatalf("build hello: %v", err)
	}
	helloRaw, err := json.Marshal(helloEnv)
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, helloRaw); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	return ws
}

func waitForInstanceHealth(t *testing.T, msgs chan agentproto.Envelope) agentproto.InstanceHealth {
	t.Helper()
	select {
	case env := <-msgs:
		if env.Type != agentproto.TypeInstanceHealth {
			t.Fatalf("received message type = %q, want %q", env.Type, agentproto.TypeInstanceHealth)
		}
		var health agentproto.InstanceHealth
		if err := env.DecodePayload(&health); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
		return health
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance_health")
		return agentproto.InstanceHealth{}
	}
}

func TestSendInstanceHealth_HealthyInstance_ReportsHealthyWithDetail(t *testing.T) {
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[]}`))
		case "/metrics":
			// Matches a real vLLM instance's own /metrics output exactly -
			// these two metrics carry a "{labels}" segment
			// (engine/model_name), confirmed against real hardware; a
			// bare "name value" fixture here would not have caught the
			// real label-stripping bug this test now guards against.
			w.Write([]byte("# HELP vllm:num_requests_running foo\n" +
				`vllm:num_requests_running{engine="0",model_name="/models/test-org/test-model"} 3` + "\n" +
				`vllm:num_requests_waiting{engine="0",model_name="/models/test-org/test-model"} 1` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer engineSrv.Close()
	port := portFromURL(t, engineSrv.URL)

	rt := &fakeRuntimeBackend{}
	conn := newTestConnForReadiness(Config{InstanceHealthCheckInterval: 20 * time.Millisecond}, rt)
	conn.trackActiveInstance("instance-1", port, "/models/test-org/test-model", "vllm")

	central := newTestCentralApp(true, "")
	central.receivedMsgs = make(chan agentproto.Envelope, 10)
	wsSrv := httptest.NewServer(central)
	defer wsSrv.Close()
	ws := dialTestAgentConn(t, wsSrv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go conn.sendInstanceHealth(ctx, ws)

	health := waitForInstanceHealth(t, central.receivedMsgs)
	if health.Status != agentproto.InstanceHealthStatusHealthy {
		t.Errorf("Status = %q, want %q", health.Status, agentproto.InstanceHealthStatusHealthy)
	}
	if health.Detail["num_requests_running"] != 3 || health.Detail["num_requests_waiting"] != 1 {
		t.Errorf("Detail = %v, want num_requests_running=3 num_requests_waiting=1", health.Detail)
	}
}

func TestSendInstanceHealth_UnreachableInstance_ReportsUnhealthy(t *testing.T) {
	// A server started then immediately closed - port is real but nothing
	// is listening on it anymore by the time the health check runs.
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	port := portFromURL(t, engineSrv.URL)
	engineSrv.Close()

	rt := &fakeRuntimeBackend{}
	conn := newTestConnForReadiness(Config{InstanceHealthCheckInterval: 20 * time.Millisecond}, rt)
	conn.trackActiveInstance("instance-1", port, "/models/test-org/test-model", "vllm")

	central := newTestCentralApp(true, "")
	central.receivedMsgs = make(chan agentproto.Envelope, 10)
	wsSrv := httptest.NewServer(central)
	defer wsSrv.Close()
	ws := dialTestAgentConn(t, wsSrv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go conn.sendInstanceHealth(ctx, ws)

	health := waitForInstanceHealth(t, central.receivedMsgs)
	if health.Status != agentproto.InstanceHealthStatusUnhealthy {
		t.Errorf("Status = %q, want %q", health.Status, agentproto.InstanceHealthStatusUnhealthy)
	}
	if health.Detail != nil {
		t.Errorf("Detail = %v, want nil for an unhealthy check", health.Detail)
	}
}

func TestSendInstanceHealth_NonPositiveInterval_Disabled(t *testing.T) {
	rt := &fakeRuntimeBackend{}
	conn := newTestConnForReadiness(Config{}, rt) // InstanceHealthCheckInterval left zero
	conn.trackActiveInstance("instance-1", 1, "/models/test-org/test-model", "vllm")

	central := newTestCentralApp(true, "")
	central.receivedMsgs = make(chan agentproto.Envelope, 10)
	wsSrv := httptest.NewServer(central)
	defer wsSrv.Close()
	ws := dialTestAgentConn(t, wsSrv)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn.sendInstanceHealth(ctx, ws) // returns immediately - not backgrounded

	select {
	case env := <-central.receivedMsgs:
		t.Errorf("received a message %+v despite a non-positive InstanceHealthCheckInterval", env)
	default:
	}
}
