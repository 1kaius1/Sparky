// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"context"
	"errors"
	"io"
	"iter"
	"reflect"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// fakeDockerClient implements dockerClient for tests without a real
// daemon - see this package's live verification against real Podman,
// documented in Backend's doc comment, for what a fake can't tell us.
type fakeDockerClient struct {
	createCalls int
	createFunc  func(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)

	startErr  error
	stopErr   error
	removeErr error

	inspectResult client.ContainerInspectResult
	inspectErr    error

	pullCalls int
	pullErr   error
}

func (f *fakeDockerClient) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.createCalls++
	return f.createFunc(ctx, options)
}

func (f *fakeDockerClient) ContainerStart(_ context.Context, _ string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, f.startErr
}

func (f *fakeDockerClient) ContainerStop(_ context.Context, _ string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, f.stopErr
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, f.removeErr
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return f.inspectResult, f.inspectErr
}

func (f *fakeDockerClient) ImagePull(_ context.Context, _ string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.pullCalls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return &fakePullResponse{}, nil
}

func (f *fakeDockerClient) Close() error { return nil }

// fakePullResponse implements client.ImagePullResponse. Start's pullImage
// only ever calls Wait, so the rest are unused no-ops.
type fakePullResponse struct{}

func (f *fakePullResponse) Read(_ []byte) (int, error)   { return 0, io.EOF }
func (f *fakePullResponse) Close() error                 { return nil }
func (f *fakePullResponse) Wait(_ context.Context) error { return nil }
func (f *fakePullResponse) JSONMessages(_ context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {}
}

func newImageNotFoundErr() error {
	return cerrdefs.ErrNotFound.WithMessage("no such image")
}

func TestStart_Success(t *testing.T) {
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	id, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if id != "container-1" {
		t.Errorf("id = %q, want %q", id, "container-1")
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (image was present, no pull should happen)", fake.createCalls)
	}
	if fake.pullCalls != 0 {
		t.Errorf("pullCalls = %d, want 0", fake.pullCalls)
	}
}

func TestStart_PullsMissingImageThenRetries(t *testing.T) {
	firstAttempt := true
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			if firstAttempt {
				firstAttempt = false
				return client.ContainerCreateResult{}, newImageNotFoundErr()
			}
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	id, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if id != "container-1" {
		t.Errorf("id = %q, want %q", id, "container-1")
	}
	if fake.createCalls != 2 {
		t.Errorf("createCalls = %d, want 2 (initial not-found, then retry after pull)", fake.createCalls)
	}
	if fake.pullCalls != 1 {
		t.Errorf("pullCalls = %d, want 1", fake.pullCalls)
	}
}

func TestStart_PullFails(t *testing.T) {
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{}, newImageNotFoundErr()
		},
		pullErr: errors.New("registry unreachable"),
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err == nil {
		t.Fatal("Start() succeeded despite a pull failure")
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no retry after a failed pull)", fake.createCalls)
	}
}

func TestStart_CreateFails_NonNotFound_NoPullAttempted(t *testing.T) {
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{}, errors.New("invalid container config")
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err == nil {
		t.Fatal("Start() succeeded despite a create failure")
	}
	if fake.pullCalls != 0 {
		t.Errorf("pullCalls = %d, want 0 - a non-not-found error must not trigger a pull", fake.pullCalls)
	}
}

func TestStart_StartFails(t *testing.T) {
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
		startErr: errors.New("start failed"),
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err == nil {
		t.Fatal("Start() succeeded despite a start failure")
	}
}

