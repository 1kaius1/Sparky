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

	// TypeLoadInstance is sent by the central app to an agent, instructing
	// it to start a container serving a Running instance - see
	// PLANNING.md's Running instances work, internal/lifecycle (the Model
	// Lifecycle Orchestrator, ARCHITECTURE.md Component Breakdown), and
	// agent/connection's dispatch, which handles it.
	TypeLoadInstance MessageType = "load_instance"

	// TypeUnloadInstance is sent by the central app to an agent,
	// instructing it to stop and remove a Running instance's container.
	TypeUnloadInstance MessageType = "unload_instance"

	// TypeInstanceResult is sent by an agent in response to
	// TypeLoadInstance/TypeUnloadInstance, reporting the outcome - the
	// only feedback mechanism the central app has for what actually
	// happened on the node, matching TypeTransferProgress's role for
	// downloads.
	TypeInstanceResult MessageType = "instance_result"

	// TypeTelemetry is sent by an agent on its own poll interval
	// (SPARKY_TELEMETRY_POLL_INTERVAL, docs/AGENT.md Configuration and
	// Service Architecture Notes' Telemetry goroutine), unprompted - the
	// central app never requests a reading.
	TypeTelemetry MessageType = "telemetry"

	// TypeStartEngineTransfer is sent by the central app to an agent,
	// instructing it to download and install a compiled-engine binary
	// release (llama.cpp today) from GitHub Releases - see PLANNING.md's
	// 2026-08-15 Decisions Log entry and agent/enginetransfer, the
	// executor that handles it. Kept separate from TypeStartTransfer
	// (model weights) since the source, verification, and destination
	// shape (a versioned install directory, not a downloaded file tree)
	// all differ.
	TypeStartEngineTransfer MessageType = "start_engine_transfer"

	// TypeEngineTransferProgress is sent by an agent to report an engine
	// provisioning run's progress, the same streamed-not-just-on-completion
	// shape as TypeTransferProgress.
	TypeEngineTransferProgress MessageType = "engine_transfer_progress"
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

// LoadInstance is TypeLoadInstance's payload. Image and Args are already
// fully resolved server-side by internal/engines' adapter registry (see
// engines.Adapter.BuildLaunchSpec). Image is meaningful only to the
// containers runtime backend (a Docker image reference) - the bare-metal
// backend ignores it, resolving a local executable from EngineType
// instead (per-engine-type SPARKY_<ENGINE>_BINARY_PATH configuration on
// the node - see docs/AGENT.md Configuration), since a binary's on-disk
// location is inherently host-specific and not something the central app
// can know. EngineType is a plain string, not internal/db.ProfileEngineType,
// for the same reason as TransferProgress.Status/InstanceResult.Status:
// this package has no dependency on internal/db. There is no model path
// field: only the agent knows its own local model storage layout
// (SPARKY_MODEL_STORAGE_PATH), so it resolves ModelRef to a local path
// itself, the same way its TypeStartTransfer handling already does for
// downloads. RequiresFullGPUResidency tells the agent whether that local
// path should be the model's whole directory (vLLM-style, Transformers
// format) or a single .gguf file within it (llama.cpp-style, partial
// offload). EngineVersion is empty for an unpinned profile - resolve to
// whatever SPARKY_<ENGINE>_BINARY_PATH already points to (today's
// unchanged behavior, via the operator-configured `latest` symlink - see
// docs/AGENT.md Engine binary provisioning). A non-empty value pins the
// launch to a specific version installed under SPARKY_ENGINE_INSTALL_PATH
// instead - resolved agent-side (agent/connection.resolveEngineBinaryPath),
// same "host-local knowledge stays host-local" reasoning as EngineType's
// binary-path resolution above.
type LoadInstance struct {
	InstanceID               string   `json:"instance_id"`
	ModelRef                 string   `json:"model_ref"`
	EngineType               string   `json:"engine_type"`
	EngineVersion            string   `json:"engine_version,omitempty"`
	Image                    string   `json:"image"`
	Args                     []string `json:"args,omitempty"`
	Port                     int      `json:"port"`
	RequiresFullGPUResidency bool     `json:"requires_full_gpu_residency"`
}

