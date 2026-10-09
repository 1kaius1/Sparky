// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
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

	logsResult client.ContainerLogsResult
	logsErr    error

	// listResult / listErr answer ContainerList; the default (empty) means
	// "no container carries the label", which exercises the legacy-name
	// fallback. listOptions records every list request.
	listResult  client.ContainerListResult
	listErr     error
	listOptions []client.ContainerListOptions

	// The container id or name each call was addressed with.
	stopRefs, removeRefs, inspectRefs, logsRefs []string
}

func (f *fakeDockerClient) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.createCalls++
	return f.createFunc(ctx, options)
}

func (f *fakeDockerClient) ContainerStart(_ context.Context, _ string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, f.startErr
}

func (f *fakeDockerClient) ContainerStop(_ context.Context, ref string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.stopRefs = append(f.stopRefs, ref)
	return client.ContainerStopResult{}, f.stopErr
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, ref string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removeRefs = append(f.removeRefs, ref)
	return client.ContainerRemoveResult{}, f.removeErr
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, ref string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.inspectRefs = append(f.inspectRefs, ref)
	return f.inspectResult, f.inspectErr
}

func (f *fakeDockerClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.listOptions = append(f.listOptions, options)
	return f.listResult, f.listErr
}

func (f *fakeDockerClient) ImagePull(_ context.Context, _ string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.pullCalls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return &fakePullResponse{}, nil
}

