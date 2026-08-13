// SPDX-License-Identifier: AGPL-3.0-or-later

// Package events is an in-process publish/subscribe broker backing
// ARCHITECTURE.md's Server-Sent Events story ("live telemetry and transfer
// progress... internal use only") - not a durable log, unlike
// internal/audit. Its only job is fanning out a signal that something
// changed to whatever browser tabs currently have a GET /events connection
// open (internal/httpapi's SSE handler); it holds no history and survives
// no restart.
package events

import "sync"

// Event is deliberately minimal - just enough for a subscriber to decide
// whether to react, not a full payload. internal/httpapi's SSE handler
// forwards Type as the SSE "event:" field and refetches the current page
// rather than patching a specific row/entity, so carrying node/entity IDs
// here would be unused plumbing - see PLANNING.md's Decisions Log for this
// phase.
type Event struct {
	Type string
}

// subscriberBuffer bounds how many undelivered events a single subscriber
// channel holds before Publish starts dropping for it - see Publish's doc
// comment for why dropping, not blocking, is the right behavior here.
const subscriberBuffer = 16

// Broker fans out Publish calls to every current Subscribe caller. The
// zero value is not usable - construct with NewBroker. Safe for concurrent
// use.
type Broker struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

// NewBroker constructs an empty Broker.
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[chan Event]struct{})}
}

// Subscribe registers a new subscriber and returns its event channel and a
// cancel function that unregisters it and closes the channel. Callers must
// call cancel when done (e.g. on client disconnect) or the subscription
// leaks. Safe to call cancel more than once.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Publish sends evt to every current subscriber. A subscriber whose buffer
// is full is skipped for this event rather than blocking the caller - this
// is invoked from the consolidated agentconn.OnMessageFunc dispatch
// (cmd/sparky-server/main.go), off the tail of the agent's own WebSocket
// read loop, and one slow or abandoned browser tab must never back-pressure
// that, the same "don't block the real work over a side channel" reasoning
// agentconn.Registry.Send's own doc comment already documents for its
// mutex scope.
func (b *Broker) Publish(evt Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
}
