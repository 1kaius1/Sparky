// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// Podman's native ("libpod") API is used for exactly one call: creating a
// container that needs a CDI GPU. Podman's Docker-compatible API cannot do
// that - found by testing against a real GPU host with a valid CDI spec, not
// assumed: HostConfig.DeviceRequests with Driver "cdi" is silently dropped
// (the container starts with no GPU and no error), and HostConfig.Devices
// treats the CDI name as a literal file path and fails a stat(). The native
// create call accepts the CDI name as a device and resolves it, exactly as
// `podman run --device nvidia.com/gpu=all` does. Everything else - start,
// inspect, logs, stop, remove, list, pull - stays on the compat API, which
// works on a container created either way.
//
// The request is plain JSON over HTTP on the same socket the Docker client
// uses, so this adds no dependency. The API version in the path is the
// oldest that has the create call; Podman keeps older versions working.

const libpodAPIVersion = "v4.0.0"

// libpodCreator is the one native-API call this package makes, narrow enough
// to fake in tests.
type libpodCreator interface {
	createContainer(ctx context.Context, req libpodCreateRequest) (string, error)
}

// libpodCreateRequest is the subset of Podman's SpecGenerator JSON that
// Start needs. Field names are Podman's (confirmed against a real daemon:
// env is a map, not KEY=VALUE strings, and ports and mounts are structured).
type libpodCreateRequest struct {
	Name         string              `json:"name,omitempty"`
	Image        string              `json:"image"`
	Command      []string            `json:"command,omitempty"`
	Env          map[string]string   `json:"env,omitempty"`
	Labels       map[string]string   `json:"labels,omitempty"`
	Devices      []libpodDevice      `json:"devices,omitempty"`
	Mounts       []libpodMount       `json:"mounts,omitempty"`
	PortMappings []libpodPortMapping `json:"portmappings,omitempty"`
	ShmSize      int64               `json:"shm_size,omitempty"`
	IPCNS        *libpodNamespace    `json:"ipcns,omitempty"`

	// The rest carry agentproto.ContainerOptions, the equivalents of the
	// matching `podman run` flags - see applyLibpodOptions.
	NetNS        *libpodNamespace `json:"netns,omitempty"`
	PIDNS        *libpodNamespace `json:"pidns,omitempty"`
	Entrypoint   []string         `json:"entrypoint,omitempty"`
	Privileged   bool             `json:"privileged,omitempty"`
	Init         bool             `json:"init,omitempty"`
	HostAdd      []string         `json:"hostadd,omitempty"`
	CapAdd       []string         `json:"cap_add,omitempty"`
	CapDrop      []string         `json:"cap_drop,omitempty"`
	Rlimits      []libpodRlimit   `json:"r_limits,omitempty"`
	Resources    *libpodResources `json:"resource_limits,omitempty"`
	SeccompPath  string           `json:"seccomp_profile_path,omitempty"`
	SelinuxOpts  []string         `json:"selinux_opts,omitempty"`
	ApparmorProf string           `json:"apparmor_profile,omitempty"`
	NoNewPrivs   bool             `json:"no_new_privileges,omitempty"`
}

