// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

func withBackend(name string) func(*Config) {
	return func(c *Config) {
		c.RuntimeBackend = name
		c.InstanceStartupTimeout = fastReadinessTimeout()
	}
}

// A daemon that accepted the GPU request but dropped it leaves a container
// with no GPU. The launch must fail at once, naming the cause and the
// diagnostic - not wait out the readiness timeout - and the container must be
// cleaned up like any other failed launch.
func TestLoad_ContainerWithoutItsGPUFailsTheLaunchAtOnce(t *testing.T) {
	rt := &fakeRuntimeBackend{
		verifyGPUErr:  fmt.Errorf("%w: the container has no devices", agentruntime.ErrNoGPU),
		captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1, State: "running"},
	}
	h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), withBackend("podman"), confirmStored)
	defer h.stop()

	r := h.firstResult(t)
	if r.Status != agentproto.InstanceStatusFailed {
		t.Fatalf("result = %+v, want failed", r)
	}
	for _, want := range []string{"without a GPU", "sparky-agent check-gpu", "podman"} {
		if !strings.Contains(r.ErrorMessage, want) {
			t.Errorf("error message %q does not mention %q", r.ErrorMessage, want)
		}
	}
	waitFor(t, "container removal", func() bool { return h.removeCount() == 1 })

	ev := h.snapshotEvents()
	if si, vi, hi := indexOf(ev, "start:new-1"), indexOf(ev, "verify:new-1"), indexOf(ev, "halt:new-1"); !(si >= 0 && si < vi && vi < hi) {
		t.Errorf("events = %v, want start, verify, then halt", ev)
	}
	// The guard fails before the readiness check ever polls the engine.
	rt.mu.Lock()
	polled := len(rt.isRunningCalls)
	rt.mu.Unlock()
	if polled != 0 {
		t.Errorf("IsRunning was polled %d times; the guard must fail the launch before readiness", polled)
	}
}

// The request the guard checks is the one the backend was configured for.
func TestLoad_GuardChecksTheMechanismOfTheBackend(t *testing.T) {
	for backend, want := range map[string]agentruntime.GPUDeviceMechanism{
		"podman": agentruntime.GPUDeviceMechanismCDI,
		"docker": agentruntime.GPUDeviceMechanismNvidia,
	} {
		t.Run(backend, func(t *testing.T) {
			rt := &fakeRuntimeBackend{verifyGPUErr: fmt.Errorf("%w: gone", agentruntime.ErrNoGPU), captureResult: agentruntime.Capture{Log: "x\n", LinesKept: 1}}
			h := newArchiveHarnessFor(t, rt, loadEnvelope(t, ""), withBackend(backend), confirmStored)
			defer h.stop()
			h.firstResult(t)
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.verifyGPUMechanisms) != 1 || rt.verifyGPUMechanisms[0] != want {
				t.Errorf("verified mechanisms = %v, want [%s]", rt.verifyGPUMechanisms, want)
			}
		})
	}
}

// "I could not check" is not "there is no GPU": a daemon that failed the
// inspect must not fail a launch that may be fine.
func TestLoad_UnverifiableGPUDoesNotFailTheLaunch(t *testing.T) {
	_, port := newFakeEngineServer(t)
	env, _ := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID: "new-1", ModelRef: "org/model", EngineType: "vllm", Image: "img", Port: port, RequiresFullGPUResidency: true,
	})
	rt := &fakeRuntimeBackend{isRunningResult: true, verifyGPUErr: errors.New("daemon went away")}
	h := newArchiveHarnessFor(t, rt, env, func(c *Config) { c.RuntimeBackend = "podman"; c.InstanceStartupTimeout = 5 * time.Second }, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusRunning {
		t.Fatalf("result = %+v, want running", r)
	}
}

// A container that has its GPU launches as before.
func TestLoad_ContainerWithItsGPULaunches(t *testing.T) {
	_, port := newFakeEngineServer(t)
	env, _ := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID: "new-1", ModelRef: "org/model", EngineType: "vllm", Image: "img", Port: port, RequiresFullGPUResidency: true,
	})
	rt := &fakeRuntimeBackend{isRunningResult: true}
	h := newArchiveHarnessFor(t, rt, env, func(c *Config) { c.RuntimeBackend = "docker"; c.InstanceStartupTimeout = 5 * time.Second }, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusRunning {
		t.Fatalf("result = %+v, want running", r)
	}
	if ev := h.snapshotEvents(); indexOf(ev, "verify:new-1") < 0 {
		t.Errorf("events = %v, want the GPU to have been verified", ev)
	}
}

// Bare-metal has no passthrough boundary: nothing to verify.
func TestLoad_BareMetalIsNotGPUVerified(t *testing.T) {
	_, port := newFakeEngineServer(t)
	env, _ := agentproto.NewEnvelope(agentproto.TypeLoadInstance, "", agentproto.LoadInstance{
		InstanceID: "new-1", ModelRef: "org/model", EngineType: "vllm", Image: "img", Port: port, RequiresFullGPUResidency: true,
	})
	rt := &fakeRuntimeBackend{isRunningResult: true, verifyGPUErr: fmt.Errorf("%w: would fail", agentruntime.ErrNoGPU)}
	h := newArchiveHarnessFor(t, rt, env, func(c *Config) { c.RuntimeBackend = "bare-metal"; c.InstanceStartupTimeout = 5 * time.Second }, confirmStored)
	defer h.stop()

	if r := h.firstResult(t); r.Status != agentproto.InstanceStatusRunning {
		t.Fatalf("result = %+v, want running", r)
	}
	if ev := h.snapshotEvents(); indexOf(ev, "verify:new-1") >= 0 {
		t.Errorf("events = %v, bare-metal must not be GPU-verified", ev)
	}
}
