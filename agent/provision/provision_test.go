// SPDX-License-Identifier: AGPL-3.0-or-later

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestEnsureSSHKeypair_GeneratesWhenMissing(t *testing.T) {
	fake := &fakeRunner{}
	p := &Provisioner{run: fake.run, sshKeyPath: DefaultSSHKeyPath}

	if err := p.EnsureSSHKeypair(context.Background()); err != nil {
		t.Fatalf("EnsureSSHKeypair() error: %v", err)
	}

	var names []string
	for _, c := range fake.calls {
		names = append(names, c.name)
	}
	if want := []string{"install", "ssh-keygen", "chown", "chown", "chmod"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("commands = %v, want %v", names, want)
	}
	wantKeygen := []string{"-q", "-t", "ed25519", "-N", "", "-C", "sparky-agent", "-f", DefaultSSHKeyPath}
	if !reflect.DeepEqual(fake.calls[1].args, wantKeygen) {
		t.Errorf("ssh-keygen args = %v, want %v", fake.calls[1].args, wantKeygen)
	}
	if fake.calls[0].args[len(fake.calls[0].args)-2] != "0700" {
		t.Errorf("ssh dir mode args = %v, want 0700", fake.calls[0].args)
	}
}

func TestReadSSHPublicKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got := ReadSSHPublicKey(write("a.pub", "ssh-ed25519 AAAAC3Nza comment here\n")); got != "ssh-ed25519 AAAAC3Nza" {
		t.Errorf("comment not stripped: %q", got)
	}
	if got := ReadSSHPublicKey(write("b.pub", "# note\nssh-ed25519 AAAAB\n")); got != "ssh-ed25519 AAAAB" {
		t.Errorf("comment line not skipped: %q", got)
	}
	if got := ReadSSHPublicKey(write("empty.pub", "")); got != "" {
		t.Errorf("empty file = %q, want empty", got)
	}
	if got := ReadSSHPublicKey(filepath.Join(dir, "missing.pub")); got != "" {
		t.Errorf("missing file = %q, want empty", got)
	}
}

func TestEnsureSSHKeypair_NeverOverwritesExistingKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRunner{}
	p := &Provisioner{run: fake.run, sshKeyPath: keyPath}

	if err := p.EnsureSSHKeypair(context.Background()); err != nil {
		t.Fatalf("EnsureSSHKeypair() error: %v", err)
	}
	for _, c := range fake.calls {
		if c.name == "ssh-keygen" {
			t.Fatal("ssh-keygen ran despite an existing private key - identity would be rotated")
		}
	}
}

func peerProvisioner(t *testing.T, fn func(name string, args []string) error) (*Provisioner, *fakeRunner, string) {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeRunner{fn: fn}
	return &Provisioner{run: fake.run, grantDir: "/grants", usedDir: "/used", sshdDropIn: filepath.Join(dir, "50-sparky-peer.conf")}, fake, dir
}

func cmdNames(f *fakeRunner) []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.name)
	}
	return out
}

func TestEnsurePeerAccess_CreatesEverythingOnAFreshHost(t *testing.T) {
	p, fake, _ := peerProvisioner(t, func(name string, _ []string) error {
		if name == "id" {
			return errors.New("no such user")
		}
		return nil
	})
	if err := p.EnsurePeerAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"install", "id", "useradd", "usermod", "install", "install", "sshd", "systemctl"}; !reflect.DeepEqual(cmdNames(fake), want) {
		t.Fatalf("commands = %v, want %v", cmdNames(fake), want)
	}
	wantAdd := []string{"--system", "--user-group", "--no-create-home", "--home-dir", "/var/lib/sparky-peer", "--shell", "/bin/sh", "sparky-peer"}
	if !reflect.DeepEqual(fake.calls[2].args, wantAdd) {
		t.Errorf("useradd args = %v, want %v", fake.calls[1].args, wantAdd)
	}
	wantInstall := []string{"-d", "-o", "serviceloop", "-g", "sparky-peer", "-m", "2750", "/grants"}
	if !reflect.DeepEqual(fake.calls[4].args, wantInstall) {
		t.Errorf("install args = %v, want %v", fake.calls[3].args, wantInstall)
	}
	wantUsed := []string{"-d", "-o", "sparky-peer", "-g", "serviceloop", "-m", "2770", "/used"}
	if !reflect.DeepEqual(fake.calls[5].args, wantUsed) {
		t.Errorf("used-dir install args = %v, want %v", fake.calls[4].args, wantUsed)
	}
	written, err := os.ReadFile(p.sshdDropIn)
	if err != nil || string(written) != sshdDropIn {
		t.Errorf("drop-in not written as expected: %v", err)
	}
}

