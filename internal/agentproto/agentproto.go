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
	// downloads. Also sent in response to TypeCheckInstance below - a
	// third sender of this same message type, not a new one, since
	// "here's this instance's current status" is exactly what it already
	// carries.
	TypeInstanceResult MessageType = "instance_result"

	// TypeCheckInstance is sent by the central app to an agent, asking it
	// to confirm whether a specific instance is actually still running -
	// the running_instances staleness reconciliation sweep triggered on a
	// fresh agent connection (PLANNING.md's Decisions Log), not a normal
	// load/unload command. The agent answers via TypeInstanceResult, the
	// same message type LoadInstance/UnloadInstance already get answered
	// with (InstanceStatusRunning or InstanceStatusStopped) - see
	// agent/connection's dispatch, which handles it via
	// runtime.Backend.IsRunning.
	TypeCheckInstance MessageType = "check_instance"

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

	// TypeInstanceHealth is sent by an agent once per
	// Config.InstanceHealthCheckInterval for every instance it has
	// confirmed running (passed its load-time readiness check - see
	// LoadInstance's own doc comment on why that check exists) - a
	// recurring liveness signal distinct from TypeInstanceResult's
	// one-shot load/unload/check_instance outcome. See SCHEMA.md Running
	// instances' health_status/last_health_check_at, unpopulated by
	// anything before this.
	TypeInstanceHealth MessageType = "instance_health"

	// TypeReportInterfaces is sent by an agent, unprompted, shortly after a
	// successful handshake - and again in answer to TypeRescanInterfaces -
	// naming every network interface it has and each one's reported link
	// speed, for peer-to-peer model transfer's network-path selection (see
	// SCHEMA.md Node network interfaces). Not polled on an interval like
	// telemetry - interfaces rarely change, unlike GPU/CPU utilization.
	TypeReportInterfaces MessageType = "report_interfaces"

	// TypeRescanInterfaces is sent by the central app to an agent, asking
	// it to re-enumerate and report its interfaces again via
	// TypeReportInterfaces - backs the transfer-initiation UI's "Rescan"
	// action, refreshing a node's list on demand rather than waiting for
	// its next reconnect.
	TypeRescanInterfaces MessageType = "rescan_interfaces"

	// TypeAuthorizePeerPull is sent by the central app to the SOURCE
	// agent of a peer-to-peer model transfer, asking it to temporarily
	// trust the DESTINATION agent's SSH public key for exactly this
	// transfer - see ARCHITECTURE.md Security Considerations' scoped
	// exception to the zero-inbound-network-exposure hard constraint. The
	// source resolves ModelRef/Quantization/Format to its own local path
	// itself (the same "only the agent knows its own storage layout"
	// reasoning StartTransfer/LoadInstance already document) - never a
	// wire-supplied path. Answered via TypePeerAuthorizeResult.
	TypeAuthorizePeerPull MessageType = "authorize_peer_pull"

	// TypePeerAuthorizeResult is the source agent's async reply to
	// TypeAuthorizePeerPull.
	TypePeerAuthorizeResult MessageType = "peer_authorize_result"

	// TypeStartPeerTransfer is sent by the central app to the DESTINATION
	// agent of a peer-to-peer model transfer - the pull side - only after
	// the source agent has answered TypeAuthorizePeerPull with Accepted
	// true. Progress/completion are reported back via the existing
	// TypeTransferProgress, exactly as they already are for an internet
	// download - SCHEMA.md's own Model transfers table is explicitly
	// "unified... since both are the same shape of thing," so this
	// protocol doesn't invent a parallel progress message type.
	TypeStartPeerTransfer MessageType = "start_peer_transfer"

	// TypeRevokePeerPull is sent by the central app to the SOURCE agent of
	// a peer-to-peer transfer once it reaches a terminal status
	// (completed/failed/cancelled), asking it to remove that transfer's
	// ephemeral SSH authorization immediately rather than waiting for its
	// own self-expiry. Best-effort - the source agent also self-expires
	// every grant it makes and sweeps clean at startup, so a lost or
	// never-acknowledged revoke is never the only thing standing between a
	// stale grant and cleanup.
	TypeRevokePeerPull MessageType = "revoke_peer_pull"

	// TypeCheckPeerConnectivity is sent by the central app to a
	// prospective DESTINATION agent, backing the transfer-initiation UI's
	// "Check Destination" action - the transfer's own submit control stays
	// disabled until this passes. Deliberately lightweight: a raw TCP dial
	// to the source's chosen interface, no SSH authentication attempted at
	// all - proving the network path is open is what matters here, and
	// this never consumes a real TypeAuthorizePeerPull grant just to check
	// reachability. Answered via TypeConnectivityCheckResult.
	TypeCheckPeerConnectivity MessageType = "check_peer_connectivity"

	// TypeConnectivityCheckResult is a prospective destination agent's
	// async reply to TypeCheckPeerConnectivity.
	TypeConnectivityCheckResult MessageType = "connectivity_check_result"

	// TypeCancelTransfer is sent by the central app to whichever agent is
	// actually doing the work for a transfer - the destination, for
	// either an internet download or a peer pull, since it's the one
	// running the download/rsync process. No reply message of its own -
	// the cancellation's effect is observed through the transfer's own
	// TypeTransferProgress/TypeInstanceResult-style status reporting
	// reaching a terminal state, not a dedicated acknowledgment.
	TypeCancelTransfer MessageType = "cancel_transfer"

	// TypeDeleteModel is sent by the central app to the node whose
	// inventory entry is being removed. Resolves ModelRef/Quantization/
	// Format to its own local path itself, same never-trust-a-wire-
	// supplied-path discipline as TypeAuthorizePeerPull. Answered via
	// TypeDeleteModelResult.
	TypeDeleteModel MessageType = "delete_model"

	// TypeDeleteModelResult is the agent's async reply to TypeDeleteModel.
	TypeDeleteModelResult MessageType = "delete_model_result"
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
// SSHPublicKey/SSHHostPublicKey are both optional (omitempty) so an
// already-upgraded central app can still accept a not-yet-upgraded agent
// that doesn't send them, and so an agent whose `sparky-agent setup` step
// hasn't produced a keypair yet (or whose host has no sshd) can still
// connect and do everything except participate in peer-to-peer model
// transfer - graceful degradation, not a hard requirement. SSHPublicKey is
// this node's own client identity (agent/provision, generated once, the
// private key never leaves the node - see SCHEMA.md Nodes'
// ssh_public_key). SSHHostPublicKey is this node's own system sshd host
// key (OS-managed, not generated by Sparky), reported so a peer pulling a
// model from this node can pin trust for that one connection instead of
// blind trust-on-first-use.
type Hello struct {
	NodeName         string `json:"node_name"`
	BearerToken      string `json:"bearer_token"`
	SSHPublicKey     string `json:"ssh_public_key,omitempty"`
	SSHHostPublicKey string `json:"ssh_host_public_key,omitempty"`
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
// looked up from that row rather than duplicated on the wire. Quantization
// is empty for "download the whole repo" (today's unchanged behavior,
// correct for vLLM/Aphrodite and a single-file GGUF repo) - a non-empty
// value restricts agent/transfer.Executor.Download to just the one
// matching .gguf file.
type StartTransfer struct {
	TransferID   string `json:"transfer_id"`
	ModelRef     string `json:"model_ref"`
	Quantization string `json:"quantization,omitempty"`
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
// binary-path resolution above. Quantization is empty for "not applicable"
// (vLLM/Aphrodite) or "the repo has only one .gguf file" - resolveModelPath
// preserves today's exact glob-and-require-exactly-one behavior in that
// case. A non-empty value resolves directly to the one file matching it,
// no exactly-one requirement needed since the value already disambiguates.
type LoadInstance struct {
	InstanceID               string   `json:"instance_id"`
	ModelRef                 string   `json:"model_ref"`
	EngineType               string   `json:"engine_type"`
	EngineVersion            string   `json:"engine_version,omitempty"`
	Quantization             string   `json:"quantization,omitempty"`
	Image                    string   `json:"image"`
	Args                     []string `json:"args,omitempty"`
	Port                     int      `json:"port"`
	RequiresFullGPUResidency bool     `json:"requires_full_gpu_residency"`
	// ShmSize and IPCMode carry engines.LaunchSpec's own container-runtime-
	// level settings (see its doc comment) across the wire - containers
	// backend only, zero/empty for bare-metal and for engine types that
	// never set them.
	ShmSize int64  `json:"shm_size,omitempty"`
	IPCMode string `json:"ipc_mode,omitempty"`
}

// UnloadInstance is TypeUnloadInstance's payload. There is no
// container-ID field - the agent derives the same deterministic container
// name from InstanceID that it used at load time (see
// containers.InstanceContainerName), so the central app never needs to
// track a live container identity of its own.
type UnloadInstance struct {
	InstanceID string `json:"instance_id"`
}

// CheckInstance is TypeCheckInstance's payload - same shape as
// UnloadInstance, since both only need to name which instance.
type CheckInstance struct {
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

// InstanceHealthStatus* are InstanceHealth.Status's possible values - plain
// strings, not internal/db.InstanceHealthStatus, for the same reason as
// InstanceResult.Status: this package has no dependency on internal/db.
// There is no "unknown" value here - that is running_instances' own
// initial-row default (SCHEMA.md), never something an agent actively
// reports; a real check always resolves to one of these two.
const (
	InstanceHealthStatusHealthy   = "healthy"
	InstanceHealthStatusUnhealthy = "unhealthy"
)

// InstanceHealth is TypeInstanceHealth's payload. CheckedAt is when the
// agent itself performed the check, not when the central app receives the
// message - meaningful given this arrives on a best-effort periodic
// cadence, not synchronously with an operator action. Detail is an
// optional, best-effort read of the engine's own load/utilization
// signal (e.g. vLLM/Aphrodite's Prometheus `/metrics` endpoint's running/
// waiting request counts) - deliberately a flexible map, not fixed fields,
// since different engine types expose genuinely different metric names
// (or none at all, e.g. llama.cpp, unverified against real hardware
// today) - same "opaque, engine-specific shape" reasoning as
// db.Profile.EngineParams. Never populated when Status is
// InstanceHealthStatusUnhealthy - an unreachable engine has nothing to
// read metrics from.
type InstanceHealth struct {
	InstanceID string             `json:"instance_id"`
	Status     string             `json:"status"`
	CheckedAt  time.Time          `json:"checked_at"`
	Detail     map[string]float64 `json:"detail,omitempty"`
}

// GPUTelemetry is a single point-in-time reading from one physical GPU -
// see SCHEMA.md GPU metrics. Index comes from the agent's own
// agent/telemetry.GPUReading.Index (nvidia-smi's reported index field, not
// assumed line order).
type GPUTelemetry struct {
	Index          int     `json:"index"`
	UtilizationPct float64 `json:"utilization_pct"`
	MemoryUsedMB   float64 `json:"memory_used_mb"`
	MemoryTotalMB  float64 `json:"memory_total_mb"`
}

// Telemetry is TypeTelemetry's payload - a single point-in-time hardware
// reading, per SCHEMA.md Metrics and GPU metrics. RecordedAt is set by the
// agent itself (the same trust level already extended to Heartbeat.SentAt),
// not stamped when the central app receives it, so the value reflects when
// the reading was actually taken, not network/processing latency after
// the fact. There is no running_instance_id field - only the central app
// knows which Running instance (if any) is currently associated with a
// node (internal/db.RunningInstance.PrimaryNodeID), so internal/metrics
// resolves that correlation server-side rather than trusting the agent to
// track its own Running instance state, which it doesn't otherwise need
// to. CPU/system-memory stay flat, node-level fields regardless of GPU
// count; GPUs is one entry per physical GPU the agent's nvidia-smi
// invocation reported.
type Telemetry struct {
	RecordedAt          time.Time      `json:"recorded_at"`
	GPUs                []GPUTelemetry `json:"gpus"`
	CPUUtilizationPct   float64        `json:"cpu_utilization_pct"`
	SystemMemoryUsedMB  float64        `json:"system_memory_used_mb"`
	SystemMemoryTotalMB float64        `json:"system_memory_total_mb"`
}

// NetworkInterface is one of an agent's reported interfaces - see
// SCHEMA.md Node network interfaces, which this mirrors field-for-field.
// LinkSpeedMbps is nil (omitted from the wire, not sent as 0) when the
// agent couldn't determine a link speed (a virtual interface, or a driver
// that doesn't expose one) - "unknown," not zero, so "Fastest"
// auto-selection never treats an unmeasured interface as slower than
// everything else by default; it is simply not a candidate.
type NetworkInterface struct {
	Name          string `json:"name"`
	IPAddress     string `json:"ip_address"`
	LinkSpeedMbps *int   `json:"link_speed_mbps,omitempty"`
}

// ReportInterfaces is TypeReportInterfaces' payload.
type ReportInterfaces struct {
	Interfaces []NetworkInterface `json:"interfaces"`
}

// RescanInterfaces is TypeRescanInterfaces' payload - deliberately empty;
// the agent to rescan is already implied by which connection this arrives
// on, and RequestID (Envelope, not this struct) is what lets the agent's
// TypeReportInterfaces reply be correlated back to this specific request.
type RescanInterfaces struct{}

// AuthorizePeerPull is TypeAuthorizePeerPull's payload. ModelRef/
// Quantization/Format let the source agent resolve its own local path
// itself - see TypeAuthorizePeerPull's own doc comment. Format is a plain
// string, not internal/db.ModelFormat, for the same reason as
// TransferProgress.Status: this package has no dependency on internal/db;
// its real values are "safetensors"/"gguf". DestPublicKey/DestIPAddress
// scope the grant to exactly one peer.
type AuthorizePeerPull struct {
	TransferID    string `json:"transfer_id"`
	DestPublicKey string `json:"dest_public_key"`
	DestIPAddress string `json:"dest_ip_address"`
	ModelRef      string `json:"model_ref"`
	Quantization  string `json:"quantization,omitempty"`
	Format        string `json:"format"`
}

// PeerAuthorizeResult is TypeAuthorizePeerPull's response payload, sent by
// the source agent. Accepted false covers a source-side failure to grant
// (the resolved local path doesn't exist, or writing the SSH authorization
// failed) - reported clearly instead of only surfacing later as an opaque
// connection failure on the destination side.
type PeerAuthorizeResult struct {
	TransferID string `json:"transfer_id"`
	Accepted   bool   `json:"accepted"`
	Reason     string `json:"reason,omitempty"`
}

// StartPeerTransfer is TypeStartPeerTransfer's payload - sent only after
// the source has already accepted via PeerAuthorizeResult. SourceHost is
// the IP address of the specific interface selected for this transfer
// (the source node's configured default, or an explicit per-transfer
// override - see SCHEMA.md Nodes' default_transfer_interface and Model
// transfers' source_interface), resolved server-side since only the
// central app has visibility across every node's reported interfaces to
// make that choice. SourceHostPublicKey lets the destination pin trust for
// this one connection instead of blind trust-on-first-use. ModelRef/
// Quantization/Format mirror AuthorizePeerPull's - the destination
// resolves its own local destination path itself, the same pattern.
type StartPeerTransfer struct {
	TransferID          string `json:"transfer_id"`
	SourceNodeID        string `json:"source_node_id"`
	SourceHost          string `json:"source_host"`
	SourceSSHPort       int    `json:"source_ssh_port"`
	SourceHostPublicKey string `json:"source_host_public_key"`
	ModelRef            string `json:"model_ref"`
	Quantization        string `json:"quantization,omitempty"`
	Format              string `json:"format"`
}

// RevokePeerPull is TypeRevokePeerPull's payload.
type RevokePeerPull struct {
	TransferID string `json:"transfer_id"`
}

// CheckPeerConnectivity is TypeCheckPeerConnectivity's payload. CheckID
// (not TransferID - no transfer exists yet at check time) correlates the
// destination's TypeConnectivityCheckResult reply back to this specific
// check, the same role TransferID plays for an in-progress transfer.
type CheckPeerConnectivity struct {
	CheckID       string `json:"check_id"`
	SourceHost    string `json:"source_host"`
	SourceSSHPort int    `json:"source_ssh_port"`
}

// ConnectivityCheckResult is TypeCheckPeerConnectivity's response payload.
// LatencyMs is meaningful only when Reachable is true; Reason only when
// Reachable is false.
type ConnectivityCheckResult struct {
	CheckID   string `json:"check_id"`
	Reachable bool   `json:"reachable"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// CancelTransfer is TypeCancelTransfer's payload - see its own doc comment
// for why there is no dedicated reply message.
type CancelTransfer struct {
	TransferID string `json:"transfer_id"`
}

// DeleteModel is TypeDeleteModel's payload. Format is a plain string, not
// internal/db.ModelFormat, for the same reason as AuthorizePeerPull's own
// Format field.
type DeleteModel struct {
	ModelRef     string `json:"model_ref"`
	Quantization string `json:"quantization,omitempty"`
	Format       string `json:"format"`
}

// DeleteModelResult is TypeDeleteModel's response payload. Success false
// covers, for example, the resolved local path not existing, or the
// filesystem removal itself failing (permissions, a busy mount).
type DeleteModelResult struct {
	ModelRef     string `json:"model_ref"`
	Quantization string `json:"quantization,omitempty"`
	Format       string `json:"format"`
	Success      bool   `json:"success"`
	Reason       string `json:"reason,omitempty"`
}
