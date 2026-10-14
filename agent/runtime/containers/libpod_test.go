// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// fakeLibpod records native create requests and answers from a script.
type fakeLibpod struct {
	reqs    []libpodCreateRequest
	results []error // one per call; a nil entry succeeds
	id      string
}

func (f *fakeLibpod) createContainer(_ context.Context, req libpodCreateRequest) (string, error) {
	f.reqs = append(f.reqs, req)
	if i := len(f.reqs) - 1; i < len(f.results) && f.results[i] != nil {
		return "", f.results[i]
	}
	return f.id, nil
}

// compatMustNotCreate is a Docker client whose create fails the test: a CDI
// GPU request must never go through the compat API, which drops it silently.
func compatMustNotCreate(t *testing.T) *fakeDockerClient {
	t.Helper()
	return &fakeDockerClient{
		createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			t.Error("the compat API was used to create a container that needs a CDI GPU")
			return client.ContainerCreateResult{ID: "compat-should-not-be-used"}, nil
		},
	}
}

func cdiSpec() runtime.Spec {
	return runtime.Spec{
		InstanceID:         "instance-1",
		Image:              "example/engine:latest",
		GPUDeviceMechanism: runtime.GPUDeviceMechanismCDI,
		CDIDevices:         []string{"nvidia.com/gpu=all"},
	}
}

func TestStart_CDI_CreatesThroughTheNativeAPIAndStartsByItsID(t *testing.T) {
	compat := compatMustNotCreate(t)
	pod := &fakeLibpod{id: "pod-container-1"}
	b := &Backend{cli: compat, libpod: pod}

	spec := cdiSpec()
	spec.ContainerName = "sparky-my-profile-20261013-120000"
	spec.ProfileID = "11111111-2222-3333-4444-555555555555"
	spec.Args = []string{"vllm", "serve", "/models/m"}
	got, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if got != "pod-container-1" {
		t.Errorf("Start() id = %q, want the id the native API returned", got)
	}
	if compat.createCalls != 0 {
		t.Errorf("compat create called %d times, want 0", compat.createCalls)
	}
	if len(pod.reqs) != 1 {
		t.Fatalf("native create called %d times, want 1", len(pod.reqs))
	}
	req := pod.reqs[0]
	if !reflect.DeepEqual(req.Devices, []libpodDevice{{Path: "nvidia.com/gpu=all"}}) {
		t.Errorf("Devices = %+v, want the CDI name", req.Devices)
	}
	if req.Name != "sparky-my-profile-20261013-120000" || req.Image != "example/engine:latest" || !reflect.DeepEqual(req.Command, spec.Args) {
		t.Errorf("name/image/command = %q %q %v, want them carried over", req.Name, req.Image, req.Command)
	}
	// The labels are how every later call finds the container again.
	if req.Labels[labelInstanceID] != "instance-1" || req.Labels[labelManaged] != "true" || req.Labels[labelProfileID] != spec.ProfileID {
		t.Errorf("labels = %v, want instance, managed and profile labels", req.Labels)
	}
	// The rest of the lifecycle runs on the compat API, addressed by that id.
	if !reflect.DeepEqual(compat.startRefs, []string{"pod-container-1"}) {
		t.Errorf("compat start refs = %v, want [pod-container-1]", compat.startRefs)
	}
}

func TestStart_CDI_MissingImageIsPulledThenRetried(t *testing.T) {
	compat := compatMustNotCreate(t)
	pod := &fakeLibpod{id: "pod-container-1", results: []error{cerrdefs.ErrNotFound.WithMessage("image not known")}}
	b := &Backend{cli: compat, libpod: pod}

	if _, err := b.Start(context.Background(), cdiSpec()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if compat.pullCalls != 1 || len(pod.reqs) != 2 {
		t.Errorf("pulls = %d, native creates = %d, want 1 pull and 2 creates", compat.pullCalls, len(pod.reqs))
	}
}

func TestStart_CDI_CreateErrorIsReportedAndNothingIsStarted(t *testing.T) {
	compat := compatMustNotCreate(t)
	b := &Backend{cli: compat, libpod: &fakeLibpod{results: []error{errors.New("unresolvable CDI devices nvidia.com/gpu=all")}}}

	_, err := b.Start(context.Background(), cdiSpec())
	if err == nil || !strings.Contains(err.Error(), "unresolvable CDI devices") {
		t.Fatalf("Start() error = %v, want the daemon's reason", err)
	}
	if len(compat.startRefs) != 0 {
		t.Errorf("a container was started after a failed create: %v", compat.startRefs)
	}
}

func TestStart_CDI_WithoutANativeClientFailsClearly(t *testing.T) {
	b := &Backend{cli: compatMustNotCreate(t)}
	if _, err := b.Start(context.Background(), cdiSpec()); err == nil {
		t.Fatal("Start() succeeded with no way to create a CDI container")
	}
}

// The Docker mechanism and a GPU-less launch must keep using the compat API.
func TestStart_NonCDI_StillUsesTheCompatAPI(t *testing.T) {
	for name, spec := range map[string]runtime.Spec{
		"docker nvidia":         {Image: "x", GPUDeviceMechanism: runtime.GPUDeviceMechanismNvidia},
		"no gpu":                {Image: "x"},
		"cdi with no device ID": {Image: "x", GPUDeviceMechanism: runtime.GPUDeviceMechanismCDI},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeDockerClient{createFunc: func(_ context.Context, _ client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
				return client.ContainerCreateResult{ID: "c1"}, nil
			}}
			pod := &fakeLibpod{}
			b := &Backend{cli: fake, libpod: pod}
			if _, err := b.Start(context.Background(), spec); err != nil {
				t.Fatalf("Start() error: %v", err)
			}
			if fake.createCalls != 1 || len(pod.reqs) != 0 {
				t.Errorf("compat creates = %d, native creates = %d, want 1 and 0", fake.createCalls, len(pod.reqs))
			}
		})
	}
}

