// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runtime defines the shared abstraction agent/connection dispatches
// load_instance/unload_instance commands through, regardless of which
// concrete runtime backend a node is configured for (SCHEMA.md Nodes'
// runtime_backend) - see ARCHITECTURE.md Runtime Backends.
package runtime

import (
	"context"
	"errors"
	"time"
)

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

	// ContainerName is the display name to give the container - containers
	// backend only, ignored by bare-metal. Empty or invalid falls back to
	// the legacy containers.InstanceContainerName. The container is found
	// again by an instance-id label, not by this name.
	ContainerName string

	// ProfileID is the profile this instance is launched from, put on the
	// container as a label so a later launch of the same profile can find
	// the containers this one leaves behind (InstancesForProfile). Optional:
	// empty (an older central app) or not a plain id means no label.
	// Containers backend only.
	ProfileID string

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

	// Stop stops the instance identified by instanceID. An instance that is
	// already gone - its container or process exited, was removed behind
	// Sparky's back, or was lost when the agent restarted - is not an error:
	// there is nothing left to stop, and an operator unloading an instance
	// reported dead must be able to clear it. Any container left behind by an
	// exited instance is still removed.
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
	//
	// An instance that does not exist at all (a container removed by hand,
	// a Docker data wipe, a reimaged node) reports (false, nil), exactly as
	// one that exited does: both are "not running", and the caller treats
	// both as dead. A non-nil error means the backend could not find out
	// (for example the daemon is unreachable) and says nothing about the
	// instance.
	IsRunning(ctx context.Context, instanceID string) (bool, error)

	// Logs returns instanceID's most recent stdout/stderr output, up to
	// tailLines - best-effort diagnostic evidence used only to enrich a
	// failure report (an early crash, a load-time readiness timeout) with
	// real detail a human can act on. Never required for correctness -
	// agent/connection's readiness check still reports failure correctly
	// even when Logs itself errors (e.g. the container was already
	// removed by the time it's called), it just has less to say why.
	Logs(ctx context.Context, instanceID string, tailLines int) (string, error)

	// Halt stops instanceID's engine but leaves what it left behind - the
	// container, or the bare-metal process record and its captured output -
	// in place, so Capture can still read it. Together with Capture and
	// Remove it is Stop taken apart: the central app must be able to store an
	// instance's log before anything that holds it is removed. An instance
	// that is already gone, or already exited, is not an error.
	Halt(ctx context.Context, instanceID string) error

	// Capture reads what instanceID left behind: its last lines of output
	// (lines <= 0 means all that is still held) and, when the backend knows
	// them, its state and exit reason. It does not change the instance. An
	// instance that does not exist reports ErrNothingToCapture.
	Capture(ctx context.Context, instanceID string, lines int) (Capture, error)

	// InstancesForProfile returns the instance ID of every container this
	// backend holds that was launched for profileID, running or not - what
	// replace-on-launch has to clean up before the profile launches again.
	// Only containers labelled at launch can be found; one started before the
	// label existed is not. The bare-metal backend has no such leftovers to
	// find (an exited process is cleared by Remove) and returns none.
	InstancesForProfile(ctx context.Context, profileID string) ([]string, error)

	// Remove deletes whatever Halt left behind. An instance that is already
	// gone is not an error. Call it only after the log has been stored
	// (agent/connection's archive-then-remove rule).
	Remove(ctx context.Context, instanceID string) error
}

// ErrNothingToCapture is returned by Backend.Capture for an instance the
// backend no longer has - a container removed by hand, a reimaged node, a
// bare-metal process lost when the agent restarted. There is nothing to
// archive, which is not a failure.
var ErrNothingToCapture = errors.New("instance has nothing to capture")

// Capture is what Backend.Capture returns: an instance's output and exit
// reason, taken before its container or process record is removed.
type Capture struct {
	// ContainerID and ContainerName identify the container (or, for
	// bare-metal, the process by PID with no name). Informational only.
	ContainerID   string
	ContainerName string

	// State is the container's own state word (created, running, exited,
	// dead, ...), or running or exited for a bare-metal process.
	State string

	// ExitCode is nil while the instance is still running or when the
	// backend cannot tell. A process killed by a signal reports -1.
	ExitCode *int

	// OOMKilled is true when the runtime reports the container was killed
	// for running out of memory. Always false for bare-metal.
	OOMKilled bool

	// StartedAt and FinishedAt are zero when unknown.
	StartedAt  time.Time
	FinishedAt time.Time

	// Log is the captured output, newest last.
	Log string

	// LinesKept is the number of lines in Log.
	LinesKept int

	// Truncated is true when older output was dropped because the backend
	// could not hold or read all of it (a size cap, or the bare-metal
	// buffer being full), not merely because fewer lines were asked for.
	Truncated bool
}
