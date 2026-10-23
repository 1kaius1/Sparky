// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gpucheck proves that a container started the way Sparky starts its
// engines gets a GPU: it runs a short container with exactly the GPU request
// a model launch sends (runtime.GPURequest) and `nvidia-smi -L`, and reports
// what the container saw. It exists because a container runtime can accept a
// GPU request and silently ignore it - the container then starts with no GPU
// and no error, and only the engine's own failure, minutes later, hints at
// why. `sparky-agent check-gpu` is its first caller.
package gpucheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// Result statuses.
const (
	StatusOK            = "ok"
	StatusFailed        = "failed"
	StatusNotApplicable = "not_applicable"
)

const (
	// DefaultImage needs no GPU libraries of its own: the runtime injects
	// nvidia-smi into the container when the GPU request takes effect. It is
	// overridable (SPARKY_GPU_CHECK_IMAGE) for a node that cannot pull it.
	DefaultImage = "docker.io/library/ubuntu:24.04"

	// DefaultTimeout covers a cold image pull on a slow link.
	DefaultTimeout = 3 * time.Minute

	defaultPollInterval = 200 * time.Millisecond
	cleanupTimeout      = 30 * time.Second
)

// Options tune a check. The zero value is usable.
type Options struct {
	Image        string
	Timeout      time.Duration
	PollInterval time.Duration // tests only
}

// Result is what a check found.
type Result struct {
	Status string
	// GPUs is how many GPUs nvidia-smi listed inside the container.
	GPUs int
	// Detail says what happened, in one or two sentences.
	Detail string
	// Output is what the probe container printed, trimmed.
	Output string
	// Hint is a remediation for a failure Sparky recognizes; empty otherwise.
	Hint string
}

var gpuLine = regexp.MustCompile(`(?m)^GPU \d+:`)

// Run checks GPU passthrough for runtimeBackend using be. A bare-metal (or
// unknown) backend has no passthrough boundary and reports not applicable.
// The probe container is always removed.
func Run(ctx context.Context, be runtime.Backend, runtimeBackend string, opts Options) Result {
	mechanism, cdi, env := runtime.GPURequest(runtimeBackend)
	if mechanism == "" {
		return Result{
			Status: StatusNotApplicable,
			Detail: fmt.Sprintf("the %q backend gives the engine direct access to the host's GPU, so there is no container passthrough to check", runtimeBackend),
		}
	}

	image := opts.Image
	if image == "" {
		image = DefaultImage
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	id := fmt.Sprintf("gpucheck-%d", time.Now().UnixNano())
	// A fresh context: the probe must be removed even when ctx has expired,
	// which is exactly when it is most likely to be left behind.
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = be.Stop(cleanCtx, id)
	}()

	_, err := be.Start(ctx, runtime.Spec{
		InstanceID:         id,
		Image:              image,
		Args:               []string{"nvidia-smi", "-L"},
		Env:                env,
		GPUDeviceMechanism: mechanism,
		CDIDevices:         cdi,
		Probe:              true,
	})
	if err != nil {
		return fail(runtimeBackend, fmt.Sprintf("could not start the probe container: %v", err), err.Error())
	}

	// The same guard a launch uses, first: it gives a definite verdict from
	// what the daemon reports, before any output has to be interpreted.
	if v, ok := be.(runtime.GPUVerifier); ok {
		if err := v.VerifyGPU(ctx, id, mechanism); errors.Is(err, runtime.ErrNoGPU) {
			return fail(runtimeBackend, err.Error(), err.Error())
		}
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		running, err := be.IsRunning(ctx, id)
		if err != nil {
			return fail(runtimeBackend, fmt.Sprintf("could not tell whether the probe finished: %v", err), err.Error())
		}
		if !running {
			break
		}
		select {
		case <-ctx.Done():
			return fail(runtimeBackend, fmt.Sprintf("the probe container did not finish within %s", timeout), "")
		case <-ticker.C:
		}
	}

	logs, err := be.Logs(ctx, id, 100)
	if err != nil {
		return fail(runtimeBackend, fmt.Sprintf("could not read the probe container's output: %v", err), err.Error())
	}
	output := strings.TrimSpace(logs)

	if n := len(gpuLine.FindAllString(output, -1)); n > 0 {
		return Result{
			Status: StatusOK,
			GPUs:   n,
			Detail: fmt.Sprintf("a container started the way Sparky starts engines sees %d GPU(s)", n),
			Output: output,
		}
	}
	r := fail(runtimeBackend, "the probe container ran but nvidia-smi inside it listed no GPU", output)
	r.Output = output
	return r
}

func fail(runtimeBackend, detail, evidence string) Result {
	return Result{Status: StatusFailed, Detail: detail, Hint: hintFor(runtimeBackend, detail+"\n"+evidence)}
}

// hintFor maps an error or output Sparky has seen, on a real host, to what to
// do about it. It matches on the text the container tools print, so it only
// recognises messages seen on a real host or documented by the tool (the
// Podman ones were observed on a real GPU host; the Docker ones are from the
// tool's documentation - the success path was run on a real Docker daemon, but
// its failure cases were not provoked, and a node that works without a
// registered "nvidia" runtime exists); anything else gets no hint rather than a
// guess.
func hintFor(runtimeBackend, text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "unresolvable cdi devices"):
		return "Podman could not resolve nvidia.com/gpu=all: there is no usable CDI spec. Generate one with `sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`, then confirm `podman run --rm --device nvidia.com/gpu=all " + DefaultImage + " nvidia-smi -L` works."
	case strings.Contains(t, "could not select device driver") || strings.Contains(t, "unknown or invalid runtime name: nvidia"):
		return "Docker cannot find the NVIDIA runtime. Install nvidia-container-toolkit and run `sudo nvidia-ctk runtime configure --runtime=docker`, then restart Docker."
	case strings.Contains(t, "no devices") || strings.Contains(t, "dropped it"):
		if runtimeBackend == "podman" {
			return "Podman accepted the request but injected no GPU devices. Run `podman run --log-level=debug --device nvidia.com/gpu=all " + DefaultImage + " true` and look for \"failed to parse CDI Spec\": a CDI spec Podman cannot read is skipped silently. nvidia-ctk 1.20.x writes specs (with an `additionalGids` field) that Podman 4.9 rejects; keep a version-0.6.0 spec in /etc/cdi."
		}
		return "Docker accepted the container but dropped the GPU request. Check that nvidia-container-toolkit is installed and the NVIDIA runtime is configured."
	case strings.Contains(t, "nvidia-smi") && (strings.Contains(t, "not found") || strings.Contains(t, "not found in $path")):
		return "The container started without a GPU (nvidia-smi was not injected). Check the CDI or NVIDIA runtime configuration for this runtime."
	case strings.Contains(t, "no such host") || strings.Contains(t, "manifest unknown") || strings.Contains(t, "pull image") || strings.Contains(t, "unauthorized") || strings.Contains(t, "i/o timeout"):
		return "The probe image could not be pulled. Pull it by hand, or set SPARKY_GPU_CHECK_IMAGE to an image this node already has."
	case strings.Contains(t, "dial unix") || strings.Contains(t, "connect: no such file") || strings.Contains(t, "connect: permission denied") || strings.Contains(t, "cannot connect") || strings.Contains(t, "connection refused"):
		return "The container daemon could not be reached. Check DOCKER_HOST, that the socket exists, and that this account may use it."
	}
	return ""
}
