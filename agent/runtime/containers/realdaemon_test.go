// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build realdaemon

package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/gpucheck"
	"github.com/1kaius1/Sparky/agent/runtime"
)

// These tests drive the real Backend against a real container daemon and a
// real GPU, so they only run when asked for:
//
//	DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
//	    go test -tags realdaemon -run TestRealDaemon -v ./agent/runtime/containers
//
// They exist because the request shape the agent sends (see
// agent/connection's gpuPassthrough) can only be shown to work, or not, by a
// real daemon: a fake accepts anything. The 2026-08-10 note on Backend that
// Podman's Docker-compatible API did not resolve CDI names was recorded on a
// host with no GPU and no CDI spec, so it says little about a host that has
// both. Run them against the rootless and the rootful socket alike.
//
// SPARKY_REALDAEMON_IMAGE overrides the image (default ubuntu:24.04, which
// needs no GPU libraries of its own - the runtime injects nvidia-smi).

// podmanCDISpec mirrors what agent/connection.gpuPassthrough returns for the
// podman backend. Kept as a literal here because that function is unexported
// in another package; connection_test's TestGPUPassthrough_Podman_CDIMechanism
// pins the real one to the same values.
func podmanCDISpec() (runtime.GPUDeviceMechanism, []string) {
	return runtime.GPUDeviceMechanismCDI, []string{"nvidia.com/gpu=all"}
}

func realBackend(t *testing.T) *Backend {
	t.Helper()
	if os.Getenv("DOCKER_HOST") == "" {
		t.Skip("DOCKER_HOST is not set - no container daemon to test against")
	}
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Skip("nvidia-smi not found on the host - no GPU to test against")
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func realImage() string {
	if v := os.Getenv("SPARKY_REALDAEMON_IMAGE"); v != "" {
		return v
	}
	return "docker.io/library/ubuntu:24.04"
}

// runToExit starts spec, waits for it to exit, and returns the container's
// combined output and its inspect JSON (for the evidence log). The container
// is always removed.
func runToExit(t *testing.T, b *Backend, spec runtime.Spec) (logs, inspect string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { b.Stop(context.Background(), spec.InstanceID) })

	if _, err := b.Start(ctx, spec); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		running, err := b.IsRunning(ctx, spec.InstanceID)
		if err != nil {
			t.Fatalf("IsRunning() error: %v", err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container still running after 90s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	logs, err := b.Logs(ctx, spec.InstanceID, 200)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}

	ref, err := b.resolve(ctx, spec.InstanceID)
	if err != nil {
		t.Fatalf("resolve() error: %v", err)
	}
	res, err := b.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect() error: %v", err)
	}
	// Only the fields a GPU guard could read: what the daemon reports as
	// requested and as applied. Podman's inspect has no DeviceRequests field,
	// so the devices it injected are the only signal there.
	evidence, _ := json.Marshal(map[string]any{
		"Devices":        res.Container.HostConfig.Devices,
		"DeviceRequests": res.Container.HostConfig.DeviceRequests,
		"Env":            res.Container.Config.Env,
	})
	return logs, string(evidence)
}

// After Stop the container is gone, which IsRunning reports as not running
// (the Dead-state contract), and the compat API finds nothing by label.
func assertGoneAfterStop(t *testing.T, b *Backend, instanceID string) {
	t.Helper()
	ctx := context.Background()
	if err := b.Stop(ctx, instanceID); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if running, err := b.IsRunning(ctx, instanceID); err != nil || running {
		t.Errorf("after Stop: IsRunning() = %v, %v, want false, nil", running, err)
	}
	if got, err := b.InstancesForProfile(ctx, "no-such-profile"); err != nil || len(got) != 0 {
		t.Errorf("InstancesForProfile() = %v, %v, want none", got, err)
	}
}

func realSpec(id string, args []string, mech runtime.GPUDeviceMechanism, cdi []string, mounts []string) runtime.Spec {
	return runtime.Spec{
		InstanceID:         id,
		Image:              realImage(),
		Args:               args,
		Mounts:             mounts,
		GPUDeviceMechanism: mech,
		CDIDevices:         cdi,
	}
}

// The request Sparky sends for a podman node must put a GPU in the container,
// and a read-only bind mount (which the agent always adds for model storage)
// must still work alongside the CDI hooks.
func TestRealDaemon_PodmanCDIGivesTheContainerAGPU(t *testing.T) {
	b := realBackend(t)

	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "model-marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mech, cdi := podmanCDISpec()
	spec := realSpec(fmt.Sprintf("realdaemon-cdi-%d", time.Now().UnixNano()),
		[]string{"sh", "-c", "nvidia-smi -L; ls " + modelDir},
		mech, cdi, []string{modelDir + ":" + modelDir + ":ro"})

	logs, inspect := runToExit(t, b, spec)
	t.Logf("container output:\n%s", logs)
	t.Logf("inspect evidence: %s", inspect)

	if !strings.Contains(logs, "GPU 0") {
		t.Errorf("no GPU visible inside the container - the CDI request through the Docker-compatible API did not take effect.\noutput: %s", logs)
	}
	if !strings.Contains(logs, "model-marker") {
		t.Errorf("the read-only bind mount was not readable inside the container.\noutput: %s", logs)
	}
	// The per-launch guard agrees with what the container really saw.
	if err := b.VerifyGPU(context.Background(), spec.InstanceID, mech); err != nil {
		t.Errorf("VerifyGPU() on a container that has its GPU = %v, want nil", err)
	}
	assertGoneAfterStop(t, b, spec.InstanceID)
}

