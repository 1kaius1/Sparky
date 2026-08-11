// SPDX-License-Identifier: AGPL-3.0-or-later

package agentconn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// fakeAuthenticator implements authenticator for tests without a real
// AuthService/database.
type fakeAuthenticator struct {
	node *db.Node
	err  error
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, _, _ string) (*db.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.node, nil
}

type statusCall struct {
	nodeID        string
	status        db.AgentStatus
	bumpHeartbeat bool
}

// fakeStatusStore implements statusStore, recording calls on a buffered
// channel so a test can wait for the async server-side goroutine's
// lifecycle transitions without a sleep-based poll.
type fakeStatusStore struct {
	calls chan statusCall
}

func newFakeStatusStore() *fakeStatusStore {
	return &fakeStatusStore{calls: make(chan statusCall, 10)}
}

func (f *fakeStatusStore) SetAgentStatus(_ context.Context, nodeID string, status db.AgentStatus, bumpHeartbeat bool) error {
	f.calls <- statusCall{nodeID, status, bumpHeartbeat}
	return nil
}

func (f *fakeStatusStore) awaitCall(t *testing.T) statusCall {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a SetAgentStatus call")
		return statusCall{}
	}
}

func testHandler(t *testing.T, auth authenticator) (*Handler, *fakeStatusStore, *Registry) {
	t.Helper()
	status := newFakeStatusStore()
	registry := NewRegistry()
	logger := log.New(io.Discard, "", 0)
	return NewHandler(auth, status, registry, logger), status, registry
}

func dialTestServer(t *testing.T, h *Handler) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func writeHello(t *testing.T, conn *websocket.Conn, requestID, nodeName, token string) {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeHello, requestID, agentproto.Hello{NodeName: nodeName, BearerToken: token})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
}

func readHelloAck(t *testing.T, conn *websocket.Conn) (env agentproto.Envelope, ack agentproto.HelloAck) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("Unmarshal(Envelope) error: %v", err)
	}
	if env.Type != agentproto.TypeHelloAck {
		t.Fatalf("Type = %q, want %q", env.Type, agentproto.TypeHelloAck)
	}
	if err := env.DecodePayload(&ack); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	return env, ack
}

func TestHandler_SuccessfulHandshake_TracksLifecycle(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, registry := testHandler(t, &fakeAuthenticator{node: node})
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	_, ack := readHelloAck(t, conn)
	if !ack.Accepted {
		t.Fatalf("HelloAck.Accepted = false, want true (Reason: %q)", ack.Reason)
	}

	// By the time the success ack is observed, the registry and status
	// writes must already have happened - ServeHTTP orders them that way
	// deliberately (see its doc comment) precisely so this holds without
	// a race.
	onlineCall := status.awaitCall(t)
	if onlineCall != (statusCall{nodeID: "node-1", status: db.AgentStatusOnline, bumpHeartbeat: true}) {
		t.Errorf("first SetAgentStatus call = %+v, want online/bumpHeartbeat for node-1", onlineCall)
	}
	if !registry.Connected("node-1") {
		t.Error("registry.Connected(node-1) = false, want true after a successful handshake")
	}

	conn.Close(websocket.StatusNormalClosure, "")

	offlineCall := status.awaitCall(t)
	if offlineCall != (statusCall{nodeID: "node-1", status: db.AgentStatusOffline, bumpHeartbeat: false}) {
		t.Errorf("second SetAgentStatus call = %+v, want offline for node-1", offlineCall)
	}
}

func TestHandler_AuthRejected_GenericReason(t *testing.T) {
	h, status, registry := testHandler(t, &fakeAuthenticator{err: errors.New("invalid node credentials")})
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "unknown-node", "spk_badtoken")
	_, ack := readHelloAck(t, conn)
	if ack.Accepted {
		t.Fatal("HelloAck.Accepted = true, want false for a rejected authentication")
	}
	if ack.Reason != "invalid credentials" {
		t.Errorf("Reason = %q, want the generic %q (must not leak which part of the credential was wrong)", ack.Reason, "invalid credentials")
	}

	select {
	case c := <-status.calls:
		t.Errorf("SetAgentStatus was called (%+v) despite a rejected handshake", c)
	case <-time.After(200 * time.Millisecond):
	}
	if registry.Connected("node-1") {
		t.Error("registry.Connected(node-1) = true, want false for a rejected handshake")
	}
}

func TestHandler_WrongFirstMessageType_Rejected(t *testing.T) {
	h, _, _ := testHandler(t, &fakeAuthenticator{node: &db.Node{ID: "node-1", Name: "spark-1"}})
	conn := dialTestServer(t, h)

	env, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "req-1", agentproto.Heartbeat{SentAt: time.Now()})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	_, ack := readHelloAck(t, conn)
	if ack.Accepted {
		t.Fatal("HelloAck.Accepted = true, want false when the first message isn't hello")
	}
	if ack.Reason != "expected hello" {
		t.Errorf("Reason = %q, want %q", ack.Reason, "expected hello")
	}
}

func TestHandler_MalformedHelloPayload_Rejected(t *testing.T) {
	h, _, _ := testHandler(t, &fakeAuthenticator{node: &db.Node{ID: "node-1", Name: "spark-1"}})
	conn := dialTestServer(t, h)

	// A hello_ack-shaped payload under a hello envelope: valid JSON, but
	// not decodable as agentproto.Hello (DisallowUnknownFields catches
	// the mismatch) - see agentproto.Envelope.DecodePayload.
	env, err := agentproto.NewEnvelope(agentproto.TypeHello, "req-1", agentproto.HelloAck{Accepted: true})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	_, ack := readHelloAck(t, conn)
	if ack.Accepted {
		t.Fatal("HelloAck.Accepted = true, want false for a malformed hello payload")
	}
	if ack.Reason != "malformed hello" {
		t.Errorf("Reason = %q, want %q", ack.Reason, "malformed hello")
	}
}
