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

// OnMessageFunc handles a message this package does not handle internally
// - hello, hello_ack, heartbeat, and error stay internal (see readLoop);
// everything else (e.g. TypeTransferProgress) is forwarded here. nodeID
// identifies which node's connection sent env. This keeps Handler generic
// - it does not need to know anything about transfers or any other
// specific command, matching ARCHITECTURE.md's framing of this package as
// the only component that speaks the agent protocol, not a place for
// feature-specific logic.
type OnMessageFunc func(nodeID string, env agentproto.Envelope)

// Handler is the WebSocket endpoint agents dial into. It implements
// http.Handler so it mounts directly into internal/httpapi's router.
type Handler struct {
	auth      authenticator
	status    statusStore
	registry  *Registry
	logger    *log.Logger
	onMessage OnMessageFunc
}

// NewHandler constructs a Handler. onMessage may be nil - a caller with no
// command types to dispatch yet (as of Model transfers Phase 2, nothing
// wires a real callback in) simply passes nil, and every message this
// package doesn't already handle internally is silently discarded.
func NewHandler(auth authenticator, status statusStore, registry *Registry, logger *log.Logger, onMessage OnMessageFunc) *Handler {
	return &Handler{auth: auth, status: status, registry: registry, logger: logger, onMessage: onMessage}
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

	h.readLoop(r.Context(), node.ID, conn)
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

// readLoop blocks until the connection closes or errors. hello, hello_ack,
// heartbeat, and error message types are consumed here without further
// action - hello/hello_ack are only meaningful during the handshake above,
// heartbeat is a keepalive with nothing to act on yet, and error is a
// peer-reported protocol failure this layer only needs to log. Every other
// message type is handed to onMessage, if set - see OnMessageFunc.
func (h *Handler) readLoop(ctx context.Context, nodeID string, conn *websocket.Conn) {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				h.logger.Printf("agentconn: connection closed: %v", err)
			}
			return
		}

		var env agentproto.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			h.logger.Printf("agentconn: node %s sent an undecodable message: %v", nodeID, err)
			continue
		}

		switch env.Type {
		case agentproto.TypeHello, agentproto.TypeHelloAck, agentproto.TypeHeartbeat:
			// Nothing to do yet - see doc comment above.
		case agentproto.TypeError:
			var errPayload agentproto.ErrorPayload
			if err := env.DecodePayload(&errPayload); err != nil {
				h.logger.Printf("agentconn: node %s sent a malformed error message: %v", nodeID, err)
				continue
			}
			h.logger.Printf("agentconn: node %s reported an error: %s (code %s)", nodeID, errPayload.Message, errPayload.Code)
		default:
			if h.onMessage != nil {
				h.onMessage(nodeID, env)
			}
		}
	}
}
