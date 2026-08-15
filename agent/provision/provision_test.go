// SPDX-License-Identifier: AGPL-3.0-or-later

package provision

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type call struct {
	name string
	args []string
}

// fakeRunner records every call and lets a test script per-command results
// via fn - same pattern as agent/runtime/containers's fakeDockerClient.
type fakeRunner struct {
	calls []call
	fn    func(name string, args []string) error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, call{name: name, args: args})
	if f.fn == nil {
		return nil
	}
	return f.fn(name, args)
}

func TestEnsureServiceloopUser_CreatesWhenMissing(t *testing.T) {
	fake := &fakeRunner{fn: func(name string, _ []string) error {
		if name == "id" {
			return errors.New("no such user")
		}
		return nil
	}}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureServiceloopUser(context.Background()); err != nil {
		t.Fatalf("EnsureServiceloopUser() error: %v", err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("calls = %v, want 2 (id, then useradd)", fake.calls)
	}
	if fake.calls[0].name != "id" {
		t.Errorf("calls[0].name = %q, want %q", fake.calls[0].name, "id")
	}
	want := []string{"--system", "--no-create-home", "--home-dir", serviceloopHome, "--shell", "/usr/sbin/nologin", "serviceloop"}
	if fake.calls[1].name != "useradd" || !reflect.DeepEqual(fake.calls[1].args, want) {
		t.Errorf("calls[1] = %+v, want useradd %v", fake.calls[1], want)
	}
}

func TestEnsureServiceloopUser_NoOpWhenExists(t *testing.T) {
	fake := &fakeRunner{}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureServiceloopUser(context.Background()); err != nil {
		t.Fatalf("EnsureServiceloopUser() error: %v", err)
	}

	if len(fake.calls) != 1 || fake.calls[0].name != "id" {
		t.Errorf("calls = %v, want exactly one `id` call and no useradd", fake.calls)
	}
}

func TestEnsureServiceloopUser_UseraddFails_ReturnsError(t *testing.T) {
	fake := &fakeRunner{fn: func(name string, _ []string) error {
		if name == "id" {
			return errors.New("no such user")
		}
		if name == "useradd" {
			return errors.New("permission denied")
		}
		return nil
	}}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureServiceloopUser(context.Background()); err == nil {
		t.Fatal("EnsureServiceloopUser() succeeded despite a useradd failure")
	}
}

func TestEnsureModelStorageDir_RunsInstall(t *testing.T) {
	fake := &fakeRunner{}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureModelStorageDir(context.Background()); err != nil {
		t.Fatalf("EnsureModelStorageDir() error: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("calls = %v, want 1", fake.calls)
	}
	want := []string{"-d", "-o", "serviceloop", "-g", "serviceloop", "-m", "0750", serviceloopHome}
	if fake.calls[0].name != "install" || !reflect.DeepEqual(fake.calls[0].args, want) {
		t.Errorf("calls[0] = %+v, want install %v", fake.calls[0], want)
	}
}

func TestEnsureModelStorageDir_Fails_ReturnsError(t *testing.T) {
	fake := &fakeRunner{fn: func(string, []string) error { return errors.New("no space left on device") }}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureModelStorageDir(context.Background()); err == nil {
		t.Fatal("EnsureModelStorageDir() succeeded despite an install failure")
	}
}

func TestEnsureGPUGroupMembership_JoinsExistingGroups(t *testing.T) {
	fake := &fakeRunner{fn: func(name string, args []string) error {
		if name == "getent" && len(args) == 2 && args[1] == "render" {
			return errors.New("no such group")
		}
		return nil
	}}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureGPUGroupMembership(context.Background()); err != nil {
		t.Fatalf("EnsureGPUGroupMembership() error: %v", err)
	}

	var usermodCalls []call
	for _, c := range fake.calls {
		if c.name == "usermod" {
			usermodCalls = append(usermodCalls, c)
		}
	}
	if len(usermodCalls) != 1 {
		t.Fatalf("usermod calls = %v, want exactly 1 (video only, render doesn't exist)", usermodCalls)
	}
	want := []string{"-aG", "video", "serviceloop"}
	if !reflect.DeepEqual(usermodCalls[0].args, want) {
		t.Errorf("usermod args = %v, want %v", usermodCalls[0].args, want)
	}
}

func TestEnsureGPUGroupMembership_BothGroupsExist_JoinsBoth(t *testing.T) {
	fake := &fakeRunner{}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureGPUGroupMembership(context.Background()); err != nil {
		t.Fatalf("EnsureGPUGroupMembership() error: %v", err)
	}

	var usermodCalls []call
	for _, c := range fake.calls {
		if c.name == "usermod" {
			usermodCalls = append(usermodCalls, c)
		}
	}
	if len(usermodCalls) != 2 {
		t.Fatalf("usermod calls = %v, want 2 (both video and render exist)", usermodCalls)
	}
}

func TestEnsureGPUGroupMembership_NoGroupsExist_NoOp(t *testing.T) {
	fake := &fakeRunner{fn: func(name string, _ []string) error {
		if name == "getent" {
			return errors.New("no such group")
		}
		return nil
	}}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureGPUGroupMembership(context.Background()); err != nil {
		t.Fatalf("EnsureGPUGroupMembership() error: %v", err)
	}

	for _, c := range fake.calls {
		if c.name == "usermod" {
			t.Errorf("unexpected usermod call %+v - no GPU group exists on this host", c)
		}
	}
}

func TestEnsureGPUGroupMembership_UsermodFails_ReturnsError(t *testing.T) {
	fake := &fakeRunner{fn: func(name string, _ []string) error {
		if name == "usermod" {
			return errors.New("permission denied")
		}
		return nil
	}}
	p := &Provisioner{run: fake.run}

	if err := p.EnsureGPUGroupMembership(context.Background()); err == nil {
		t.Fatal("EnsureGPUGroupMembership() succeeded despite a usermod failure")
	}
}
