// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build realdaemon

package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/gpucheck"
	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
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
	shm := 2
	spec.Container = agentproto.ContainerOptions{IPCMode: "host", ShmSizeGB: &shm}

	logs, _ := runToExit(t, b, spec)
	t.Logf("container output:\n%s", logs)
	if !strings.Contains(logs, "GPU 0") {
		t.Errorf("no GPU visible with host IPC, a shm size and a port.\noutput: %s", logs)
	}
	assertGoneAfterStop(t, b, spec.InstanceID)
}

// daemonBackend is realBackend without the GPU requirement, for the tests that
// only need a daemon.
func daemonBackend(t *testing.T) *Backend {
	t.Helper()
	if os.Getenv("DOCKER_HOST") == "" {
		t.Skip("DOCKER_HOST is not set - no container daemon to test against")
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// everyOption sets as many container options as can run together on an
// ordinary host without a GPU or special privileges, so the tests below can
// check that each one actually reached the container. The ulimits are lowered
// rather than raised: a rootless daemon cannot raise a limit above its own
// hard limit (so memlock -1, the value GPU engines want, is checked in the
// unit tests only).
func everyOption(mountDir string) agentproto.ContainerOptions {
	shm, mem, cpus := 1, 1.0, 1.5
	return agentproto.ContainerOptions{
		ShmSizeGB:   &shm,
		MemoryGB:    &mem,
		CPUs:        &cpus,
		Init:        true,
		AddHosts:    []string{"sparky-test-host:10.9.8.7"},
		Tmpfs:       []string{"/scratch"},
		Mounts:      []agentproto.ContainerMount{{Source: mountDir, Target: "/extra", ReadOnly: true}},
		CapAdd:      []string{"IPC_LOCK"},
		CapDrop:     []string{"CHOWN"},
		SecurityOpt: []string{"no-new-privileges"},
		Ulimits:     []agentproto.ContainerUlimit{{Name: "memlock", Soft: 1 << 20, Hard: 2 << 20}, {Name: "nofile", Soft: 2048, Hard: 4096}},
	}
}

// checkEveryOptionApplied runs a shell inside the container that reports what
// it sees, plus the daemon's own record of the container, so a field a daemon
// accepted and ignored shows up as a failure rather than a pass.
func checkEveryOptionApplied(t *testing.T, logs, inspect string) {
	t.Helper()
	for what, want := range map[string]string{
		"extra bind mount readable": "extra-marker",
		"tmpfs mounted":             "tmpfs /scratch",
		"add_hosts in /etc/hosts":   "10.9.8.7",
		"memlock soft ulimit (1MiB, which ulimit -l prints in KiB)": "memlock=1024",
		"nofile soft ulimit":          "nofile=2048",
		"IPC_LOCK capability granted": "ipc-lock=yes",
		"CHOWN capability dropped":    "chown=no",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("%s: %q not in the container's output.\n%s", what, want, logs)
		}
	}
	for what, want := range map[string]string{
		"memory limit 1GiB":    `"Memory":1073741824`,
		"cpu quota 1.5":        `"NanoCpus":1500000000`,
		"init process":         `"Init":true`,
		"no-new-privileges":    "no-new-privileges",
		"shm size 1GiB":        `"ShmSize":1073741824`,
		"extra bind read-only": ":ro",
	} {
		if !strings.Contains(inspect, want) {
			t.Errorf("%s: %q not in the daemon's record of the container.\n%s", what, want, inspect)
		}
	}
}

const reportScript = `cat /extra/extra-marker; ` +
	`grep -q ' /scratch tmpfs ' /proc/mounts && echo "tmpfs /scratch"; ` +
	`grep sparky-test-host /etc/hosts; ` +
	`echo "memlock=$(ulimit -l) nofile=$(ulimit -n)"; ` +
	`cap=$(awk '/^CapEff/{print $2}' /proc/self/status); ` +
	`if [ $(( 0x$cap >> 14 & 1 )) = 1 ]; then echo ipc-lock=yes; else echo ipc-lock=no; fi; ` +
	`if [ $(( 0x$cap & 1 )) = 1 ]; then echo chown=yes; else echo chown=no; fi`

// optionsInspect is the part of the daemon's record that shows the limits and
// flags that cannot be seen from inside the container.
func optionsInspect(t *testing.T, b *Backend, instanceID string) string {
	t.Helper()
	ctx := context.Background()
	ref, err := b.resolve(ctx, instanceID)
	if err != nil {
		t.Fatalf("resolve() error: %v", err)
	}
	res, err := b.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect() error: %v", err)
	}
	hc := res.Container.HostConfig
	out, _ := json.Marshal(map[string]any{
		"Memory": hc.Memory, "NanoCpus": hc.NanoCPUs, "Init": hc.Init, "ShmSize": hc.ShmSize,
		"SecurityOpt": hc.SecurityOpt, "CapDrop": hc.CapDrop, "CapAdd": hc.CapAdd, "Binds": hc.Binds,
	})
	return string(out)
}

