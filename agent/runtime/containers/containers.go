// SPDX-License-Identifier: AGPL-3.0-or-later

// Package containers is the Docker/Podman runtime backend - see
// ARCHITECTURE.md Runtime Backends. One implementation serves both
// runtimes identically, since Podman exposes a Docker-Engine-API-
// compatible socket - see CLAUDE.md Tech Stack.
package containers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// cdiDriver is the device driver name the daemon registers for CDI device
// injection - confirmed against moby/moby's own daemon/cdi.go
// (RegisterCDIDriver registers it under this exact name), not guessed from
// CLI documentation. See the CDI caveat in this package's doc comment on
// Backend for a real gap found testing this against Podman.
const cdiDriver = "cdi"

// nvidiaDriver is the device driver name for Docker's native GPU
// passthrough mechanism - the same one `docker run --gpus all` uses.
// Confirmed against real production Docker/DGX Spark hardware, not
// guessed - see runtime.GPUDeviceMechanismNvidia's own doc comment.
const nvidiaDriver = "nvidia"

// Labels Start puts on every container it creates. labelInstanceID is how
// every later call (Stop, IsRunning, Logs, and the central app's
// check_instance sweep after a reconnect or agent restart) finds the
// container again: the visible container name now carries the profile name
// and start time and so cannot be recomputed from the instance ID, but a
// label can be searched for. Neither this package nor the central app still
// needs to track a live container ID of its own.
const (
	labelInstanceID = "sparky.instance_id"
	labelManaged    = "sparky.managed"
)

// InstanceContainerName returns the legacy deterministic container name
// (sparky-instance-<id>) that older agents gave every container. Start falls
// back to it when the central app supplies no valid name, and the lookup
// falls back to it for a container with no label - one started before the
// label existed and still running across an agent upgrade.
func InstanceContainerName(instanceID string) string {
	return "sparky-instance-" + instanceID
}

// validContainerName is what Docker accepts (Podman is no stricter), with a
// length cap. The name arrives from the central app over the wire, so it is
// checked here rather than trusted to be well-formed.
var validContainerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// containerNameFor picks the name to create the container with: the
// central app's descriptive name when it is present and valid, otherwise
// the legacy deterministic one. Falling back, rather than failing the
// launch over a cosmetic name, keeps an older or buggy server working.
func containerNameFor(spec runtime.Spec) string {
	if validContainerName.MatchString(spec.ContainerName) {
		return spec.ContainerName
	}
	return InstanceContainerName(spec.InstanceID)
}

// dockerClient is the subset of *client.Client this package uses, narrow
// enough to fake in tests - same pattern as internal/auth's ldapConn.
type dockerClient interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	Close() error
}

// Backend manages containers via the Docker Engine API.
//
// GPU passthrough mechanism is chosen by Spec.GPUDeviceMechanism, not
// hardcoded - Docker and Podman need different ones through this same
// Engine API, confirmed empirically rather than assumed:
//
// Docker (GPUDeviceMechanismNvidia, DeviceRequests{Driver: "nvidia"}) -
// confirmed working against real production Docker/DGX Spark hardware (a
// real vLLM launch script - PLANNING.md's 2026-08-17 Decisions Log entry).
// This is the fleet's actual mechanism for every Spark node: Spark ships
// Docker deliberately, never Podman (2026-08-19 Decisions Log entry).
//
// Podman CDI caveat (GPUDeviceMechanismCDI, DeviceRequests{Driver: "cdi"}),
// found by testing against a real local Podman 4.9.3 daemon, not assumed:
// Podman's own CLI resolves CDI-qualified device names correctly
// (confirmed via `podman run --device nvidia.com/gpu=all`, which fails
// with a proper "unresolvable CDI devices" error - no CDI spec exists on a
// GPU-less test host, which is the expected failure). But going through
// the Docker-Engine-API-compatible socket this package actually uses,
// neither of the two mechanisms Docker's own API supports -
// HostConfig.DeviceRequests with Driver "cdi" (confirmed correct against
// moby/moby's daemon source) nor HostConfig.Devices with a CDI name as
// PathOnHost - triggered any CDI resolution: DeviceRequests was silently
// accepted and dropped (no error, but no device either - confirmed via
// `podman inspect`, which does not even have a DeviceRequests field to
// report), and Devices was treated as a literal host path and failed a
// plain stat(). This package implements the documented, correct Docker
// API contract (DeviceRequests) regardless - the best available attempt
// for Podman's compat API - but CDI passthrough through that API on
// Podman needs verification against the actual target Podman version, per
// ARCHITECTURE.md's existing manual test checklist item for this. Non-GPU
// container lifecycle (create, pull-if-missing, start, inspect, stop,
// remove) is fully verified against real Podman and unaffected by this gap.
type Backend struct {
	cli dockerClient
}

// New constructs a Backend. It respects the standard DOCKER_HOST env var if
// set - also honored by Podman's compatible socket - falling back to the
// platform default otherwise.
func New() (*Backend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Backend{cli: cli}, nil
}

// Close releases the underlying client connection.
func (b *Backend) Close() error {
	return b.cli.Close()
}

