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
	return testHandlerWithOnMessage(t, auth, nil)
}

func testHandlerWithOnMessage(t *testing.T, auth authenticator, onMessage OnMessageFunc) (*Handler, *fakeStatusStore, *Registry) {
	t.Helper()
	return testHandlerWithCallbacks(t, auth, onMessage, nil)
}

func testHandlerWithOnConnect(t *testing.T, auth authenticator, onConnect OnConnectFunc) (*Handler, *fakeStatusStore, *Registry) {
	t.Helper()
	return testHandlerWithCallbacks(t, auth, nil, onConnect)
}

func testHandlerWithCallbacks(t *testing.T, auth authenticator, onMessage OnMessageFunc, onConnect OnConnectFunc) (*Handler, *fakeStatusStore, *Registry) {
	t.Helper()
	status := newFakeStatusStore()
	registry := NewRegistry()
	logger := log.New(io.Discard, "", 0)
	return NewHandler(auth, status, registry, logger, onMessage, onConnect), status, registry
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

func writeEnvelope(t *testing.T, conn *websocket.Conn, env agentproto.Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
}

type receivedMessage struct {
	nodeID string
	env    agentproto.Envelope
}

func TestHandler_OnMessage_ForwardsUnknownTypes(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	received := make(chan receivedMessage, 1)
	onMessage := func(nodeID string, env agentproto.Envelope) {
		received <- receivedMessage{nodeID, env}
	}
	h, _, _ := testHandlerWithOnMessage(t, &fakeAuthenticator{node: node}, onMessage)
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	readHelloAck(t, conn)

	progress := agentproto.TransferProgress{TransferID: "xfer-1", BytesTransferred: 100, BytesTotal: 200, Status: "transferring"}
	env, err := agentproto.NewEnvelope(agentproto.TypeTransferProgress, "", progress)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	writeEnvelope(t, conn, env)

	select {
	case got := <-received:
		if got.nodeID != "node-1" {
			t.Errorf("onMessage nodeID = %q, want %q", got.nodeID, "node-1")
		}
		if got.env.Type != agentproto.TypeTransferProgress {
			t.Errorf("Type = %q, want %q", got.env.Type, agentproto.TypeTransferProgress)
		}
		var gotProgress agentproto.TransferProgress
		if err := got.env.DecodePayload(&gotProgress); err != nil {
			t.Fatalf("DecodePayload() error: %v", err)
		}
		if gotProgress != progress {
			t.Errorf("TransferProgress = %+v, want %+v", gotProgress, progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onMessage to be called")
	}
}

func TestHandler_OnMessage_NotCalledForInternalTypes(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	called := make(chan receivedMessage, 1)
	onMessage := func(nodeID string, env agentproto.Envelope) {
		called <- receivedMessage{nodeID, env}
	}
	h, _, _ := testHandlerWithOnMessage(t, &fakeAuthenticator{node: node}, onMessage)
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	readHelloAck(t, conn)

	env, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{SentAt: time.Now()})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	writeEnvelope(t, conn, env)

	select {
	case got := <-called:
		t.Fatalf("onMessage was called (%+v) for a heartbeat, want it to stay internal", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHandler_OnConnect_FiresAfterSuccessfulHandshake(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	connected := make(chan string, 1)
	onConnect := func(_ context.Context, nodeID string) {
		connected <- nodeID
	}
	h, _, _ := testHandlerWithOnConnect(t, &fakeAuthenticator{node: node}, onConnect)
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	readHelloAck(t, conn)

	select {
	case gotNodeID := <-connected:
		if gotNodeID != "node-1" {
			t.Errorf("onConnect nodeID = %q, want %q", gotNodeID, "node-1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onConnect to be called")
	}
}

func TestHandler_OnConnect_NotCalledForRejectedHandshake(t *testing.T) {
	called := make(chan string, 1)
	onConnect := func(_ context.Context, nodeID string) {
		called <- nodeID
	}
	h, _, _ := testHandlerWithOnConnect(t, &fakeAuthenticator{err: errors.New("invalid node credentials")}, onConnect)
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "unknown-node", "spk_badtoken")
	readHelloAck(t, conn)

	select {
	case gotNodeID := <-called:
		t.Fatalf("onConnect was called (%q) despite a rejected handshake", gotNodeID)
	case <-time.After(200 * time.Millisecond):
	}
}

// handshakeAndDrainOnline dials, completes the handshake, and drains the
// initial online SetAgentStatus call every unreachable-detection test below
// doesn't itself care about, leaving status.calls ready to observe only
// what that test triggers.
func handshakeAndDrainOnline(t *testing.T, h *Handler) *websocket.Conn {
	t.Helper()
	conn := dialTestServer(t, h)
	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	readHelloAck(t, conn)
	return conn
}

func TestHandler_Unreachable_DetectedAfterSilence(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, _ := testHandler(t, &fakeAuthenticator{node: node})
	h.unreachableTimeout = 100 * time.Millisecond
	h.unreachableCheckEvery = 20 * time.Millisecond
	handshakeAndDrainOnline(t, h)
	status.awaitCall(t) // the initial online call

	call := status.awaitCall(t)
	if call != (statusCall{nodeID: "node-1", status: db.AgentStatusUnreachable, bumpHeartbeat: false}) {
		t.Errorf("SetAgentStatus call = %+v, want unreachable/no-bump for node-1", call)
	}
}

func TestHandler_Unreachable_NotDetectedWhileHeartbeatsContinue(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, _ := testHandler(t, &fakeAuthenticator{node: node})
	h.unreachableTimeout = 300 * time.Millisecond
	h.unreachableCheckEvery = 50 * time.Millisecond
	conn := handshakeAndDrainOnline(t, h)
	status.awaitCall(t) // the initial online call

	time.Sleep(100 * time.Millisecond)
	heartbeat, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{SentAt: time.Now()})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	writeEnvelope(t, conn, heartbeat)

	// 250ms after the heartbeat above, still under the 300ms timeout it
	// reset - no unreachable call should have fired.
	select {
	case c := <-status.calls:
		t.Errorf("SetAgentStatus was called (%+v) despite a heartbeat resetting the silence window", c)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestHandler_Unreachable_RecoversToOnlineOnNewTraffic(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, _ := testHandler(t, &fakeAuthenticator{node: node})
	h.unreachableTimeout = 100 * time.Millisecond
	h.unreachableCheckEvery = 20 * time.Millisecond
	conn := handshakeAndDrainOnline(t, h)
	status.awaitCall(t) // the initial online call

	unreachableCall := status.awaitCall(t)
	if unreachableCall.status != db.AgentStatusUnreachable {
		t.Fatalf("status = %q, want %q before testing recovery", unreachableCall.status, db.AgentStatusUnreachable)
	}

	heartbeat, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{SentAt: time.Now()})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	writeEnvelope(t, conn, heartbeat)

	recoveredCall := status.awaitCall(t)
	if recoveredCall != (statusCall{nodeID: "node-1", status: db.AgentStatusOnline, bumpHeartbeat: true}) {
		t.Errorf("SetAgentStatus call = %+v, want online/bump for node-1 after new traffic", recoveredCall)
	}
}

func TestHandler_Unreachable_ThenRealDisconnect_EndsOffline(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, _ := testHandler(t, &fakeAuthenticator{node: node})
	h.unreachableTimeout = 100 * time.Millisecond
	h.unreachableCheckEvery = 20 * time.Millisecond
	conn := handshakeAndDrainOnline(t, h)
	status.awaitCall(t) // the initial online call

	unreachableCall := status.awaitCall(t)
	if unreachableCall.status != db.AgentStatusUnreachable {
		t.Fatalf("status = %q, want %q before disconnecting", unreachableCall.status, db.AgentStatusUnreachable)
	}

	conn.Close(websocket.StatusNormalClosure, "")

	// The watchDone join in ServeHTTP's deferred cleanup guarantees
	// watchLiveness has fully stopped before the offline write - so the
	// very next call observed here must be offline, never a late
	// unreachable write racing behind it.
	finalCall := status.awaitCall(t)
	if finalCall != (statusCall{nodeID: "node-1", status: db.AgentStatusOffline, bumpHeartbeat: false}) {
		t.Errorf("final SetAgentStatus call = %+v, want offline for node-1", finalCall)
	}
}

func TestHandler_Unreachable_ReportedOnceNotEveryTick(t *testing.T) {
	node := &db.Node{ID: "node-1", Name: "spark-1"}
	h, status, _ := testHandler(t, &fakeAuthenticator{node: node})
	h.unreachableTimeout = 60 * time.Millisecond
	h.unreachableCheckEvery = 15 * time.Millisecond
	handshakeAndDrainOnline(t, h)
	status.awaitCall(t) // the initial online call

	unreachableCall := status.awaitCall(t)
	if unreachableCall.status != db.AgentStatusUnreachable {
		t.Fatalf("status = %q, want %q", unreachableCall.status, db.AgentStatusUnreachable)
	}

	// Several more check ticks elapse with no recovery - only the one
	// unreachable call above should ever have been written.
	select {
	case c := <-status.calls:
		t.Errorf("a second SetAgentStatus call (%+v) was made for continued silence, want exactly one per silence", c)
	case <-time.After(200 * time.Millisecond):
	}
}