// The image's own ENTRYPOINT must survive: Sparky's args replace the image's
// CMD, as with the compat API. nginx has an ENTRYPOINT script that execs its
// arguments.
func TestRealDaemon_PodmanCDIKeepsTheImageEntrypoint(t *testing.T) {
	b := realBackend(t)

	mech, cdi := podmanCDISpec()
	spec := realSpec(fmt.Sprintf("realdaemon-entry-%d", time.Now().UnixNano()),
		[]string{"echo", "args-replaced-the-cmd"}, mech, cdi, nil)
	spec.Image = "docker.io/library/nginx:latest"
	spec.Env = []string{"SPARKY_TEST_ENV=from-spec"}

	logs, inspect := runToExit(t, b, spec)
	t.Logf("container output:\n%s", logs)
	t.Logf("inspect evidence: %s", inspect)

	if !strings.Contains(logs, "args-replaced-the-cmd") {
		t.Errorf("the arguments did not reach the entrypoint.\noutput: %s", logs)
	}
	if !strings.Contains(inspect, "SPARKY_TEST_ENV=from-spec") {
		t.Errorf("the environment was not applied: %s", inspect)
	}
	assertGoneAfterStop(t, b, spec.InstanceID)
}

// What a vLLM profile with shm_size_gb sends: a published port, host IPC and a
// shm size together with the GPU. Podman refuses a shm size under host IPC, so
// the native path must drop it rather than fail the launch.
func TestRealDaemon_PodmanCDIWithHostIPCShmSizeAndPort(t *testing.T) {
	b := realBackend(t)

	mech, cdi := podmanCDISpec()
	spec := realSpec(fmt.Sprintf("realdaemon-ipc-%d", time.Now().UnixNano()),
		[]string{"sh", "-c", "nvidia-smi -L"}, mech, cdi, nil)
	spec.Port = 18999
	spec.IPCMode = "host"
	spec.ShmSize = 2 << 30

	logs, _ := runToExit(t, b, spec)
	t.Logf("container output:\n%s", logs)
	if !strings.Contains(logs, "GPU 0") {
		t.Errorf("no GPU visible with host IPC, a shm size and a port.\noutput: %s", logs)
	}
	assertGoneAfterStop(t, b, spec.InstanceID)
}

// Negative control: without a GPU request the same container must not see a
// GPU, otherwise the positive test above proves nothing.
func TestRealDaemon_NoGPURequestMeansNoGPU(t *testing.T) {
	b := realBackend(t)

	spec := realSpec(fmt.Sprintf("realdaemon-none-%d", time.Now().UnixNano()),
		[]string{"sh", "-c", "nvidia-smi -L"},
		"", nil, nil)

	logs, inspect := runToExit(t, b, spec)
	t.Logf("container output:\n%s", logs)
	t.Logf("inspect evidence: %s", inspect)

	if strings.Contains(logs, "GPU 0") {
		t.Errorf("a GPU was visible without any GPU request - the control is invalid.\noutput: %s", logs)
	}
	// ...and the guard calls it: a container that was meant to have a CDI GPU
	// and has none is exactly the silent failure it exists to catch.
	mech, _ := podmanCDISpec()
	if err := b.VerifyGPU(context.Background(), spec.InstanceID, mech); !errors.Is(err, runtime.ErrNoGPU) {
		t.Errorf("VerifyGPU() on a container with no GPU = %v, want ErrNoGPU", err)
	}
}

// The diagnostic `sparky-agent check-gpu` runs reports a working node as ok.
func TestRealDaemon_GPUCheckReportsAWorkingPodmanNode(t *testing.T) {
	b := realBackend(t)

	res := gpucheck.Run(context.Background(), b, "podman", gpucheck.Options{Image: realImage()})
	t.Logf("result: %+v", res)
	if res.Status != gpucheck.StatusOK || res.GPUs < 1 {
		t.Errorf("result = %+v, want ok with at least one GPU", res)
	}
	// The probe removed itself.
	if running, err := b.IsRunning(context.Background(), "gpucheck-anything"); err != nil || running {
		t.Errorf("IsRunning() = %v, %v", running, err)
	}
}

// Negative control for the diagnostic: asking a Podman daemon for a GPU the way
// Docker does is accepted and silently dropped, and the check must say so
// rather than report a pass.
func TestRealDaemon_GPUCheckFailsWhenTheRequestIsTheWrongKindForTheDaemon(t *testing.T) {
	b := realBackend(t)

	res := gpucheck.Run(context.Background(), b, "docker", gpucheck.Options{Image: realImage()})
	t.Logf("result: %+v", res)
	if res.Status != gpucheck.StatusFailed {
		t.Errorf("result = %+v, want failed: a Docker-style GPU request is not honored by Podman", res)
	}
}