type libpodRlimit struct {
	Type string `json:"type"`
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

type libpodResources struct {
	Memory *libpodMemoryLimit `json:"memory,omitempty"`
	CPU    *libpodCPULimit    `json:"cpu,omitempty"`
}

type libpodMemoryLimit struct {
	Limit int64 `json:"limit"`
}

type libpodCPULimit struct {
	Quota  int64  `json:"quota"`
	Period uint64 `json:"period"`
}

type libpodDevice struct {
	// Path is a CDI-qualified name such as nvidia.com/gpu=all; Podman
	// resolves it from the CDI specs, as it does for the CLI's --device.
	Path string `json:"path"`
}

type libpodMount struct {
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Options     []string `json:"options,omitempty"`
}

type libpodPortMapping struct {
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type libpodNamespace struct {
	NSMode string `json:"nsmode"`
}

// buildLibpodRequest translates what Start would send through the compat API
// into Podman's native create request, so the two paths launch the same
// container apart from how the GPU is requested.
func buildLibpodRequest(spec runtime.Spec, name string, labels map[string]string) (libpodCreateRequest, error) {
	req := libpodCreateRequest{
		Name:    name,
		Image:   spec.Image,
		Command: spec.Args,
		Labels:  labels,
	}

	for _, dev := range spec.CDIDevices {
		req.Devices = append(req.Devices, libpodDevice{Path: dev})
	}

	if len(spec.Env) > 0 {
		req.Env = make(map[string]string, len(spec.Env))
		for _, kv := range spec.Env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return libpodCreateRequest{}, fmt.Errorf("environment entry %q is not KEY=VALUE", kv)
			}
			req.Env[k] = v
		}
	}

	for _, bind := range spec.Mounts {
		m, err := parseBind(bind)
		if err != nil {
			return libpodCreateRequest{}, err
		}
		req.Mounts = append(req.Mounts, m)
	}

	// With the node's own network namespace there is nothing to publish.
	if spec.Port != 0 && !hostNetwork(spec.Container) {
		if spec.Port < 1 || spec.Port > 65535 {
			return libpodCreateRequest{}, fmt.Errorf("port %d is out of range", spec.Port)
		}
		req.PortMappings = []libpodPortMapping{{
			HostPort:      uint16(spec.Port),
			ContainerPort: uint16(spec.Port),
			Protocol:      "tcp",
		}}
	}

	if err := applyLibpodOptions(spec.Container, &req); err != nil {
		return libpodCreateRequest{}, err
	}

	return req, nil
}

// applyLibpodOptions is applyContainerOptions for Podman's native create
// request: the same agentproto.ContainerOptions, spelled the way the
// SpecGenerator wants them. Anything it cannot express is an error rather than
// being dropped, because a silently ignored security option or ulimit is
// worse than a launch that says why it did not start.
func applyLibpodOptions(o agentproto.ContainerOptions, req *libpodCreateRequest) error {
	switch o.IPCMode {
	case "":
	case "host", "private", "shareable":
		req.IPCNS = &libpodNamespace{NSMode: o.IPCMode}
	default:
		return fmt.Errorf("IPC mode %q is not supported for a podman GPU launch", o.IPCMode)
	}

	// Podman refuses a shm size together with the host IPC namespace
	// ("cannot set shmsize when running in the host IPC Namespace") - the
	// container uses the host's /dev/shm then, so the size has no meaning.
	// Sparky sends both for a vLLM profile with a shared memory size, so
	// dropping the size here is what makes that launch work on Podman at all.
	if n := shmBytes(o); n > 0 && o.IPCMode != "host" {
		req.ShmSize = n
	}

	if o.NetworkMode != "" {
		req.NetNS = &libpodNamespace{NSMode: o.NetworkMode}
	}
	if o.PIDMode != "" {
		req.PIDNS = &libpodNamespace{NSMode: o.PIDMode}
	}
	req.Entrypoint = o.Entrypoint
	req.Privileged = o.Privileged
	req.Init = o.Init
	req.HostAdd = o.AddHosts
	req.CapAdd = o.CapAdd
	req.CapDrop = o.CapDrop

	if o.MemoryGB != nil || o.CPUs != nil {
		req.Resources = &libpodResources{}
		if o.MemoryGB != nil {
			req.Resources.Memory = &libpodMemoryLimit{Limit: int64(*o.MemoryGB * float64(bytesPerGiB))}
		}
		if o.CPUs != nil {
			// CFS quota over a 100ms period, which is what --cpus means.
			const period = 100_000
			req.Resources.CPU = &libpodCPULimit{Quota: int64(*o.CPUs * period), Period: period}
		}
	}

	for _, p := range o.Tmpfs {
		req.Mounts = append(req.Mounts, libpodMount{Type: "tmpfs", Destination: p})
	}
	for _, m := range o.Mounts {
		lm := libpodMount{Type: "bind", Source: m.Source, Destination: m.Target}
		if m.ReadOnly {
			lm.Options = []string{"ro"}
		}
		req.Mounts = append(req.Mounts, lm)
	}
	for _, d := range o.Devices {
		req.Devices = append(req.Devices, libpodDevice{Path: d})
	}

	for _, u := range o.Ulimits {
		req.Rlimits = append(req.Rlimits, libpodRlimit{
			// The bare name ("memlock"): Podman adds the RLIMIT_ prefix itself
			// and rejects one that already has it (confirmed against 4.9.3).
			Type: u.Name,
			Soft: rlimitValue(u.Soft),
			Hard: rlimitValue(u.Hard),
		})
	}

	for _, s := range o.SecurityOpt {
		if err := applyLibpodSecurityOpt(s, req); err != nil {
			return err
		}
	}
	return nil
}

