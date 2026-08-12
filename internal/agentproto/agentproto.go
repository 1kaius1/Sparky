// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentproto is the shared WebSocket/JSON protocol between
// cmd/sparky-server and cmd/sparky-agent - see ARCHITECTURE.md Protocol
// and Request Lifecycle ("Central app to agent"). It holds only message
// types and encode/decode helpers, no networking - the agent-initiated
// connection itself, the bearer-token handshake enforcement, and command
// dispatch are later phases (see PLANNING.md's Agent: Docker/Podman
// runtime backend... phase breakdown, Phases 3-5).
package agentproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// MessageType identifies the shape of an Envelope's Payload.
type MessageType string

const (
	// TypeHello is sent by the agent immediately after dialing, presenting
	// its node identity and bearer token - see ARCHITECTURE.md Protocol.
	TypeHello MessageType = "hello"

	// TypeHelloAck is the central app's response to TypeHello, accepting
	// or rejecting the connection.
	TypeHelloAck MessageType = "hello_ack"

	// TypeHeartbeat is sent periodically in either direction to keep the
	// connection alive and detect a silently-dead socket. Distinct from
	// telemetry readings (docs/AGENT.md Service Architecture Notes'
	// Telemetry goroutine), which carry actual hardware metrics.
	TypeHeartbeat MessageType = "heartbeat"

	// TypeError reports a protocol-level failure - a malformed message or
	// a rejection outside the hello handshake (which uses HelloAck
	// instead, since it needs an Accepted flag, not just a message).
	TypeError MessageType = "error"

	// TypeStartTransfer is sent by the central app to an agent, instructing
	// it to begin downloading (or, once v0.3.0's peer replication exists,
	// receiving) a model - see PLANNING.md's Model transfers Phase 2/3 and
	// agent/transfer, the Transfer Executor that will handle it.
	TypeStartTransfer MessageType = "start_transfer"

	// TypeTransferProgress is sent by an agent to report a transfer's
	// progress, streamed periodically rather than only on completion - see
	// docs/AGENT.md Service Architecture Notes' Transfer goroutines.
	TypeTransferProgress MessageType = "transfer_progress"
)

// Envelope is the outer shape of every message on the connection. RequestID
// correlates a response to its request over the shared, multiplexed
// channel - see ARCHITECTURE.md Protocol. It is set by the sender of the
// original request; a message with no logical request (e.g. an
// agent-initiated heartbeat) may leave it empty.
type Envelope struct {
	Type      MessageType     `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope marshals payload and wraps it in an Envelope of the given
// type and request ID.
func NewEnvelope(msgType MessageType, requestID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", msgType, err)
	}
	return Envelope{Type: msgType, RequestID: requestID, Payload: raw}, nil
}

// DecodePayload unmarshals the envelope's Payload into v, which should be
// a pointer to the type matching e.Type (e.g. *Hello for TypeHello).
// Unknown fields are rejected rather than silently ignored, so decoding
// into a payload type that doesn't match e.Type fails loudly instead of
// leaving v's non-overlapping fields at their zero value.
func (e Envelope) DecodePayload(v any) error {
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	return nil
}

// Hello is TypeHello's payload - the agent's connect-time handshake.
type Hello struct {
	NodeName    string `json:"node_name"`
	BearerToken string `json:"bearer_token"`
}

// HelloAck is TypeHelloAck's payload - the central app's handshake result.
// Reason is set only when Accepted is false (e.g. unknown node name, bad
// token) and is safe to log - it must never echo the token back.
type HelloAck struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// Heartbeat is TypeHeartbeat's payload.
type Heartbeat struct {
	SentAt time.Time `json:"sent_at"`
}

// ErrorPayload is TypeError's payload. Named with the Payload suffix,
// unlike this package's other payload types, so it doesn't read as
// implementing Go's error interface - it doesn't.
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// StartTransfer is TypeStartTransfer's payload - identifies the
// db.ModelTransfer row (by ID, not embedded) an agent should act on, and
// the model to fetch. Everything else about the transfer (destination
// node, source) is already implied by which agent this was sent to and is
// looked up from that row rather than duplicated on the wire.
type StartTransfer struct {
	TransferID string `json:"transfer_id"`
	ModelRef   string `json:"model_ref"`
}

// TransferProgress is TypeTransferProgress's payload. Status is a plain
// string, not internal/db.TransferStatus, deliberately - this package has
// no dependency on internal/db (it is shared by both binaries, and
// cmd/sparky-agent has no database access at all), so it carries the
// enum's string values without importing the type that defines them.
type TransferProgress struct {
	TransferID       string `json:"transfer_id"`
	BytesTransferred int64  `json:"bytes_transferred"`
	BytesTotal       int64  `json:"bytes_total"`
	Status           string `json:"status"`
	ErrorMessage     string `json:"error_message,omitempty"`
}
