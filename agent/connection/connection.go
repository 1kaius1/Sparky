// SPDX-License-Identifier: AGPL-3.0-or-later

// Package connection is sparky-agent's Connection goroutine - see
// docs/AGENT.md Service Architecture Notes: "owns the WebSocket
// lifecycle - dial, handshake with the bearer token, read loop, and
// reconnect-with-backoff on disconnect." Phase 5 of the agent runtime/
// WebSocket work (PLANNING.md): makes the connection to
// internal/agentconn's server-side endpoint real. No real command
// payloads exist yet beyond agentproto's hello/heartbeat/error - Model
// profiles and Running instances are what will eventually define one -
// so dispatch only recognizes what already exists today.
package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/runtime/containers"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// handshakeTimeout bounds both a single dial attempt and waiting for the
// hello_ack reply - matches internal/agentconn's own handshake timeout
// on the server side.
const handshakeTimeout = 10 * time.Second

// heartbeatInterval is how often Conn sends a keepalive heartbeat over an
// established connection - see agentproto.Heartbeat's doc comment: this
// is distinct from telemetry (a separate, not-yet-built goroutine, per
// docs/AGENT.md). No SPARKY_* env var controls this - docs/AGENT.md
// doesn't define one, and there's no case yet for making it
// operator-tunable.
const heartbeatInterval = 30 * time.Second

const (
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 30 * time.Second
)

// runtimeBackend is the subset of *containers.Backend a real command
// dispatch would call into - narrow enough to fake in tests. Nothing
// calls it yet (see Conn.dispatch); this is the "logic layer ahead of
// its eventual caller" pattern already used for RBAC Phase B, the audit
// log, and the node registry - wiring the dependency through now so a
// real command type, once one exists, has somewhere to go.
type runtimeBackend interface {
	StartContainer(ctx context.Context, spec containers.Spec) (string, error)
	StopContainer(ctx context.Context, containerID string) error
}

// Config is what Conn needs to dial and authenticate - the subset of
// agent/config.Config relevant to this package, so it doesn't depend on
// the whole agent's environment surface.
type Config struct {
	CentralURL  string
	BearerToken string
	NodeName    string
}

// Conn owns the agent's single persistent WebSocket connection to the
// central app.
type Conn struct {
	cfg     Config
	runtime runtimeBackend
	logger  *log.Logger

	minBackoff time.Duration
	maxBackoff time.Duration
}

// New constructs a Conn.
func New(cfg Config, runtime runtimeBackend, logger *log.Logger) *Conn {
	return &Conn{
		cfg:        cfg,
		runtime:    runtime,
		logger:     logger,
		minBackoff: defaultMinBackoff,
		maxBackoff: defaultMaxBackoff,
	}
}

// Run blocks until ctx is canceled, maintaining the connection: dial,
// handshake, read loop, and - on any failure or disconnect - reconnect
// after an exponential backoff (reset to the minimum after any
// connection that got far enough to complete its handshake). Every
// outbound message this agent sends goes over the connection this
// method owns, per docs/AGENT.md: "Every other goroutine sends outbound
// messages through this one rather than managing the socket itself" -
// there are no other goroutines yet to do so, but this is the one
// connection point they'll use once there are.
func (c *Conn) Run(ctx context.Context) {
	backoff := c.minBackoff
	for ctx.Err() == nil {
		connected, err := c.runOnce(ctx)
		if err != nil && ctx.Err() == nil {
			c.logger.Printf("agent connection: %v", err)
		}
		if connected {
			backoff = c.minBackoff
		}
		if ctx.Err() != nil {
			return
		}

		wait := jitter(backoff)
		c.logger.Printf("agent connection: reconnecting in %s", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		backoff *= 2
		if backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}

// runOnce dials once, completes the handshake, and then blocks in the
// read loop until the connection ends. connected reports whether the
// handshake was reached and accepted - Run uses it to decide whether to
// reset the backoff, distinguishing "never got connected" from "was
// connected, then dropped."
func (c *Conn) runOnce(ctx context.Context) (connected bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.cfg.CentralURL, nil)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	if err := c.handshake(ctx, conn); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return false, fmt.Errorf("handshake: %w", err)
	}
	c.logger.Printf("agent connection: connected to %s as %q", c.cfg.CentralURL, c.cfg.NodeName)

	readCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go c.sendHeartbeats(readCtx, conn)

	return true, c.readLoop(ctx, conn)
}

// handshake sends TypeHello and waits for an accepting TypeHelloAck -
// see internal/agentconn's server-side handshake, which this mirrors.
func (c *Conn) handshake(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	env, err := agentproto.NewEnvelope(agentproto.TypeHello, "", agentproto.Hello{
		NodeName:    c.cfg.NodeName,
		BearerToken: c.cfg.BearerToken,
	})
	if err != nil {
		return fmt.Errorf("build hello: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	_, respRaw, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	var respEnv agentproto.Envelope
	if err := json.Unmarshal(respRaw, &respEnv); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if respEnv.Type != agentproto.TypeHelloAck {
		return fmt.Errorf("first response type = %q, want %q", respEnv.Type, agentproto.TypeHelloAck)
	}
	var ack agentproto.HelloAck
	if err := respEnv.DecodePayload(&ack); err != nil {
		return fmt.Errorf("decode hello_ack payload: %w", err)
	}
	if !ack.Accepted {
		return fmt.Errorf("central app rejected connection: %s", ack.Reason)
	}
	return nil
}

// sendHeartbeats runs until ctx is canceled, sending a heartbeat every
// heartbeatInterval. A write failure is left for the read loop to
// notice and act on (it's reading the same connection concurrently) -
// this goroutine just stops.
func (c *Conn) sendHeartbeats(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			env, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{SentAt: time.Now()})
			if err != nil {
				c.logger.Printf("agent connection: build heartbeat: %v", err)
				continue
			}
			raw, err := json.Marshal(env)
			if err != nil {
				c.logger.Printf("agent connection: marshal heartbeat: %v", err)
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	}
}

// readLoop blocks reading messages until the connection errors or ctx is
// canceled (canceling ctx aborts an in-progress Read and closes the
// connection - this is how Run's shutdown reaches an active connection;
// see docs/AGENT.md Signal Handling. This is not a graceful WebSocket
// close handshake, just an abrupt close via context cancellation - the
// central app's connection lifecycle handling (internal/agentconn)
// treats any read failure as a disconnect regardless of how it happened,
// so this is sufficient without the extra complexity of racing a close
// handshake against further reads).
func (c *Conn) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env agentproto.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.logger.Printf("agent connection: received malformed message: %v", err)
			continue
		}
		c.dispatch(env)
	}
}

// dispatch routes a received message. c.runtime is already wired in,
// ready for a real command type to call into - see runtimeBackend's doc
// comment for why there isn't one yet.
func (c *Conn) dispatch(env agentproto.Envelope) {
	switch env.Type {
	case agentproto.TypeHeartbeat:
		// A heartbeat from the central app is itself just a keepalive -
		// no action beyond having read it.
	case agentproto.TypeError:
		var errPayload agentproto.ErrorPayload
		if err := env.DecodePayload(&errPayload); err != nil {
			c.logger.Printf("agent connection: received malformed error payload: %v", err)
			return
		}
		c.logger.Printf("agent connection: central app reported an error: %s (code %s)", errPayload.Message, errPayload.Code)
	default:
		c.logger.Printf("agent connection: received unhandled message type %q", env.Type)
	}
}

// jitter returns a duration in [d/2, d) - "equal jitter" - so many
// agents reconnecting after the same event (e.g. a central app restart)
// don't all retry in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)))
}