// isPodman reports whether the daemon answers Podman's own ping endpoint.
func isPodman(b *Backend) bool {
	lc, ok := b.libpod.(*libpodClient)
	if !ok || lc.err != nil {
		return false
	}
	resp, err := lc.http.Get(lc.base + "/libpod/_ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// The Docker-compatible path, as a Docker node uses it: every container option
// must reach the container. Not run against Podman, whose Docker-compatible
// create call silently drops Ulimits (verified on 4.9.3: the CLI applies
// --ulimit, the same request through this API does not) - the same class of
// gap as the CDI one in libpod.go. A Podman node never takes this path: it
// always requests its GPU by CDI name, which goes through the native API, and
// TestRealDaemon_ContainerOptions_PodmanNativePath covers that.
func TestRealDaemon_ContainerOptions_CompatPath(t *testing.T) {
	b := daemonBackend(t)
	if isPodman(b) {
		t.Skip("Podman's Docker-compatible API drops Ulimits; Podman nodes use the native API (see the native-path test)")
	}

	mountDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mountDir, "extra-marker"), []byte("extra-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := realSpec(fmt.Sprintf("realdaemon-opts-%d", time.Now().UnixNano()), []string{"sh", "-c", reportScript}, "", nil, nil)
	spec.Container = everyOption(mountDir)

	// The exited container is still there to inspect; runToExit removes it
	// in a cleanup after the test.
	logs, _ := runToExit(t, b, spec)
	checkEveryOptionApplied(t, logs, optionsInspect(t, b, spec.InstanceID))
}

// Podman's native create call (used when a CDI GPU is requested) takes the
// options in its own spelling. A GPU is not needed to test that spelling:
// the request is built exactly as Start builds it and sent to the native API
// directly, with no CDI device in it.
func TestRealDaemon_ContainerOptions_PodmanNativePath(t *testing.T) {
	b := daemonBackend(t)
	if b.libpod == nil {
		t.Skip("no native API client")
	}

	mountDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mountDir, "extra-marker"), []byte("extra-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("realdaemon-native-%d", time.Now().UnixNano())
	spec := realSpec(id, []string{"sh", "-c", reportScript}, "", nil, nil)
	spec.Container = everyOption(mountDir)

	ctx := context.Background()
	labels := map[string]string{labelInstanceID: id, labelProbe: "true"}
	req, err := buildLibpodRequest(spec, "sparky-"+id, labels)
	if err != nil {
		t.Fatalf("buildLibpodRequest() error: %v", err)
	}
	cid, err := b.libpod.createContainer(ctx, req)
	if err != nil {
		t.Fatalf("native create rejected the request: %v", err)
	}
	t.Cleanup(func() { b.Stop(context.Background(), id) })
	if _, err := b.cli.ContainerStart(ctx, cid, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		running, err := b.IsRunning(ctx, id)
		if err != nil {
			t.Fatalf("IsRunning() error: %v", err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container still running after 60s")
		}
	}
	logs, err := b.Logs(ctx, id, 200)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	checkEveryOptionApplied(t, logs, optionsInspect(t, b, id))
}

// The namespace and entrypoint options through Podman's native API: host
// network and PID namespaces, a replaced entrypoint, and privileged mode. Each
// is compared with what the test process itself sees, so a daemon that accepts
// and ignores one fails the comparison.
func TestRealDaemon_ContainerOptions_PodmanNativePathNamespacesEntrypointPrivileged(t *testing.T) {
	b := daemonBackend(t)
	if b.libpod == nil || !isPodman(b) {
		t.Skip("needs Podman's native API")
	}

	id := fmt.Sprintf("realdaemon-ns-%d", time.Now().UnixNano())
	spec := realSpec(id, []string{
		// With the entrypoint replaced by ["sh","-c"], this one argument is
		// the script - exactly the contract buildEngineLaunchArgs relies on.
		`echo pid1=$(cat /proc/1/comm); echo nets=$(ls /sys/class/net | wc -l); echo capeff=$(awk '/^CapEff/{print $2}' /proc/self/status)`,
	}, "", nil, nil)
	spec.Port = 18998
	spec.Container = agentproto.ContainerOptions{
		NetworkMode: "host",
		PIDMode:     "host",
		Entrypoint:  []string{"sh", "-c"},
		Privileged:  true,
	}

	ctx := context.Background()
	labels := map[string]string{labelInstanceID: id, labelProbe: "true"}
	req, err := buildLibpodRequest(spec, "sparky-"+id, labels)
	if err != nil {
		t.Fatalf("buildLibpodRequest() error: %v", err)
	}
	if len(req.PortMappings) != 0 {
		t.Fatalf("PortMappings = %+v, want none under host networking", req.PortMappings)
	}
	cid, err := b.libpod.createContainer(ctx, req)
	if err != nil {
		t.Fatalf("native create rejected the request: %v", err)
	}
	t.Cleanup(func() { b.Stop(context.Background(), id) })
	if _, err := b.cli.ContainerStart(ctx, cid, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		running, err := b.IsRunning(ctx, id)
		if err != nil {
			t.Fatalf("IsRunning() error: %v", err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container still running after 60s")
		}
	}
	logs, err := b.Logs(ctx, id, 50)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}

	hostPID1, _ := os.ReadFile("/proc/1/comm")
	nets, _ := os.ReadDir("/sys/class/net")
	if want := "pid1=" + strings.TrimSpace(string(hostPID1)); !strings.Contains(logs, want) {
		t.Errorf("host PID namespace: %q not in the output.\n%s", want, logs)
	}
	if want := fmt.Sprintf("nets=%d", len(nets)); !strings.Contains(logs, want) {
		t.Errorf("host network namespace: %q not in the output.\n%s", want, logs)
	}
	// Privileged: every capability the kernel knows, which is more than the
	// default set (0x...05fb in a rootless container).
	if strings.Contains(logs, "capeff=00000000800405fb") || !strings.Contains(logs, "capeff=") {
		t.Errorf("privileged mode did not widen the capabilities.\n%s", logs)
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
