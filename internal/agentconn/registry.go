// SPDX-License-Identifier: AGPL-3.0-or-later

package agentconn

import (
	"sync"

	"github.com/coder/websocket"
)

// Registry tracks which node currently owns which live connection. A
// future caller - the Model Lifecycle Orchestrator, once Model profiles
// and Running instances exist - will use this to find the right
// connection to send a command to, without managing WebSocket state
// itself, per ARCHITECTURE.md's Agent-Communication Layer ("the only
// component that speaks the agent protocol"). No send/dispatch API yet -
// this is just the bookkeeping Phase 4 needs; dispatch is a later phase's
// job, once there's a real command to send.
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
