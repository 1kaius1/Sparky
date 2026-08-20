// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runtime defines the shared abstraction agent/connection dispatches
// load_instance/unload_instance commands through, regardless of which
// concrete runtime backend a node is configured for (SCHEMA.md Nodes'
// runtime_backend) - see ARCHITECTURE.md Runtime Backends.
package runtime

import "context"

// Spec describes one engine instance to launch. It is a superset of what
// either backend needs: containers-only fields (Image, Mounts, CDIDevices)
// are ignored by the bare-metal backend, and BinaryPath is ignored by the
// containers backend.
type Spec struct {
	InstanceID string

	// EngineType is the profile's engine type ("vllm" / "llamacpp" - the
	// same string values as internal/db.ProfileEngineType, carried as a
	// plain string for the same reason agentproto's other status/type
	// fields are - see agentproto.LoadInstance's doc comment). Informational
	// for the containers backend; required by the bare-metal backend to
	// resolve BinaryPath.
	EngineType string

	// Image is the container image to run - containers backend only.
	Image string

	// BinaryPath is the resolved local executable to exec directly - the
	// bare-metal backend only. Resolved by the caller (agent/connection)
	// from EngineType via its own per-engine-type configuration, since a
	// binary's on-disk location is inherently host-specific.
	BinaryPath string

	Env []string

	// Args are the full command-line arguments, already including any
	// --model/--port/--host flags the caller resolved - see
	// agent/connection.runLoad.
	Args []string

	// Port, if nonzero, is the port the engine's server listens on.
	Port int

	// Mounts are bind mounts in "hostPath:containerPath[:mode]" form -
	// containers backend only.
	Mounts []string

	// GPUDeviceMechanism selects which Docker Engine API mechanism the
	// containers backend uses to request GPU access - containers backend
	// only, and only meaningful when non-empty (the zero value requests no
	// GPU device at all, e.g. a profile with no GPU requirement, or the
	// bare-metal backend, which has direct GPU access already with no
	// passthrough boundary to cross). Docker and Podman need different
	// mechanisms here, not because the Engine API differs between them
	// (Podman's socket is Docker-Engine-API-compatible), but because only
	// Podman resolves CDI-qualified device names through it - see
	// GPUDeviceMechanismCDI's own doc comment for the empirical finding.
	GPUDeviceMechanism GPUDeviceMechanism

	// CDIDevices are CDI-qualified device names (e.g. "nvidia.com/gpu=all")
	// - only meaningful when GPUDeviceMechanism is GPUDeviceMechanismCDI.
	CDIDevices []string

	// ShmSize is the /dev/shm size, in bytes, to give the container -
	// containers backend only, ignored by bare-metal (which already
	// shares the host's own /dev/shm). Zero means "use the container
	// runtime's own default". Threaded through from
	// engines.LaunchSpec.ShmSizeBytes via agentproto.LoadInstance - see
	// that field's own doc comment for why this exists (vLLM multi-GPU
	// tensor-parallel NCCL communication).
	ShmSize int64

	// IPCMode is the container's IPC namespace mode (e.g. "host") -
	// containers backend only, ignored by bare-metal. Empty means "use
	// the container runtime's own default". Always set alongside ShmSize
	// - see engines.LaunchSpec.IPCMode.
	IPCMode string
}

// GPUDeviceMechanism selects which Docker Engine API mechanism
// agent/runtime/containers.Backend.Start uses to request GPU access.
type GPUDeviceMechanism string

const (
	// GPUDeviceMechanismNvidia requests GPU access via
	// container.HostConfig.DeviceRequests{Driver: "nvidia"} - the same
	// mechanism `docker run --gpus all` uses. Confirmed working against
	// real production Docker/DGX Spark hardware (a real vLLM launch script
	// - see PLANNING.md's 2026-08-17 Decisions Log entry); Spark's own
	// fleet deliberately ships Docker rather than Podman (2026-08-19
	// Decisions Log entry), so this is that fleet's actual mechanism, not
	// a fallback.
	GPUDeviceMechanismNvidia GPUDeviceMechanism = "nvidia"

	// GPUDeviceMechanismCDI requests GPU access via
	// container.HostConfig.DeviceRequests{Driver: "cdi"} - Podman's own
	// canonical mechanism. Verified NOT to trigger CDI resolution against
	// a real local Podman 4.9.3 daemon through this Docker-Engine-API-
	// compatible socket, even though Podman's own CLI resolves the same
	// CDI names correctly (PLANNING.md's 2026-08-10 Decisions Log entry) -
	// kept as Podman's mechanism regardless, since it is the documented,
	// correct API contract and the best available attempt pending
	// verification against the actual target Podman version.
	GPUDeviceMechanismCDI GPUDeviceMechanism = "cdi"
)

// Backend starts and stops engine instances - implemented by
// agent/runtime/containers (Docker/Podman) and agent/runtime/baremetal
// (direct process exec). agent/connection holds exactly one Backend, picked
// once at startup by cmd/sparky-agent based on the node's configured
// runtime_backend, and never branches on which concrete implementation it
// has.
type Backend interface {
	// Start launches spec and returns an implementation-specific
	// identifier (a container ID or a PID) for local/diagnostic use only -
	// Stop and Shutdown never require it back, since both backends derive
	// their own internal identity from Spec.InstanceID.
	Start(ctx context.Context, spec Spec) (string, error)

	// Stop stops the instance identified by instanceID.
	Stop(ctx context.Context, instanceID string) error

	// Shutdown stops every instance this backend is still tracking, called
	// once as the agent process exits. A no-op for the containers backend,
	// which deliberately leaves containers running across an agent
	// restart - see docs/AGENT.md Signal Handling. For the bare-metal
	// backend, this is what keeps an agent exit from orphaning a live
	// child process.
	Shutdown(ctx context.Context) error

	// IsRunning reports whether instanceID is currently running, used by
	// agent/connection's TypeCheckInstance dispatch to answer the central
	// app's reconciliation sweep after a reconnect (PLANNING.md's
	// running_instances staleness fix) - each backend answers from
	// whatever it already tracks for its own operational purposes, never
	// inventing new bookkeeping just to answer this. The containers
	// backend queries the Docker/Podman daemon directly (the durable
	// source of truth, independent of the agent's own process lifetime);
	// the bare-metal backend answers from its own in-memory process map,
	// which is why a genuine agent crash-and-restart correctly reports
	// "not running" for anything it no longer remembers starting.
	IsRunning(ctx context.Context, instanceID string) (bool, error)
}