// Start creates and starts a container from spec, returning its ID. If
// spec.Image is not already present on the node, it is pulled first -
// unlike the `docker run` CLI, the raw Engine API's ContainerCreate does
// not pull a missing image itself, confirmed empirically against a real
// daemon rather than assumed. The container is named by containerNameFor and
// labelled with spec.InstanceID, so Stop and the other calls find it again
// by label without this package tracking any state of its own.
func (b *Backend) Start(ctx context.Context, spec runtime.Spec) (string, error) {
	var hostConfig container.HostConfig
	switch spec.GPUDeviceMechanism {
	case runtime.GPUDeviceMechanismNvidia:
		hostConfig.DeviceRequests = []container.DeviceRequest{
			{
				Driver:       nvidiaDriver,
				Count:        -1, // -1 = all - the same request docker run --gpus all makes
				Capabilities: [][]string{{"gpu"}},
			},
		}
	case runtime.GPUDeviceMechanismCDI:
		if len(spec.CDIDevices) > 0 {
			hostConfig.DeviceRequests = []container.DeviceRequest{
				{
					Driver:    cdiDriver,
					DeviceIDs: spec.CDIDevices,
				},
			}
		}
	}
	if len(spec.Mounts) > 0 {
		hostConfig.Binds = spec.Mounts
	}
	if spec.ShmSize > 0 {
		hostConfig.ShmSize = spec.ShmSize
	}
	if spec.IPCMode != "" {
		hostConfig.IpcMode = container.IpcMode(spec.IPCMode)
	}

	config := &container.Config{
		Image:  spec.Image,
		Env:    spec.Env,
		Cmd:    spec.Args,
		Labels: map[string]string{labelInstanceID: spec.InstanceID, labelManaged: "true"},
	}
	if spec.Port != 0 {
		port, err := network.ParsePort(fmt.Sprintf("%d/tcp", spec.Port))
		if err != nil {
			return "", fmt.Errorf("parse port %d: %w", spec.Port, err)
		}
		config.ExposedPorts = network.PortSet{port: struct{}{}}
		hostConfig.PortBindings = network.PortMap{port: []network.PortBinding{{HostPort: strconv.Itoa(spec.Port)}}}
	}

	createOpts := client.ContainerCreateOptions{
		Name:       containerNameFor(spec),
		Config:     config,
		HostConfig: &hostConfig,
	}

	created, err := b.cli.ContainerCreate(ctx, createOpts)
	if cerrdefs.IsNotFound(err) {
		if pullErr := b.pullImage(ctx, spec.Image); pullErr != nil {
			return "", fmt.Errorf("pull image %s: %w", spec.Image, pullErr)
		}
		created, err = b.cli.ContainerCreate(ctx, createOpts)
	}
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if _, err := b.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start container %s: %w", created.ID, err)
	}

	return created.ID, nil
}

// pullImage pulls image and blocks until the pull completes or fails.
func (b *Backend) pullImage(ctx context.Context, image string) error {
	resp, err := b.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	return resp.Wait(ctx)
}

// resolve finds the container for instanceID: the one carrying its
// instance-id label (started by this version of the agent). A container with
// no such label is one an older agent started under the legacy
// deterministic name, which is returned as the fallback so it stays
// stoppable and checkable across an agent upgrade. Should more than one
// container carry the label, which a single launch cannot produce, the most
// recently created one wins. The result is a container ID, or the legacy
// name, either of which the Engine API accepts interchangeably.
func (b *Backend) resolve(ctx context.Context, instanceID string) (string, error) {
	res, err := b.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // a stopped or exited container still has to be found, to inspect or remove it
		Filters: make(client.Filters).Add("label", labelInstanceID+"="+instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("find container for instance %s: %w", instanceID, err)
	}
	if len(res.Items) == 0 {
		return InstanceContainerName(instanceID), nil
	}
	newest := res.Items[0]
	for _, c := range res.Items[1:] {
		if c.Created > newest.Created {
			newest = c
		}
	}
	return newest.ID, nil
}

// Stop stops and removes the container for instanceID, found by resolve - so
// no state needs to be tracked between Start and Stop.
func (b *Backend) Stop(ctx context.Context, instanceID string) error {
	ref, err := b.resolve(ctx, instanceID)
	if err != nil {
		return err
	}
	if _, err := b.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("stop container %s: %w", ref, err)
	}
	if _, err := b.cli.ContainerRemove(ctx, ref, client.ContainerRemoveOptions{}); err != nil {
		return fmt.Errorf("remove container %s: %w", ref, err)
	}
	return nil
}

// Shutdown is a no-op: a Running instance's container is managed by the
// container runtime daemon independent of the agent's own process
// lifetime, and is deliberately left running across an agent restart - see
// docs/AGENT.md Signal Handling.
func (b *Backend) Shutdown(ctx context.Context) error {
	return nil
}

// IsRunning reports whether instanceID's container is currently running -
// see runtime.Backend's doc comment. Finds the container the same way
// Stop does (resolve), so - like it - it needs no state of its own to answer.
func (b *Backend) IsRunning(ctx context.Context, instanceID string) (bool, error) {
	ref, err := b.resolve(ctx, instanceID)
	if err != nil {
		return false, err
	}
	result, err := b.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", ref, err)
	}
	if result.Container.State == nil {
		return false, nil
	}
	return result.Container.State.Running, nil
}

// Logs returns instanceID's most recent stdout/stderr output, up to
// tailLines - see runtime.Backend's own doc comment: best-effort
// diagnostic evidence for a launch-readiness failure report, never
// required for correctness. Docker multiplexes stdout/stderr into one
// stream when the container has no TTY (Sparky never allocates one), so
// stdcopy.StdCopy demultiplexes it back into plain text rather than
// returning the raw framed bytes, which would otherwise interleave
// unprintable frame headers into the diagnostic message a human is meant
// to read.
func (b *Backend) Logs(ctx context.Context, instanceID string, tailLines int) (string, error) {
	ref, err := b.resolve(ctx, instanceID)
	if err != nil {
		return "", err
	}
	rc, err := b.cli.ContainerLogs(ctx, ref, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tailLines),
	})
	if err != nil {
		return "", fmt.Errorf("get logs for container %s: %w", ref, err)
	}
	defer rc.Close()

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, rc); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read logs for container %s: %w", ref, err)
	}
	return out.String(), nil
}