func TestBuildLibpodRequest_Translation(t *testing.T) {
	spec := runtime.Spec{
		Image:      "example/engine:latest",
		Args:       []string{"serve", "--port", "8000"},
		Env:        []string{"NVIDIA_VISIBLE_DEVICES=all", "EMPTY=", "WITH_EQ=a=b"},
		Mounts:     []string{"/srv/models:/srv/models:ro", "/data:/data", "/x:/y:ro,z"},
		Port:       8000,
		CDIDevices: []string{"nvidia.com/gpu=all"},
		ShmSize:    2 << 30,
		IPCMode:    "private",
	}
	req, err := buildLibpodRequest(spec, "name-1", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("buildLibpodRequest() error: %v", err)
	}

	wantEnv := map[string]string{"NVIDIA_VISIBLE_DEVICES": "all", "EMPTY": "", "WITH_EQ": "a=b"}
	if !reflect.DeepEqual(req.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", req.Env, wantEnv)
	}
	wantMounts := []libpodMount{
		{Type: "bind", Source: "/srv/models", Destination: "/srv/models", Options: []string{"ro"}},
		{Type: "bind", Source: "/data", Destination: "/data"},
		{Type: "bind", Source: "/x", Destination: "/y", Options: []string{"ro", "z"}},
	}
	if !reflect.DeepEqual(req.Mounts, wantMounts) {
		t.Errorf("Mounts = %+v, want %+v", req.Mounts, wantMounts)
	}
	if !reflect.DeepEqual(req.PortMappings, []libpodPortMapping{{HostPort: 8000, ContainerPort: 8000, Protocol: "tcp"}}) {
		t.Errorf("PortMappings = %+v", req.PortMappings)
	}
	if req.ShmSize != 2<<30 || req.IPCNS == nil || req.IPCNS.NSMode != "private" {
		t.Errorf("shm/ipc = %d %+v, want 2GiB and private", req.ShmSize, req.IPCNS)
	}
	if req.Name != "name-1" || req.Labels["k"] != "v" {
		t.Errorf("name/labels = %q %v", req.Name, req.Labels)
	}
}

// Podman refuses a shm size together with the host IPC namespace, and the
// agent sends both for a vLLM profile with shm_size_gb.
func TestBuildLibpodRequest_HostIPCDropsTheShmSize(t *testing.T) {
	req, err := buildLibpodRequest(runtime.Spec{Image: "x", ShmSize: 2 << 30, IPCMode: "host"}, "n", nil)
	if err != nil {
		t.Fatalf("buildLibpodRequest() error: %v", err)
	}
	if req.ShmSize != 0 {
		t.Errorf("ShmSize = %d, want it dropped under host IPC", req.ShmSize)
	}
	if req.IPCNS == nil || req.IPCNS.NSMode != "host" {
		t.Errorf("IPCNS = %+v, want host", req.IPCNS)
	}
}

func TestBuildLibpodRequest_RejectsMalformedInput(t *testing.T) {
	for name, spec := range map[string]runtime.Spec{
		"env without equals":    {Image: "x", Env: []string{"JUSTAKEY"}},
		"env with empty key":    {Image: "x", Env: []string{"=v"}},
		"mount with one part":   {Image: "x", Mounts: []string{"/only"}},
		"mount with empty path": {Image: "x", Mounts: []string{":/dst"}},
		"mount with four parts": {Image: "x", Mounts: []string{"/a:/b:ro:z"}},
		"port out of range":     {Image: "x", Port: 70000},
		"unsupported ipc mode":  {Image: "x", IPCMode: "container:abc"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildLibpodRequest(spec, "n", nil); err == nil {
				t.Error("buildLibpodRequest() accepted malformed input")
			}
		})
	}
}

