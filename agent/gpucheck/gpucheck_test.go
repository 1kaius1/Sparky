// SPDX-License-Identifier: AGPL-3.0-or-later

package gpucheck

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// fakeBackend scripts one probe container's life.
type fakeBackend struct {
	mu sync.Mutex

	startErr   error
	startSpecs []runtime.Spec
	// runningFor is how many IsRunning calls report true before the container
	// "exits"; negative means it never exits.
	runningFor int
	isRunErr   error
	logs       string
	logsErr    error
	verifyErr  error
	noVerifier bool

	isRunCalls int
	stopCalls  []string
	verified   []runtime.GPUDeviceMechanism
}

func (f *fakeBackend) Start(_ context.Context, spec runtime.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startSpecs = append(f.startSpecs, spec)
	return "id", f.startErr
}
func (f *fakeBackend) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, id)
	return nil
}
func (f *fakeBackend) IsRunning(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isRunCalls++
	if f.isRunErr != nil {
		return false, f.isRunErr
	}
	return f.runningFor < 0 || f.isRunCalls <= f.runningFor, nil
}
func (f *fakeBackend) Logs(context.Context, string, int) (string, error) { return f.logs, f.logsErr }
func (f *fakeBackend) Shutdown(context.Context) error                    { return nil }
func (f *fakeBackend) Halt(context.Context, string) error                { return nil }
func (f *fakeBackend) Remove(context.Context, string) error              { return nil }
func (f *fakeBackend) Capture(context.Context, string, int) (runtime.Capture, error) {
	return runtime.Capture{}, nil
}
func (f *fakeBackend) InstancesForProfile(context.Context, string) ([]string, error) { return nil, nil }

// verifier is the same backend seen as a runtime.GPUVerifier.
type verifyingBackend struct{ *fakeBackend }

func (v verifyingBackend) VerifyGPU(_ context.Context, _ string, m runtime.GPUDeviceMechanism) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.verified = append(v.verified, m)
	return v.verifyErr
}

func fast() Options { return Options{PollInterval: time.Millisecond, Timeout: 2 * time.Second} }

func TestRun_Success_SendsTheLaunchRequestAndCountsGPUs(t *testing.T) {
	fb := &fakeBackend{runningFor: 2, logs: "GPU 0: NVIDIA GeForce RTX 4090 (UUID: x)\nGPU 1: NVIDIA GeForce RTX 4090 (UUID: y)\n"}
	res := Run(context.Background(), verifyingBackend{fb}, "podman", fast())

	if res.Status != StatusOK || res.GPUs != 2 {
		t.Fatalf("result = %+v, want ok with 2 GPUs", res)
	}
	if len(fb.startSpecs) != 1 {
		t.Fatalf("started %d containers, want 1", len(fb.startSpecs))
	}
	spec := fb.startSpecs[0]
	// The probe must send exactly what a launch sends, or it proves nothing.
	mech, cdi, env := runtime.GPURequest("podman")
	if spec.GPUDeviceMechanism != mech || !reflect.DeepEqual(spec.CDIDevices, cdi) || !reflect.DeepEqual(spec.Env, env) {
		t.Errorf("spec GPU request = %q %v %v, want the launch request %q %v %v", spec.GPUDeviceMechanism, spec.CDIDevices, spec.Env, mech, cdi, env)
	}
	if !spec.Probe || !reflect.DeepEqual(spec.Args, []string{"nvidia-smi", "-L"}) || spec.Image != DefaultImage {
		t.Errorf("spec = %+v, want a probe running nvidia-smi -L in the default image", spec)
	}
	if !strings.HasPrefix(spec.InstanceID, "gpucheck-") {
		t.Errorf("instance id %q, want a gpucheck- prefix", spec.InstanceID)
	}
	if len(fb.verified) != 1 || fb.verified[0] != runtime.GPUDeviceMechanismCDI {
		t.Errorf("verified = %v, want the CDI mechanism checked once", fb.verified)
	}
	if len(fb.stopCalls) != 1 || fb.stopCalls[0] != spec.InstanceID {
		t.Errorf("stop calls = %v, want the probe removed", fb.stopCalls)
	}
}

func TestRun_UsesTheConfiguredImage(t *testing.T) {
	fb := &fakeBackend{logs: "GPU 0: x\n"}
	opts := fast()
	opts.Image = "registry.example/probe:1"
	Run(context.Background(), fb, "docker", opts)
	if got := fb.startSpecs[0].Image; got != "registry.example/probe:1" {
		t.Errorf("image = %q", got)
	}
	mech, _, env := runtime.GPURequest("docker")
	if fb.startSpecs[0].GPUDeviceMechanism != mech || !reflect.DeepEqual(fb.startSpecs[0].Env, env) {
		t.Errorf("docker probe did not send the docker launch request")
	}
}

func TestRun_BareMetalIsNotApplicableAndStartsNothing(t *testing.T) {
	fb := &fakeBackend{}
	for _, backend := range []string{"bare-metal", "", "unknown"} {
		res := Run(context.Background(), fb, backend, fast())
		if res.Status != StatusNotApplicable || res.Detail == "" {
			t.Errorf("%q: result = %+v, want not applicable", backend, res)
		}
	}
	if len(fb.startSpecs) != 0 {
		t.Errorf("started %d containers for backends with no passthrough", len(fb.startSpecs))
	}
	// A nil backend is fine when nothing is going to run.
	if res := Run(context.Background(), nil, "bare-metal", fast()); res.Status != StatusNotApplicable {
		t.Errorf("nil backend: %+v", res)
	}
}