// UnloadInstance is TypeUnloadInstance's payload. There is no
// container-ID field - the agent derives the same deterministic container
// name from InstanceID that it used at load time (see
// containers.InstanceContainerName), so the central app never needs to
// track a live container identity of its own.
type UnloadInstance struct {
	InstanceID string `json:"instance_id"`
}

// InstanceStatus* are InstanceResult.Status's possible values - plain
// strings, not internal/db.RunningInstanceStatus, deliberately, for the
// same reason as TransferProgress.Status: this package has no dependency
// on internal/db. Only a subset of db.RunningInstanceStatus's values
// appears here - "starting" and "stopping" are set centrally, before and
// during dispatch, never reported by the agent.
const (
	InstanceStatusRunning = "running"
	InstanceStatusFailed  = "failed"
	InstanceStatusStopped = "stopped"
)

// InstanceResult is TypeInstanceResult's payload. ActualPort is
// meaningful only when Status is InstanceStatusRunning; ErrorMessage only
// when Status is InstanceStatusFailed.
type InstanceResult struct {
	InstanceID   string `json:"instance_id"`
	Status       string `json:"status"`
	ActualPort   int    `json:"actual_port,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// StartEngineTransfer is TypeStartEngineTransfer's payload - identifies the
// db.EngineTransfer row (by ID, not embedded) an agent should act on, the
// engine to provision, and which release to install. EngineType is a plain
// string, not internal/db.ProfileEngineType, for the same reason as
// TransferProgress.Status: this package has no dependency on internal/db.
// There is no arch field - the agent resolves the release asset for its own
// runtime.GOARCH, since only it knows that.
type StartEngineTransfer struct {
	TransferID string `json:"transfer_id"`
	EngineType string `json:"engine_type"`
	Version    string `json:"version"`
}

// EngineTransferProgress is TypeEngineTransferProgress's payload - the same
// shape as TransferProgress plus InstallPath and InstalledSizeBytes, both
// populated only on the terminal "completed" status (mirroring how
// ErrorMessage is populated only on "failed"). InstallPath is the
// node-local absolute path the agent installed this version into, so the
// central app's inventory upsert can record it without reconstructing the
// on-disk convention itself. InstalledSizeBytes is deliberately a separate
// field from BytesTotal, not a reuse of it the way Model transfers reuses
// BytesTotal as installed size (there, downloaded bytes and installed
// bytes are the same thing) - here BytesTotal is the compressed download
// size, while InstalledSizeBytes is the actual on-disk size after
// extraction, and the two differ for an xz-compressed tarball.
type EngineTransferProgress struct {
	TransferID         string `json:"transfer_id"`
	BytesTransferred   int64  `json:"bytes_transferred"`
	BytesTotal         int64  `json:"bytes_total"`
	Status             string `json:"status"`
	ErrorMessage       string `json:"error_message,omitempty"`
	InstallPath        string `json:"install_path,omitempty"`
	InstalledSizeBytes int64  `json:"installed_size_bytes,omitempty"`
}

// Telemetry is TypeTelemetry's payload - a single point-in-time hardware
// reading, per SCHEMA.md Metrics. RecordedAt is set by the agent itself
// (the same trust level already extended to Heartbeat.SentAt), not
// stamped when the central app receives it, so the value reflects when
// the reading was actually taken, not network/processing latency after
// the fact. There is no running_instance_id field - only the central app
// knows which Running instance (if any) is currently associated with a
// node (internal/db.RunningInstance.PrimaryNodeID), so internal/metrics
// resolves that correlation server-side rather than trusting the agent to
// track its own Running instance state, which it doesn't otherwise need
// to.
type Telemetry struct {
	RecordedAt          time.Time `json:"recorded_at"`
	GPUUtilizationPct   float64   `json:"gpu_utilization_pct"`
	GPUMemoryUsedMB     float64   `json:"gpu_memory_used_mb"`
	GPUMemoryTotalMB    float64   `json:"gpu_memory_total_mb"`
	CPUUtilizationPct   float64   `json:"cpu_utilization_pct"`
	SystemMemoryUsedMB  float64   `json:"system_memory_used_mb"`
	SystemMemoryTotalMB float64   `json:"system_memory_total_mb"`
}