// libpodServer answers Podman's create endpoint.
func libpodServer(t *testing.T, status int, body string, capture *struct {
	method, path, contentType string
	req                       libpodCreateRequest
}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture.method, capture.path, capture.contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
			json.NewDecoder(r.Body).Decode(&capture.req)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestLibpodClient_CreateSuccess(t *testing.T) {
	var got struct {
		method, path, contentType string
		req                       libpodCreateRequest
	}
	srv := libpodServer(t, 201, `{"Id":"abc123","Warnings":[]}`, &got)
	defer srv.Close()
	c := newLibpodClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	id, err := c.createContainer(context.Background(), libpodCreateRequest{Image: "img", Devices: []libpodDevice{{Path: "nvidia.com/gpu=all"}}})
	if err != nil || id != "abc123" {
		t.Fatalf("createContainer() = %q, %v, want abc123", id, err)
	}
	if got.method != http.MethodPost || got.path != "/"+libpodAPIVersion+"/libpod/containers/create" || got.contentType != "application/json" {
		t.Errorf("request = %s %s (%s), want a JSON POST to the versioned create endpoint", got.method, got.path, got.contentType)
	}
	if got.req.Image != "img" || len(got.req.Devices) != 1 || got.req.Devices[0].Path != "nvidia.com/gpu=all" {
		t.Errorf("decoded body = %+v", got.req)
	}
}

func TestLibpodClient_ErrorMapping(t *testing.T) {
	t.Run("unknown image is not-found so Start can pull", func(t *testing.T) {
		srv := libpodServer(t, 404, `{"cause":"image not known","message":"no such image: x: image not known","response":404}`, nil)
		defer srv.Close()
		_, err := newLibpodClient("tcp://"+strings.TrimPrefix(srv.URL, "http://")).createContainer(context.Background(), libpodCreateRequest{Image: "x"})
		if !cerrdefs.IsNotFound(err) {
			t.Errorf("error = %v, want a not-found error", err)
		}
	})
	t.Run("other failures carry the daemon's message", func(t *testing.T) {
		srv := libpodServer(t, 500, `{"cause":"x","message":"unresolvable CDI devices nvidia.com/gpu=all","response":500}`, nil)
		defer srv.Close()
		_, err := newLibpodClient("tcp://"+strings.TrimPrefix(srv.URL, "http://")).createContainer(context.Background(), libpodCreateRequest{Image: "x"})
		if err == nil || cerrdefs.IsNotFound(err) || !strings.Contains(err.Error(), "unresolvable CDI devices") {
			t.Errorf("error = %v, want the daemon's message and not not-found", err)
		}
	})
	t.Run("a plain 404 means the endpoint is missing, not the image", func(t *testing.T) {
		srv := libpodServer(t, 404, `Not Found`, nil)
		defer srv.Close()
		_, err := newLibpodClient("tcp://"+strings.TrimPrefix(srv.URL, "http://")).createContainer(context.Background(), libpodCreateRequest{Image: "x"})
		if err == nil || cerrdefs.IsNotFound(err) || !strings.Contains(err.Error(), "is it Podman") {
			t.Errorf("error = %v, want a not-Podman hint that must not trigger a pull", err)
		}
	})
	t.Run("a success with no id is an error", func(t *testing.T) {
		srv := libpodServer(t, 201, `{}`, nil)
		defer srv.Close()
		if _, err := newLibpodClient("tcp://"+strings.TrimPrefix(srv.URL, "http://")).createContainer(context.Background(), libpodCreateRequest{Image: "x"}); err == nil {
			t.Error("createContainer() accepted a response with no id")
		}
	})
}

// The socket address is how rootless and rootful Podman are reached.
func TestLibpodClient_OverAUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "podman.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"over-unix"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	id, err := newLibpodClient("unix://"+sock).createContainer(context.Background(), libpodCreateRequest{Image: "x"})
	if err != nil || id != "over-unix" {
		t.Fatalf("createContainer() = %q, %v, want over-unix", id, err)
	}
	if _, statErr := os.Stat(sock); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestLibpodClient_UnsupportedAddressesFailWhenUsed(t *testing.T) {
	for _, host := range []string{"ssh://user@host", "npipe:////./pipe/docker_engine", "::bad::"} {
		c := newLibpodClient(host)
		if _, err := c.createContainer(context.Background(), libpodCreateRequest{Image: "x"}); err == nil {
			t.Errorf("createContainer() against %q succeeded, want an error", host)
		}
	}
}