func TestSSHDDropIn_ScopedToPeerUserAndLocked(t *testing.T) {
	// Every directive must sit inside the Match block, and the hardening
	// directives must be present - a regression here silently widens what
	// the account can do.
	head, block, found := strings.Cut(sshdDropIn, "Match User sparky-peer\n")
	if !found {
		t.Fatal("no Match User sparky-peer block")
	}
	for _, line := range strings.Split(head, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			t.Errorf("directive %q outside the Match block would apply to every account", l)
		}
	}
	for _, want := range []string{
		"AuthorizedKeysFile none", "AuthorizedKeysCommand /opt/sparky/bin/sparky-agent peer-authkeys %u",
		"AuthorizedKeysCommandUser serviceloop", "AuthenticationMethods publickey", "PasswordAuthentication no",
		"KbdInteractiveAuthentication no", "AllowTcpForwarding no", "AllowAgentForwarding no", "X11Forwarding no",
		"PermitTTY no", "PermitUserRC no", "AllowStreamLocalForwarding no", "PermitTunnel no", "GatewayPorts no",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("drop-in is missing %q", want)
		}
	}
	if strings.Contains(sshdDropIn, "ForceCommand") {
		t.Error("ForceCommand would override the per-transfer forced command")
	}
}

func TestEnsurePeerAccess_IdempotentWhenUpToDate(t *testing.T) {
	p, fake, _ := peerProvisioner(t, nil)
	if err := os.WriteFile(p.sshdDropIn, []byte(sshdDropIn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsurePeerAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.calls {
		if c.name == "useradd" || c.name == "sshd" || c.name == "systemctl" {
			t.Errorf("%s ran although the account exists and the drop-in is current", c.name)
		}
	}
}

func TestEnsurePeerAccess_SSHDRejectionRollsBack(t *testing.T) {
	p, _, _ := peerProvisioner(t, func(name string, _ []string) error {
		if name == "sshd" {
			return errors.New("bad config")
		}
		return nil
	})
	if err := p.EnsurePeerAccess(context.Background()); err == nil {
		t.Fatal("want an error when sshd rejects the config")
	}
	if _, err := os.Stat(p.sshdDropIn); !errors.Is(err, os.ErrNotExist) {
		t.Error("a rejected drop-in must be removed so sshd is never left broken")
	}

	p2, _, _ := peerProvisioner(t, func(name string, _ []string) error {
		if name == "sshd" {
			return errors.New("bad config")
		}
		return nil
	})
	if err := os.WriteFile(p2.sshdDropIn, []byte("# previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = p2.EnsurePeerAccess(context.Background())
	if got, _ := os.ReadFile(p2.sshdDropIn); string(got) != "# previous\n" {
		t.Errorf("previous drop-in not restored, got %q", got)
	}
}

func TestEnsurePeerAccess_MissingDropInDirIsAClearError(t *testing.T) {
	p, _, dir := peerProvisioner(t, nil)
	p.sshdDropIn = filepath.Join(dir, "missing", "50-sparky-peer.conf")
	err := p.EnsurePeerAccess(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Include") {
		t.Errorf("error = %v, want one that explains the missing drop-in directory", err)
	}
}
