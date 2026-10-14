// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// probeBackend stands in for a container runtime; only the calls gpucheck
// makes are implemented.
type probeBackend struct {
	runtime.Backend
	startErr error
	logs     string
	closed   bool
}

func (p *probeBackend) Start(context.Context, runtime.Spec) (string, error) { return "id", p.startErr }
func (p *probeBackend) Stop(context.Context, string) error                  { return nil }
func (p *probeBackend) IsRunning(context.Context, string) (bool, error)     { return false, nil }
func (p *probeBackend) Logs(context.Context, string, int) (string, error)   { return p.logs, nil }
func (p *probeBackend) Close() error                                        { p.closed = true; return nil }

type factoryCall struct{ backend, dockerHost string }

func runCheck(t *testing.T, args []string, env map[string]string, be *probeBackend) (code int, out, errOut string, calls []factoryCall) {
	t.Helper()
	var so, se bytes.Buffer
	getenv := func(k string) string { return env[k] }
	factory := func(backend, host string) (runtime.Backend, error) {
		calls = append(calls, factoryCall{backend, host})
		if be == nil {
			return nil, nil
		}
		return be, nil
	}
	code = checkGPU(args, getenv, factory, &so, &se)
	return code, so.String(), se.String(), calls
}

func envFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckGPU_OK(t *testing.T) {
	be := &probeBackend{logs: "GPU 0: NVIDIA GeForce RTX 4090 Laptop GPU (UUID: x)\n"}
	code, out, errOut, calls := runCheck(t, []string{"--env-file", envFile(t, "")}, map[string]string{"SPARKY_RUNTIME_BACKEND": "podman", "DOCKER_HOST": "unix:///run/podman/podman.sock"}, be)

	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, want := range []string{"OK:", "1 GPU", "GPU 0: NVIDIA GeForce RTX 4090", "podman", "unix:///run/podman/podman.sock"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(calls) != 1 || calls[0] != (factoryCall{"podman", "unix:///run/podman/podman.sock"}) {
		t.Errorf("backend factory calls = %v", calls)
	}
	if !be.closed {
		t.Error("the backend was not closed")
	}
}

// With nothing in the environment, the settings come from secrets.env - the
// whole point of running it by hand as the service account.
func TestCheckGPU_ReadsTheNodeSettingsFromSecretsEnv(t *testing.T) {
	file := envFile(t, "SPARKY_RUNTIME_BACKEND=podman\nDOCKER_HOST=unix:///run/user/990/podman/podman.sock\nSPARKY_GPU_CHECK_IMAGE=registry.example/probe:1\n")
	be := &probeBackend{logs: "GPU 0: x\n"}
	code, out, _, calls := runCheck(t, []string{"--env-file", file}, nil, be)

	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if len(calls) != 1 || calls[0] != (factoryCall{"podman", "unix:///run/user/990/podman/podman.sock"}) {
		t.Errorf("backend factory calls = %v, want the values from the file", calls)
	}
	if !strings.Contains(out, "registry.example/probe:1") {
		t.Errorf("the probe image from the file was not used:\n%s", out)
	}
}

func TestCheckGPU_EnvironmentWinsOverTheFile(t *testing.T) {
	file := envFile(t, "SPARKY_RUNTIME_BACKEND=docker\n")
	be := &probeBackend{logs: "GPU 0: x\n"}
	_, _, _, calls := runCheck(t, []string{"--env-file", file}, map[string]string{"SPARKY_RUNTIME_BACKEND": "podman"}, be)
	if len(calls) != 1 || calls[0].backend != "podman" {
		t.Errorf("backend factory calls = %v, want podman from the environment", calls)
	}
}

func TestCheckGPU_FlagImageWinsOverEverything(t *testing.T) {
	file := envFile(t, "SPARKY_GPU_CHECK_IMAGE=from-file\n")
	be := &probeBackend{logs: "GPU 0: x\n"}
	_, out, _, _ := runCheck(t, []string{"--env-file", file, "--image", "from-flag"}, map[string]string{"SPARKY_RUNTIME_BACKEND": "docker", "SPARKY_GPU_CHECK_IMAGE": "from-env"}, be)
	if !strings.Contains(out, "from-flag") || strings.Contains(out, "from-env") || strings.Contains(out, "from-file") {
		t.Errorf("output does not use only the flag image:\n%s", out)
	}
}

func TestCheckGPU_FailureExplainsAndExitsOne(t *testing.T) {
	be := &probeBackend{startErr: errors.New("create container: podman create: unresolvable CDI devices nvidia.com/gpu=all")}
	code, out, _, _ := runCheck(t, []string{"--env-file", envFile(t, "")}, map[string]string{"SPARKY_RUNTIME_BACKEND": "podman"}, be)

	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{"FAILED:", "unresolvable CDI devices", "hint:", "nvidia-ctk cdi generate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckGPU_BareMetalIsNotApplicableAndNeedsNoBackend(t *testing.T) {
	code, out, _, calls := runCheck(t, []string{"--env-file", envFile(t, "")}, map[string]string{"SPARKY_RUNTIME_BACKEND": "bare-metal"}, nil)
	if code != 0 || !strings.Contains(out, "Not applicable") {
		t.Errorf("exit = %d, output:\n%s", code, out)
	}
	if len(calls) != 1 || calls[0].backend != "bare-metal" {
		t.Errorf("factory calls = %v", calls)
	}
}

func TestCheckGPU_ConfigurationProblemsExitTwoWithAReason(t *testing.T) {
	cases := map[string]struct {
		args    []string
		env     map[string]string
		wantErr string
	}{
		"no backend anywhere":         {[]string{"--env-file", "/nonexistent/secrets.env"}, nil, "SPARKY_RUNTIME_BACKEND is not set"},
		"unknown backend":             {[]string{"--env-file", "/nonexistent/secrets.env"}, map[string]string{"SPARKY_RUNTIME_BACKEND": "lxc"}, "unknown SPARKY_RUNTIME_BACKEND"},
		"unreadable file, no backend": {[]string{"--env-file", t.TempDir()}, nil, "service account"},
		"bad flag":                    {[]string{"--no-such-flag"}, nil, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, errOut, calls := runCheck(t, c.args, c.env, nil)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if c.wantErr != "" && !strings.Contains(errOut, c.wantErr) {
				t.Errorf("stderr %q lacks %q", errOut, c.wantErr)
			}
			if len(calls) != 0 {
				t.Errorf("a backend was created for an unusable configuration: %v", calls)
			}
		})
	}
}

func TestCheckGPU_BackendCreationFailure(t *testing.T) {
	var so, se bytes.Buffer
	code := checkGPU([]string{"--env-file", envFile(t, "")}, func(k string) string {
		return map[string]string{"SPARKY_RUNTIME_BACKEND": "docker"}[k]
	}, func(string, string) (runtime.Backend, error) { return nil, errors.New("no socket") }, &so, &se)
	if code != 2 || !strings.Contains(se.String(), "no socket") {
		t.Errorf("exit = %d, stderr = %q", code, se.String())
	}
}