func (f *fakeDockerClient) ContainerLogs(_ context.Context, ref string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.logsRefs = append(f.logsRefs, ref)
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.logsResult != nil {
		return f.logsResult, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
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
		Image:              "example/engine:latest",
		GPUDeviceMechanism: runtime.GPUDeviceMechanismCDI,
		CDIDevices:         []string{"nvidia.com/gpu=all"},
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

func TestStart_SetsNvidiaDeviceRequest(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{
		Image:              "example/engine:latest",
		GPUDeviceMechanism: runtime.GPUDeviceMechanismNvidia,
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
	if reqs[0].Driver != nvidiaDriver {
		t.Errorf("Driver = %q, want %q", reqs[0].Driver, nvidiaDriver)
	}
	if reqs[0].Count != -1 {
		t.Errorf("Count = %d, want -1 (all)", reqs[0].Count)
	}
	if len(reqs[0].Capabilities) != 1 || len(reqs[0].Capabilities[0]) != 1 || reqs[0].Capabilities[0][0] != "gpu" {
		t.Errorf("Capabilities = %v, want [[gpu]]", reqs[0].Capabilities)
	}
}

func TestStart_NoMechanism_NoDeviceRequest(t *testing.T) {
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
		t.Errorf("DeviceRequests = %v, want empty when Spec.GPUDeviceMechanism is unset", captured.HostConfig.DeviceRequests)
	}
}

func TestStart_CDIMechanism_NoCDIDevices_NoDeviceRequest(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	_, err := b.Start(context.Background(), runtime.Spec{
		Image:              "example/image:latest",
		GPUDeviceMechanism: runtime.GPUDeviceMechanismCDI,
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if len(captured.HostConfig.DeviceRequests) != 0 {
		t.Errorf("DeviceRequests = %v, want empty when Spec.CDIDevices is empty even with the CDI mechanism selected", captured.HostConfig.DeviceRequests)
	}
}

func TestStart_SetsShmSizeAndIPCMode(t *testing.T) {
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}

	const shmSize = 16 * 1024 * 1024 * 1024
	_, err := b.Start(context.Background(), runtime.Spec{
		Image:   "example/engine:latest",
		ShmSize: shmSize,
		IPCMode: "host",
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if captured.HostConfig.ShmSize != shmSize {
		t.Errorf("ShmSize = %d, want %d", captured.HostConfig.ShmSize, shmSize)
	}
	if captured.HostConfig.IpcMode != container.IPCModeHost {
		t.Errorf("IpcMode = %q, want %q", captured.HostConfig.IpcMode, container.IPCModeHost)
	}
}

func TestStart_NoShmSizeOrIPCMode_LeavesHostConfigDefaults(t *testing.T) {
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
	if captured.HostConfig.ShmSize != 0 {
		t.Errorf("ShmSize = %d, want 0", captured.HostConfig.ShmSize)
	}
	if captured.HostConfig.IpcMode != "" {
		t.Errorf("IpcMode = %q, want empty", captured.HostConfig.IpcMode)
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

func startCapturing(t *testing.T, spec runtime.Spec) client.ContainerCreateOptions {
	t.Helper()
	var captured client.ContainerCreateOptions
	fake := &fakeDockerClient{
		createFunc: func(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			captured = options
			return client.ContainerCreateResult{ID: "container-1"}, nil
		},
	}
	b := &Backend{cli: fake}
	if _, err := b.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	return captured
}

func TestStart_UsesTheDescriptiveNameFromTheCentralApp(t *testing.T) {
	got := startCapturing(t, runtime.Spec{InstanceID: "instance-1", Image: "img", ContainerName: "sparky-Qwen3-8B-20261008-100459"})
	if got.Name != "sparky-Qwen3-8B-20261008-100459" {
		t.Errorf("Name = %q, want the name from the central app", got.Name)
	}
}

func TestStart_FallsBackToTheLegacyNameWhenNoneOrInvalidIsGiven(t *testing.T) {
	legacy := InstanceContainerName("instance-1")
	for _, name := range []string{
		"",                       // an older server sends none
		"has space",              // not a valid container name
		"-leading-dash",          // must start alphanumeric
		"slash/inside",           // path-like
		"semi;colon",             // junk from the wire
		strings.Repeat("a", 129), // over the length cap
		"caf\u00e9",              // non-ASCII
	} {
		got := startCapturing(t, runtime.Spec{InstanceID: "instance-1", Image: "img", ContainerName: name})
		if got.Name != legacy {
			t.Errorf("ContainerName %q: Name = %q, want the legacy %q", name, got.Name, legacy)
		}
	}
}

func TestStart_LabelsTheContainerWithTheInstanceID(t *testing.T) {
	got := startCapturing(t, runtime.Spec{InstanceID: "instance-1", Image: "img", ContainerName: "sparky-p-20261008-100459"})
	if got.Config.Labels[labelInstanceID] != "instance-1" || got.Config.Labels[labelManaged] != "true" {
		t.Errorf("Labels = %v, want the instance-id and managed labels", got.Config.Labels)
	}
}

func TestInstanceContainerName(t *testing.T) {
	got := InstanceContainerName("instance-1")
	want := "sparky-instance-instance-1"
	if got != want {
		t.Errorf("InstanceContainerName() = %q, want %q", got, want)
	}
}

func labelled(id string, created int64) container.Summary {
	return container.Summary{ID: id, Created: created}
}

// Every lookup must go to the container found by label, using its id - the
// descriptive name cannot be recomputed from the instance id.
func TestLookups_FindTheContainerByLabel(t *testing.T) {
	fake := &fakeDockerClient{
		listResult:    client.ContainerListResult{Items: []container.Summary{labelled("abc123", 100)}},
		inspectResult: client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: true}}},
	}
	b := &Backend{cli: fake}
	ctx := context.Background()

	if err := b.Stop(ctx, "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if running, err := b.IsRunning(ctx, "instance-1"); err != nil || !running {
		t.Fatalf("IsRunning() = %v, %v, want true", running, err)
	}
	if _, err := b.Logs(ctx, "instance-1", 10); err != nil {
		t.Fatalf("Logs() error: %v", err)
	}

	for name, refs := range map[string][]string{"stop": fake.stopRefs, "remove": fake.removeRefs, "inspect": fake.inspectRefs, "logs": fake.logsRefs} {
		if len(refs) != 1 || refs[0] != "abc123" {
			t.Errorf("%s addressed %v, want [abc123] (the label match's id)", name, refs)
		}
	}
	for i, opts := range fake.listOptions {
		if !opts.All {
			t.Errorf("list %d: All = false; a stopped or exited container must still be found", i)
		}
		if !opts.Filters["label"][labelInstanceID+"=instance-1"] {
			t.Errorf("list %d: filters = %v, want label %s=instance-1", i, opts.Filters, labelInstanceID)
		}
	}
}

// A container started by an older agent has no label; it must stay
// manageable under its legacy name after an upgrade.
func TestLookups_FallBackToTheLegacyNameWhenNoLabelMatches(t *testing.T) {
	fake := &fakeDockerClient{
		inspectResult: client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: true}}},
	}
	b := &Backend{cli: fake}
	ctx := context.Background()
	legacy := InstanceContainerName("instance-1")

	if err := b.Stop(ctx, "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if _, err := b.IsRunning(ctx, "instance-1"); err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if _, err := b.Logs(ctx, "instance-1", 10); err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	for name, refs := range map[string][]string{"stop": fake.stopRefs, "remove": fake.removeRefs, "inspect": fake.inspectRefs, "logs": fake.logsRefs} {
		if len(refs) != 1 || refs[0] != legacy {
			t.Errorf("%s addressed %v, want [%s]", name, refs, legacy)
		}
	}
}

func TestLookups_PreferTheNewestWhenSeveralCarryTheLabel(t *testing.T) {
	fake := &fakeDockerClient{listResult: client.ContainerListResult{Items: []container.Summary{
		labelled("old", 100), labelled("newest", 300), labelled("middle", 200),
	}}}
	b := &Backend{cli: fake}
	if err := b.Stop(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if len(fake.stopRefs) != 1 || fake.stopRefs[0] != "newest" {
		t.Errorf("stop addressed %v, want [newest]", fake.stopRefs)
	}
}

// A failed lookup is reported as such, and nothing is stopped or removed on
// a guess.
func TestLookups_ListFailureIsReportedAndActsOnNothing(t *testing.T) {
	fake := &fakeDockerClient{listErr: errors.New("daemon unreachable")}
	b := &Backend{cli: fake}
	ctx := context.Background()

	if err := b.Stop(ctx, "instance-1"); err == nil || !strings.Contains(err.Error(), "daemon unreachable") {
		t.Errorf("Stop() error = %v, want the list failure", err)
	}
	if _, err := b.IsRunning(ctx, "instance-1"); err == nil {
		t.Error("IsRunning() succeeded despite a failed lookup")
	}
	if _, err := b.Logs(ctx, "instance-1", 10); err == nil {
		t.Error("Logs() succeeded despite a failed lookup")
	}
	if len(fake.stopRefs)+len(fake.removeRefs)+len(fake.inspectRefs)+len(fake.logsRefs) != 0 {
		t.Errorf("a failed lookup must not act on any container: %v %v %v %v", fake.stopRefs, fake.removeRefs, fake.inspectRefs, fake.logsRefs)
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

// A container already removed (by hand, a Docker data wipe) is the state
// Stop is trying to reach; failing would leave a dead instance impossible to
// unload.
func TestStop_ContainerAlreadyGone_IsNotAnError(t *testing.T) {
	gone := cerrdefs.ErrNotFound.WithMessage("no such container")
	for name, fake := range map[string]*fakeDockerClient{
		"stop reports not found":   {stopErr: gone},
		"remove reports not found": {removeErr: gone},
		"both report not found":    {stopErr: gone, removeErr: gone},
	} {
		t.Run(name, func(t *testing.T) {
			b := &Backend{cli: fake}
			if err := b.Stop(context.Background(), "instance-1"); err != nil {
				t.Fatalf("Stop() error: %v, want nil", err)
			}
		})
	}
}

// An exited container still has to be removed: Stop must not skip the
// removal just because there was nothing running to stop.
func TestStop_ExitedContainer_IsStillRemoved(t *testing.T) {
	fake := &fakeDockerClient{}
	b := &Backend{cli: fake}

	if err := b.Stop(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if len(fake.removeRefs) != 1 {
		t.Errorf("remove called %d times, want 1", len(fake.removeRefs))
	}
}

func TestStop_NotFoundIsTheOnlyErrorTolerated(t *testing.T) {
	b := &Backend{cli: &fakeDockerClient{stopErr: cerrdefs.ErrPermissionDenied.WithMessage("denied")}}
	if err := b.Stop(context.Background(), "instance-1"); err == nil {
		t.Fatal("Stop() swallowed a non-not-found error")
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

// A container that no longer exists is not running. Reporting an error
// instead would leave the instance "running" in the central app forever.
func TestIsRunning_ContainerGone_ReportsNotRunning(t *testing.T) {
	b := &Backend{cli: &fakeDockerClient{inspectErr: cerrdefs.ErrNotFound.WithMessage("no such container")}}

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v, want nil for a container that is gone", err)
	}
	if running {
		t.Error("IsRunning() = true for a container that is gone")
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

// stdcopyFrame builds one Docker multiplexed-stream frame - see
// client.ContainerLogs' own doc comment for the exact wire format this
// hand-builds, since the stdcopy package exports a demultiplexer
// (stdcopy.StdCopy) but no corresponding multiplexing writer to build a
// fake stream with.
func stdcopyFrame(streamType byte, payload string) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, []byte(payload)...)
}

func TestLogs_DemuxesStdoutAndStderr(t *testing.T) {
	var stream []byte
	stream = append(stream, stdcopyFrame(1, "starting up\n")...)
	stream = append(stream, stdcopyFrame(2, "a warning on stderr\n")...)
	fake := &fakeDockerClient{logsResult: io.NopCloser(bytes.NewReader(stream))}
	b := &Backend{cli: fake}

	logs, err := b.Logs(context.Background(), "instance-1", 100)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	if !strings.Contains(logs, "starting up") || !strings.Contains(logs, "a warning on stderr") {
		t.Errorf("Logs() = %q, want both the stdout and stderr lines demultiplexed into it", logs)
	}
}

func TestLogs_ContainerLogsError(t *testing.T) {
	fake := &fakeDockerClient{logsErr: errors.New("no such container")}
	b := &Backend{cli: fake}

	_, err := b.Logs(context.Background(), "instance-1", 100)
	if err == nil {
		t.Fatal("Logs() succeeded despite a ContainerLogs failure")
	}
}
