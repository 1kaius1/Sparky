// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentconn is the Agent-Communication Layer - see
// ARCHITECTURE.md Component Breakdown: "The only component that speaks
// the agent protocol. Every other central-app component that needs
// something to happen on hardware goes through this layer rather than
// managing WebSocket connections itself." Phase 4 of the agent runtime/
// WebSocket work (PLANNING.md): accept connections, run the hello/auth
// handshake (internal/agentproto, internal/nodes.AuthService), and track
// agent_status against connection lifecycle. No command dispatch yet -
// see Registry's doc comment.
package agentconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// handshakeTimeout bounds how long a newly accepted connection has to
// send its Hello message. Without this, a connection that completes the
// WebSocket upgrade but never sends anything would hold its goroutine
// open indefinitely.
const handshakeTimeout = 10 * time.Second

// authenticator is the subset of *nodes.AuthService this package needs,
// narrow enough to fake in tests without a real database - same pattern
// used throughout this codebase (e.g. internal/nodes's own nodeStore).
type authenticator interface {
	Authenticate(ctx context.Context, name, token string) (*db.Node, error)
}

// statusStore is the subset of *db.NodeRepository this package needs.
type statusStore interface {
	SetAgentStatus(ctx context.Context, nodeID string, status db.AgentStatus, bumpHeartbeat bool) error
}

// Handler is the WebSocket endpoint agents dial into. It implements
// http.Handler so it mounts directly into internal/httpapi's router.
type Handler struct {
	auth     authenticator
	status   statusStore
	registry *Registry
	logger   *log.Logger
}

// NewHandler constructs a Handler.
func NewHandler(auth authenticator, status statusStore, registry *Registry, logger *log.Logger) *Handler {
	return &Handler{auth: auth, status: status, registry: registry, logger: logger}
}

// ServeHTTP upgrades the request to a WebSocket, runs the hello/auth
// handshake, and then blocks - tracking agent_status and this node's
// entry in Registry - until the connection closes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// nil AcceptOptions: the default origin check passes for any request
	// with no Origin header, which is what a non-browser agent client
	// sends - confirmed against coder/websocket's own
	// authenticateOrigin, not assumed. InsecureSkipVerify is not needed.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept already wrote a response on error.
		return
	}

	node, requestID, err := h.handshake(r.Context(), conn)
	if err != nil {
		h.logger.Printf("agentconn: handshake failed: %v", err)
		conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}

	// Register and mark online before acking success, so the agent never
	// observes acceptance before this layer's own state reflects it.
	h.registry.Register(node.ID, conn)
	if err := h.status.SetAgentStatus(r.Context(), node.ID, db.AgentStatusOnline, true); err != nil {
		h.logger.Printf("agentconn: set agent_status online for node %s: %v", node.ID, err)
	}
	if err := h.sendHelloAck(r.Context(), conn, requestID, true, ""); err != nil {
		h.logger.Printf("agentconn: send hello_ack for node %s: %v", node.ID, err)
	}
	h.logger.Printf("agentconn: node %s (%s) connected", node.Name, node.ID)

	defer func() {
		h.registry.Unregister(node.ID, conn)
		// context.Background(), not r.Context(): the request context is
		// already done by the time we get here (that's why we're here),
		// but this write still needs to go through.
		if err := h.status.SetAgentStatus(context.Background(), node.ID, db.AgentStatusOffline, false); err != nil {
			h.logger.Printf("agentconn: set agent_status offline for node %s: %v", node.ID, err)
		}
		h.logger.Printf("agentconn: node %s (%s) disconnected", node.Name, node.ID)
	}()

	h.readLoop(r.Context(), conn)
}

// handshake reads exactly one message and expects it to be TypeHello,
// replying with a rejection HelloAck if it isn't valid. The success
// HelloAck is deliberately not sent here - see ServeHTTP, which sends it
// only after this node is registered and marked online, so the agent
// never observes acceptance before this layer's own state reflects it.
func (h *Handler) handshake(ctx context.Context, conn *websocket.Conn) (node *db.Node, requestID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read hello: %w", err)
	}

	var env agentproto.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type != agentproto.TypeHello {
		_ = h.sendHelloAck(ctx, conn, env.RequestID, false, "expected hello")
		return nil, "", fmt.Errorf("first message type = %q, want %q", env.Type, agentproto.TypeHello)
	}

	var hello agentproto.Hello
	if err := env.DecodePayload(&hello); err != nil {
		_ = h.sendHelloAck(ctx, conn, env.RequestID, false, "malformed hello")
		return nil, "", fmt.Errorf("decode hello payload: %w", err)
	}

	node, err = h.auth.Authenticate(ctx, hello.NodeName, hello.BearerToken)
	if err != nil {
		// Same generic reason regardless of what actually failed - see
		// nodes.ErrInvalidCredentials's doc comment: this must not let a
		// caller distinguish an unknown node name from a wrong token.
		_ = h.sendHelloAck(ctx, conn, env.RequestID, false, "invalid credentials")
		return nil, "", fmt.Errorf("authenticate node %q: %w", hello.NodeName, err)
	}

	return node, env.RequestID, nil
}

func (h *Handler) sendHelloAck(ctx context.Context, conn *websocket.Conn, requestID string, accepted bool, reason string) error {
	env, err := agentproto.NewEnvelope(agentproto.TypeHelloAck, requestID, agentproto.HelloAck{Accepted: accepted, Reason: reason})
	if err != nil {
		return fmt.Errorf("build hello_ack: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal hello_ack: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

// readLoop blocks until the connection closes or errors. Dispatching a
// received message to a runtime backend is Phase 5 and later work
// (PLANNING.md) - no real command payloads exist yet, so there is
// nothing to do with a message here beyond noticing the connection is
// still alive.
func (h *Handler) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				h.logger.Printf("agentconn: connection closed: %v", err)
			}
			return
		}
	}
}
