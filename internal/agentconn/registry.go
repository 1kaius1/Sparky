// SPDX-License-Identifier: AGPL-3.0-or-later

package agentconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// ErrNodeNotConnected is returned by Send when nodeID has no live
// connection. The caller (Model transfers Phase 3+, and eventually the
// Model Lifecycle Orchestrator) decides how to handle that - queuing,
// failing the operation immediately, and so on - this package makes no
// such decision itself.
var ErrNodeNotConnected = errors.New("node not connected")

// Registry tracks which node currently owns which live connection, and
// lets a caller send to it - the Model Lifecycle Orchestrator, once Model
// profiles and Running instances exist, and Model transfers Phase 3+ in
// the meantime, use this to reach a node without managing WebSocket state
// themselves, per ARCHITECTURE.md's Agent-Communication Layer ("the only
// component that speaks the agent protocol").
type Registry struct {
	mu    sync.Mutex
	conns map[string]*websocket.Conn
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]*websocket.Conn)}
}

// Register associates conn with nodeID, replacing any prior connection
// for that node - a node that reconnects (e.g. after a network blip)
// simply supersedes its old entry.
func (r *Registry) Register(nodeID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[nodeID] = conn
}

// Unregister removes nodeID's entry, if it still points at conn. A
// superseded old connection's own deferred cleanup must not evict the
// newer connection that has since replaced it.
func (r *Registry) Unregister(nodeID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[nodeID] == conn {
		delete(r.conns, nodeID)
	}
}

// Connected reports whether nodeID currently has a live connection.
func (r *Registry) Connected(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.conns[nodeID]
	return ok
}

// Send writes env to nodeID's current connection. Returns
// ErrNodeNotConnected if the node has no live connection - the lookup and
// the write are two separate steps, so the mutex is only held for the
// lookup; a connection is safe for concurrent writes on its own (per
// coder/websocket), and holding the lock across a network write would
// block every other node's Send/Register/Unregister call on it.
func (r *Registry) Send(ctx context.Context, nodeID string, env agentproto.Envelope) error {
	r.mu.Lock()
	conn, ok := r.conns[nodeID]
	r.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope for node %s: %w", nodeID, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("write to node %s: %w", nodeID, err)
	}
	return nil
}
