// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/1kaius1/Sparky/internal/events"
)

// eventKeepaliveInterval bounds how long an idle /events connection goes
// without a write - without a periodic write, an intermediate proxy or
// load balancer can time out and silently drop an otherwise-healthy
// connection. Matches the agent's own heartbeat cadence
// (docs/AGENT.md/agent/connection) rather than inventing a separate
// interval.
const eventKeepaliveInterval = 30 * time.Second

// eventSource is the subset of *events.Broker this package needs, narrow
// enough to fake in tests without a real Broker.
type eventSource interface {
	Subscribe() (<-chan events.Event, func())
}

// handleEvents is GET /events - the Server-Sent Events endpoint
// ARCHITECTURE.md commits to for live telemetry and transfer progress
// ("SSE chosen over WebSocket - updates flow server-to-client only").
// Session-gated like every other Dashboard UI page (RequireSession), no
// RBAC beyond that - the events themselves carry no more than a type
// (internal/events.Event's own doc comment), so there is nothing here a
// Read-only-tier viewer shouldn't see. web/static/js/sse.js is the
// client, listening on this stream to trigger an htmx refetch of whatever
// page is currently visible rather than patching individual DOM nodes -
// see PLANNING.md's Decisions Log for this phase.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Every real net/http ResponseWriter this server ever constructs
		// supports flushing - this only guards against a future response
		// wrapper (e.g. a compressing middleware) that doesn't.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, cancel := a.events.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(eventKeepaliveInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", evt.Type)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
