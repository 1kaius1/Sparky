// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provision performs idempotent local OS-level setup for a
// bare-metal sparky-agent install - the serviceloop system account, its
// model storage home directory, and GPU-passthrough group membership. See
// cmd/sparky-agent's `setup` subcommand, the only caller of this package -
// PLANNING.md's 2026-08-07 Decisions Log entry explains why this logic
// lives in the agent binary rather than only in
// scripts/packaging/lib/agent-common.sh (bash), which it replaces: real
// go test coverage, and one implementation instead of duplicating
// useradd/usermod handling across three install methods.
package provision

import (
	"context"
	"fmt"
	"os/exec"
)

// runner is a fakeable seam for shelling out - same pattern as
// agent/telemetry's commandRunner, narrowed to just error (exit status)
// since nothing here needs to parse stdout.
type runner func(ctx context.Context, name string, args ...string) error

func runCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// serviceloopHome is serviceloop's home directory - deliberately under
// /opt/sparky rather than the useradd default of /home/serviceloop, since
// the systemd unit's ProtectHome=true makes /home/* inaccessible to the
// running process regardless of whether the directory exists there.
const serviceloopHome = "/opt/sparky/serviceloop"

// Provisioner performs the provisioning steps below. Requires root - see
// cmd/sparky-agent's own explicit check before constructing one. Not safe
// for concurrent use (mirrors telemetry.Collector) - not a concern in
// practice, since a `setup` invocation only ever runs these once, in order.
type Provisioner struct {
	run runner
}

// New constructs a Provisioner that shells out for real.
func New() *Provisioner {
	return &Provisioner{run: runCommand}
}

// EnsureServiceloopUser creates the serviceloop system account if it
// doesn't already exist - idempotent, safe to call on every install and
// upgrade.
func (p *Provisioner) EnsureServiceloopUser(ctx context.Context) error {
	if err := p.run(ctx, "id", "-u", "serviceloop"); err == nil {
		return nil
	}
	if err := p.run(ctx, "useradd", "--system", "--no-create-home", "--home-dir", serviceloopHome, "--shell", "/usr/sbin/nologin", "serviceloop"); err != nil {
		return fmt.Errorf("useradd serviceloop: %w", err)
	}
	return nil
}

// EnsureModelStorageDir creates serviceloop's home directory - also the
// parent of the bare-metal runtime backend's default
// SPARKY_MODEL_STORAGE_PATH (agent/config's bareMetalDefaultModelStoragePath).
// Must run after EnsureServiceloopUser - resolving the directory's
// ownership needs that account to already exist.
func (p *Provisioner) EnsureModelStorageDir(ctx context.Context) error {
	if err := p.run(ctx, "install", "-d", "-o", "serviceloop", "-g", "serviceloop", "-m", "0750", serviceloopHome); err != nil {
		return fmt.Errorf("create %s: %w", serviceloopHome, err)
	}
	return nil
}

// EnsureGPUGroupMembership joins serviceloop to whichever of video/render
// actually exists on this host - distro/driver-dependent, so both are
// attempted rather than guessed; joining a group that turns out to be
// irrelevant is harmless, but silently skipping the one that does matter
// would leave the agent unable to see any GPU device node at all.
func (p *Provisioner) EnsureGPUGroupMembership(ctx context.Context) error {
	for _, grp := range []string{"video", "render"} {
		if err := p.run(ctx, "getent", "group", grp); err != nil {
			continue
		}
		if err := p.run(ctx, "usermod", "-aG", grp, "serviceloop"); err != nil {
			return fmt.Errorf("join serviceloop to group %s: %w", grp, err)
		}
	}
	return nil
}