func TestRun_FailureModes(t *testing.T) {
	cases := []struct {
		name      string
		backend   string
		fb        *fakeBackend
		verifier  bool
		wantInDet string
		wantHint  string
	}{
		{
			name: "start fails because the CDI name is unresolvable", backend: "podman",
			fb:        &fakeBackend{startErr: errors.New("create container: podman create: unresolvable CDI devices nvidia.com/gpu=all")},
			wantInDet: "could not start the probe container", wantHint: "nvidia-ctk cdi generate",
		},
		{
			name: "docker has no nvidia runtime", backend: "docker",
			fb:        &fakeBackend{startErr: errors.New(`could not select device driver "nvidia" with capabilities: [[gpu]]`)},
			wantInDet: "could not start", wantHint: "nvidia-container-toolkit",
		},
		{
			name: "image cannot be pulled", backend: "docker",
			fb:        &fakeBackend{startErr: errors.New("pull image x: dial tcp: lookup registry-1.docker.io: no such host")},
			wantInDet: "could not start", wantHint: "SPARKY_GPU_CHECK_IMAGE",
		},
		{
			name: "daemon unreachable", backend: "podman",
			fb:        &fakeBackend{startErr: errors.New("create container: Post \"http://d/containers/create\": dial unix /run/podman/podman.sock: connect: permission denied")},
			wantInDet: "could not start", wantHint: "DOCKER_HOST",
		},
		{
			name: "the daemon dropped the request (podman)", backend: "podman", verifier: true,
			fb:        &fakeBackend{verifyErr: fmt.Errorf("%w: the container has no devices", runtime.ErrNoGPU)},
			wantInDet: "no devices", wantHint: "failed to parse CDI Spec",
		},
		{
			name: "the daemon dropped the request (docker)", backend: "docker", verifier: true,
			fb:        &fakeBackend{verifyErr: fmt.Errorf("%w: the container has no GPU device request - the daemon dropped it", runtime.ErrNoGPU)},
			wantInDet: "dropped", wantHint: "nvidia-container-toolkit",
		},
		{
			name: "container ran but sees no GPU", backend: "podman",
			fb:        &fakeBackend{logs: "sh: 1: nvidia-smi: not found\n"},
			wantInDet: "listed no GPU", wantHint: "without a GPU",
		},
		{
			name: "output with no GPU line and no known cause", backend: "podman",
			fb:        &fakeBackend{logs: "something unexpected\n"},
			wantInDet: "listed no GPU", wantHint: "",
		},
		{
			name: "cannot tell whether it finished", backend: "podman",
			fb:        &fakeBackend{isRunErr: errors.New("inspect: boom")},
			wantInDet: "could not tell whether", wantHint: "",
		},
		{
			name: "cannot read the output", backend: "podman",
			fb:        &fakeBackend{logsErr: errors.New("logs: boom")},
			wantInDet: "could not read", wantHint: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var be runtime.Backend = c.fb
			if c.verifier {
				be = verifyingBackend{c.fb}
			}
			res := Run(context.Background(), be, c.backend, fast())
			if res.Status != StatusFailed {
				t.Fatalf("result = %+v, want failed", res)
			}
			if !strings.Contains(res.Detail, c.wantInDet) {
				t.Errorf("detail %q does not contain %q", res.Detail, c.wantInDet)
			}
			if c.wantHint == "" && res.Hint != "" {
				t.Errorf("hint = %q, want none (no guessing)", res.Hint)
			}
			if c.wantHint != "" && !strings.Contains(res.Hint, c.wantHint) {
				t.Errorf("hint %q does not contain %q", res.Hint, c.wantHint)
			}
			// However it failed, the probe container is removed.
			if len(c.fb.startSpecs) == 1 && len(c.fb.stopCalls) != 1 {
				t.Errorf("stop calls = %v, want the probe removed", c.fb.stopCalls)
			}
		})
	}
}

// A probe that never finishes is abandoned at the timeout - and still removed.
func TestRun_TimeoutStillRemovesTheProbe(t *testing.T) {
	fb := &fakeBackend{runningFor: -1}
	res := Run(context.Background(), fb, "podman", Options{PollInterval: time.Millisecond, Timeout: 50 * time.Millisecond})
	if res.Status != StatusFailed || !strings.Contains(res.Detail, "did not finish") {
		t.Fatalf("result = %+v, want a timeout failure", res)
	}
	if len(fb.stopCalls) != 1 {
		t.Errorf("stop calls = %v, want the probe removed even after the timeout", fb.stopCalls)
	}
}

// Non-verdict verifier errors are not failures: the output decides.
func TestRun_UnverifiableGPUFallsBackToTheProbeOutput(t *testing.T) {
	fb := &fakeBackend{logs: "GPU 0: x\n", verifyErr: errors.New("inspect failed")}
	if res := Run(context.Background(), verifyingBackend{fb}, "podman", fast()); res.Status != StatusOK {
		t.Errorf("result = %+v, want ok", res)
	}
}

func TestHintFor_RecognisesOnlyWhatItKnows(t *testing.T) {
	if h := hintFor("podman", "completely unrelated failure"); h != "" {
		t.Errorf("hint for an unknown failure = %q, want none", h)
	}
	if h := hintFor("podman", "UNRESOLVABLE CDI DEVICES nvidia.com/gpu=all"); h == "" {
		t.Error("matching must be case-insensitive")
	}
}
