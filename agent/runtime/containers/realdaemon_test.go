// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build realdaemon

package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

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
}
