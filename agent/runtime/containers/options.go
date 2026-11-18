// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"github.com/moby/moby/api/types/container"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

const (
	bytesPerGiB = int64(1) << 30
	nanoPerCPU  = 1_000_000_000
)

// hostNetwork reports whether the profile asks for the node's own network
// namespace. The engine then listens on the node's port directly, so there is
// nothing to publish: Docker discards published ports in that mode and Podman
// refuses them.
func hostNetwork(o agentproto.ContainerOptions) bool {
	return o.NetworkMode == "host"
}

// shmBytes is the /dev/shm size to request, zero for the runtime's default.
func shmBytes(o agentproto.ContainerOptions) int64 {
	if o.ShmSizeGB == nil {
		return 0
	}
	return int64(*o.ShmSizeGB) * bytesPerGiB
}

// bindString is the "source:target[:ro]" form Docker's Binds takes.
func bindString(m agentproto.ContainerMount) string {
	s := m.Source + ":" + m.Target
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// applyContainerOptions copies a profile's container options onto the create
// request the Docker Engine API takes - the equivalents of the matching
// `docker run` flags. Unset fields leave the runtime's own default. The values
// were validated (internal/launchopts) before they reached here; this only
// translates them. A mount's source and target contain no ':' by that
// validation, which is what keeps bindString from being steered into a
// different mapping than the one the profile named.
func applyContainerOptions(o agentproto.ContainerOptions, hostConfig *container.HostConfig, config *container.Config) {
	if n := shmBytes(o); n > 0 {
		hostConfig.ShmSize = n
	}
	if o.IPCMode != "" {
		hostConfig.IpcMode = container.IpcMode(o.IPCMode)
	}
	if o.NetworkMode != "" {
		hostConfig.NetworkMode = container.NetworkMode(o.NetworkMode)
	}
	if o.PIDMode != "" {
		hostConfig.PidMode = container.PidMode(o.PIDMode)
	}
	if len(o.Entrypoint) > 0 {
		config.Entrypoint = o.Entrypoint
	}
	if o.MemoryGB != nil {
		hostConfig.Memory = int64(*o.MemoryGB * float64(bytesPerGiB))
	}
	if o.CPUs != nil {
		hostConfig.NanoCPUs = int64(*o.CPUs * nanoPerCPU)
	}
	if o.Init {
		init := true
		hostConfig.Init = &init
	}
	if len(o.AddHosts) > 0 {
		hostConfig.ExtraHosts = append(hostConfig.ExtraHosts, o.AddHosts...)
	}
	if len(o.Tmpfs) > 0 {
		hostConfig.Tmpfs = make(map[string]string, len(o.Tmpfs))
		for _, p := range o.Tmpfs {
			hostConfig.Tmpfs[p] = ""
		}
	}
	hostConfig.Privileged = o.Privileged
	for _, m := range o.Mounts {
		hostConfig.Binds = append(hostConfig.Binds, bindString(m))
	}
	for _, d := range o.Devices {
		hostConfig.Devices = append(hostConfig.Devices, container.DeviceMapping{
			PathOnHost: d, PathInContainer: d, CgroupPermissions: "rwm",
		})
	}
	hostConfig.CapAdd = append(hostConfig.CapAdd, o.CapAdd...)
	hostConfig.CapDrop = append(hostConfig.CapDrop, o.CapDrop...)
	hostConfig.SecurityOpt = append(hostConfig.SecurityOpt, o.SecurityOpt...)
	for _, u := range o.Ulimits {
		hostConfig.Ulimits = append(hostConfig.Ulimits, &container.Ulimit{Name: u.Name, Soft: u.Soft, Hard: u.Hard})
	}
}