func TestStart_SetsCDIDeviceRequest(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{
		Image:      "example/engine:latest",
		CDIDevices: []string{"nvidia.com/gpu=all"},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if captured.HostConfig == nil {
		t.Fatal("HostConfig is nil")
	}
	reqs := captured.HostConfig.DeviceRequests
	if len(reqs) != 1 {
		t.Fatalf("DeviceRequests = %v, want exactly 1 entry", reqs)
	}
	if reqs[0].Driver != cdiDriver {
		t.Errorf("Driver = %q, want %q", reqs[0].Driver, cdiDriver)
	}
	if len(reqs[0].DeviceIDs) != 1 || reqs[0].DeviceIDs[0] != "nvidia.com/gpu=all" {
		t.Errorf("DeviceIDs = %v, want [nvidia.com/gpu=all]", reqs[0].DeviceIDs)
	}
}

func TestStart_NoCDIDevices_NoDeviceRequest(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if len(captured.HostConfig.DeviceRequests) != 0 {
		t.Errorf("DeviceRequests = %v, want empty when Spec.CDIDevices is empty", captured.HostConfig.DeviceRequests)
	}
}

func TestStart_SetsArgsAsCmd(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{
		Image: "example/engine:latest",
		Args:  []string{"--model", "/models/repo/model.gguf", "--port", "8000"},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	want := []string{"--model", "/models/repo/model.gguf", "--port", "8000"}
	if !reflect.DeepEqual(captured.Config.Cmd, want) {
		t.Errorf("Cmd = %v, want %v", captured.Config.Cmd, want)
	}
}

func TestStart_SetsPortBinding(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/engine:latest", Port: 8000})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	port := network.MustParsePort("8000/tcp")
	if _, ok := captured.Config.ExposedPorts[port]; !ok {
		t.Errorf("ExposedPorts = %v, want an entry for %v", captured.Config.ExposedPorts, port)
	}
	bindings := captured.HostConfig.PortBindings[port]
	if len(bindings) != 1 || bindings[0].HostPort != "8000" {
		t.Errorf("PortBindings[%v] = %v, want a single binding with HostPort=8000", port, bindings)
	}
}

func TestStart_NoPort_NoPortBinding(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{Image: "example/image:latest"})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if len(captured.Config.ExposedPorts) != 0 {
		t.Errorf("ExposedPorts = %v, want empty when Spec.Port is zero", captured.Config.ExposedPorts)
	}
	if len(captured.HostConfig.PortBindings) != 0 {
		t.Errorf("PortBindings = %v, want empty when Spec.Port is zero", captured.HostConfig.PortBindings)
	}
}

func TestStart_SetsMounts(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{
		Image:  "example/engine:latest",
		Mounts: []string{"/srv/models:/srv/models:ro"},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	want := []string{"/srv/models:/srv/models:ro"}
	if !reflect.DeepEqual(captured.HostConfig.Binds, want) {
		t.Errorf("Binds = %v, want %v", captured.HostConfig.Binds, want)
	}
}

func TestStart_NamesContainerFromInstanceID(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{InstanceID: "instance-1", Image: "example/engine:latest"})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	want := InstanceContainerName("instance-1")
	if captured.Name != want {
		t.Errorf("Name = %q, want %q", captured.Name, want)
	}
}

func TestInstanceContainerName(t *testing.T) {
	got := InstanceContainerName("instance-1")
	want := "sparky-instance-instance-1"
	if got != want {
		t.Errorf("InstanceContainerName() = %q, want %q", got, want)
	}
}

func TestStop_Success(t *testing.T) {
	fake := &fakeDockerClient{}
	b := &Backend{cli: fake}

	if err := b.Stop(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestStop_StopFails(t *testing.T) {
	fake := &fakeDockerClient{stopErr: errors.New("stop failed")}
	b := &Backend{cli: fake}

	if err := b.Stop(context.Background(), "instance-1"); err == nil {
		t.Fatal("Stop() succeeded despite a stop failure")
	}
}

func TestStop_RemoveFails(t *testing.T) {
	fake := &fakeDockerClient{removeErr: errors.New("remove failed")}
	b := &Backend{cli: fake}

	if err := b.Stop(context.Background(), "instance-1"); err == nil {
		t.Fatal("Stop() succeeded despite a remove failure")
	}
}

func TestShutdown_NoOp(t *testing.T) {
	b := &Backend{cli: &fakeDockerClient{}}

	if err := b.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() error: %v, want nil - containers are deliberately left running", err)
	}
}

func TestIsRunning_True(t *testing.T) {
	fake := &fakeDockerClient{
		inspectResult: client.ContainerInspectResult{
			Container: container.InspectResponse{State: &container.State{Running: true}},
		},
	}
	b := &Backend{cli: fake}

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if !running {
		t.Error("running = false, want true")
	}
}

func TestIsRunning_False(t *testing.T) {
	fake := &fakeDockerClient{
		inspectResult: client.ContainerInspectResult{
			Container: container.InspectResponse{State: &container.State{Running: false}},
		},
	}
	b := &Backend{cli: fake}

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if running {
		t.Error("running = true, want false")
	}
}

func TestIsRunning_NilState(t *testing.T) {
	fake := &fakeDockerClient{
		inspectResult: client.ContainerInspectResult{Container: container.InspectResponse{State: nil}},
	}
	b := &Backend{cli: fake}

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if running {
		t.Error("running = true, want false for a nil State")
	}
}

func TestIsRunning_InspectError(t *testing.T) {
	fake := &fakeDockerClient{inspectErr: errors.New("no such container")}
	b := &Backend{cli: fake}

	_, err := b.IsRunning(context.Background(), "instance-1")
	if err == nil {
		t.Fatal("IsRunning() succeeded despite an inspect failure")
	}
}
