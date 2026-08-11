// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/runtime/containers"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// fakeRuntimeBackend implements runtimeBackend - unused by dispatch
// today (see runtimeBackend's doc comment), kept here only so New's
// signature has something real to pass in tests.
type fakeRuntimeBackend struct{}

func (fakeRuntimeBackend) StartContainer(context.Context, containers.Spec) (string, error) {
	return "", nil
}
func (fakeRuntimeBackend) StopContainer(context.Context, string) error { return nil }

// testCentralApp is a minimal stand-in for internal/agentconn's real
// handler, just enough to drive Conn through real WebSocket handshakes
// in-process - accepts a connection, reads exactly one hello message,
// and replies according to accept.
type testCentralApp struct {
	accept        bool
	reason        string
	connectCount  atomic.Int32
	receivedHello chan agentproto.Hello
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

	// Accepted: stay open, reading (and discarding) anything further,
	// until the client disconnects or the request context ends.
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
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
	conn := New(cfg, fakeRuntimeBackend{}, testLogger())
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
	conn := New(cfg, fakeRuntimeBackend{}, testLogger())
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
	conn := New(cfg, fakeRuntimeBackend{}, testLogger())
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
