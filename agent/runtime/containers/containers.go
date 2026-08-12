// SPDX-License-Identifier: AGPL-3.0-or-later

// Package containers is the Docker/Podman runtime backend - see
// ARCHITECTURE.md Runtime Backends. One implementation serves both
// runtimes identically, since Podman exposes a Docker-Engine-API-
// compatible socket - see CLAUDE.md Tech Stack.
package containers

import (
	"context"
	"fmt"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// cdiDriver is the device driver name the daemon registers for CDI device
// injection - confirmed against moby/moby's own daemon/cdi.go
// (RegisterCDIDriver registers it under this exact name), not guessed from
// CLI documentation. See the CDI caveat in this package's doc comment on
// Backend for a real gap found testing this against Podman.
const cdiDriver = "cdi"

// Spec describes a container to start.
type Spec struct {
	Image string
	Name  string
	Env   []string

	// Cmd is the command-line arguments passed to the container's
	// entrypoint - e.g. an engine adapter's LaunchSpec.Args, plus
	// whatever --model/--port flags the caller has already resolved. Nil
	// means run the image's own default command.
	Cmd []string

	// Port, if nonzero, is published from the container to the identical
	// port number on the host - Sparky always runs an engine's server on
	// the same port number inside the container as the profile declares
	// (SCHEMA.md Running instances' actual_port), so there is only ever
	// one port number to track, not a separate host/container pair. Zero
	// means no port is published.
	Port int

	// Mounts are bind mounts in "hostPath:containerPath[:mode]" form
	// (Docker's -v syntax) - used to give the container access to the
	// agent's local model storage directory at the identical path inside
	// the container as on the host, so a --model flag resolved against
	// the host path needs no translation to also resolve inside the
	// container.
	Mounts []string

	// CDIDevices are CDI-qualified device names (e.g. "nvidia.com/gpu=all")
	// - see docs/AGENT.md Tech Stack and ARCHITECTURE.md Runtime Backends.
	// Nil/empty means no GPU passthrough.
	CDIDevices []string
}

// InstanceContainerName returns the deterministic container name Sparky
// uses for a Running instance. StartContainer (via Spec.Name) and
// StopContainer (which the Docker Engine API accepts a name for
// interchangeably with an ID) both key off this same value, so the
// central app never needs to track a live container ID of its own - see
// agent/connection's load_instance/unload_instance dispatch.
func InstanceContainerName(instanceID string) string {
	return "sparky-instance-" + instanceID
}

// dockerClient is the subset of *client.Client this package uses, narrow
// enough to fake in tests - same pattern as internal/auth's ldapConn.
type dockerClient interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	Close() error
}

// Backend manages containers via the Docker Engine API.
//
// CDI caveat, found by testing against a real local Podman 4.9.3 daemon,
// not assumed: Podman's own CLI resolves CDI-qualified device names
// correctly (confirmed via `podman run --device nvidia.com/gpu=all`,
// which fails with a proper "unresolvable CDI devices" error - no CDI
// spec exists on a GPU-less test host, which is the expected failure).
// But going through the Docker-Engine-API-compatible socket this package
// actually uses, neither of the two mechanisms Docker's own API supports -
// HostConfig.DeviceRequests with Driver "cdi" (confirmed correct against
// moby/moby's daemon source) nor HostConfig.Devices with a CDI name as
// PathOnHost - triggered any CDI resolution: DeviceRequests was silently
// accepted and dropped (no error, but no device either - confirmed via
// `podman inspect`, which does not even have a DeviceRequests field to
// report), and Devices was treated as a literal host path and failed a
// plain stat(). This package implements the documented, correct Docker
// API contract (DeviceRequests), which is right for a real Docker daemon
// and the best available attempt for Podman's compat API - but CDI
// passthrough through that API on Podman needs verification against the
// actual target Podman version, per ARCHITECTURE.md's existing manual
// test checklist item for this. Non-GPU container lifecycle (create,
// pull-if-missing, start, inspect, stop, remove) is fully verified
// against real Podman and unaffected by this gap.
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

// StartContainer creates and starts a container from spec, returning its
// ID. If spec.Image is not already present on the node, it is pulled
// first - unlike the `docker run` CLI, the raw Engine API's ContainerCreate
// does not pull a missing image itself, confirmed empirically against a
// real daemon rather than assumed.
func (b *Backend) StartContainer(ctx context.Context, spec Spec) (string, error) {
	var hostConfig container.HostConfig
	if len(spec.CDIDevices) > 0 {
		hostConfig.DeviceRequests = []container.DeviceRequest{
			{
				Driver:    cdiDriver,
				DeviceIDs: spec.CDIDevices,
			},
		}
	}
	if len(spec.Mounts) > 0 {
		hostConfig.Binds = spec.Mounts
	}

	config := &container.Config{
		Image: spec.Image,
		Env:   spec.Env,
		Cmd:   spec.Cmd,
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
		Name:       spec.Name,
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

// StopContainer stops and removes a container by ID.
func (b *Backend) StopContainer(ctx context.Context, containerID string) error {
	if _, err := b.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("stop container %s: %w", containerID, err)
	}
	if _, err := b.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{}); err != nil {
		return fmt.Errorf("remove container %s: %w", containerID, err)
	}
	return nil
}

// IsRunning reports whether the container is currently running.
func (b *Backend) IsRunning(ctx context.Context, containerID string) (bool, error) {
	result, err := b.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	if result.Container.State == nil {
		return false, nil
	}
	return result.Container.State.Running, nil
}