// rlimitValue turns a ulimit's -1 ("unlimited") into the all-ones value the
// kernel uses for it.
func rlimitValue(v int64) uint64 {
	if v < 0 {
		return math.MaxUint64
	}
	return uint64(v)
}

// applyLibpodSecurityOpt maps the --security-opt forms that matter on a GPU
// host to the SpecGenerator's own fields. Others are refused.
func applyLibpodSecurityOpt(opt string, req *libpodCreateRequest) error {
	key, value, _ := strings.Cut(opt, "=")
	switch key {
	case "seccomp":
		req.SeccompPath = value
	case "label":
		req.SelinuxOpts = append(req.SelinuxOpts, value)
	case "apparmor":
		req.ApparmorProf = value
	case "no-new-privileges":
		req.NoNewPrivs = value == "" || value == "true"
	default:
		return fmt.Errorf("security option %q is not supported for a podman GPU launch (supported: seccomp=, label=, apparmor=, no-new-privileges)", opt)
	}
	return nil
}

// parseBind turns the "host:container[:options]" form Spec.Mounts uses into a
// native bind mount, with the comma-separated options (ro, z, ...) as a list.
func parseBind(bind string) (libpodMount, error) {
	parts := strings.Split(bind, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return libpodMount{}, fmt.Errorf("mount %q is not host:container[:options]", bind)
	}
	m := libpodMount{Type: "bind", Source: parts[0], Destination: parts[1]}
	if len(parts) == 3 && parts[2] != "" {
		m.Options = strings.Split(parts[2], ",")
	}
	return m, nil
}

// libpodClient talks to Podman's native API on the daemon the Docker client is
// already pointed at.
type libpodClient struct {
	http *http.Client
	base string
	// err is set when the configured host is one this client cannot reach
	// (for example ssh://); it is reported when a GPU launch needs it, so a
	// node that never launches a CDI container is unaffected.
	err error
}

// newLibpodClient builds a client for host, the DOCKER_HOST-style address the
// Docker client resolved: unix:// (the normal case, rootless or rootful) or
// plain tcp://.
func newLibpodClient(host string) *libpodClient {
	u, err := url.Parse(host)
	if err != nil {
		return &libpodClient{err: fmt.Errorf("parse container daemon address %q: %w", host, err)}
	}
	transport := &http.Transport{ResponseHeaderTimeout: 2 * time.Minute}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		return &libpodClient{http: &http.Client{Transport: transport}, base: "http://podman"}
	case "tcp":
		return &libpodClient{http: &http.Client{Transport: transport}, base: "http://" + u.Host}
	default:
		return &libpodClient{err: fmt.Errorf("podman GPU passthrough needs a unix:// or tcp:// container daemon address, got %q", host)}
	}
}

// libpodError is Podman's JSON error body.
type libpodError struct {
	Cause    string `json:"cause"`
	Message  string `json:"message"`
	Response int    `json:"response"`
}

// createContainer creates (does not start) a container and returns its ID.
// An unknown image is reported as a not-found error so Start can pull it and
// retry, the same way it does for the compat API.
func (c *libpodClient) createContainer(ctx context.Context, req libpodCreateRequest) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode podman create request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+libpodAPIVersion+"/libpod/containers/create", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build podman create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("podman create request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read podman create response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		var pe libpodError
		if jsonErr := json.Unmarshal(raw, &pe); jsonErr != nil || pe.Message == "" {
			// A body that is not Podman's JSON error means the endpoint
			// itself is missing: not Podman, or too old to have it.
			return "", fmt.Errorf("container daemon answered %d to Podman's native create call (is it Podman?): %s", resp.StatusCode, truncate(string(raw), 200))
		}
		if resp.StatusCode == http.StatusNotFound {
			return "", cerrdefs.ErrNotFound.WithMessage(pe.Message)
		}
		return "", fmt.Errorf("podman create: %s", pe.Message)
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("podman create returned no container id: %s", truncate(string(raw), 200))
	}
	return created.ID, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
