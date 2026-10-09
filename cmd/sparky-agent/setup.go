// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/1kaius1/Sparky/agent/provision"
)

// runSetup implements `sparky-agent setup` - creates/verifies the
// serviceloop system account, its model storage home directory, its
// GPU-passthrough group membership, its Docker socket access (docker backend
// only), and its SSH identity. Idempotent and safe to re-run - see
// PLANNING.md's 2026-08-07 Decisions Log entry for why this logic lives in
// the binary rather than only in scripts/packaging/lib/agent-common.sh:
// real go test coverage (see agent/provision), and one implementation
// instead of duplicating useradd/usermod handling across three install
// methods' bash. Called automatically by scripts/packaging/postinstall.sh
// and scripts/install_agent.sh on every install and upgrade - also safe to
// run by hand afterward for diagnostics/repair on an already-provisioned
// node.
func runSetup(logger *log.Logger) {
	if os.Getuid() != 0 {
		logger.Fatalf("setup: must be run as root (sudo)")
	}

	p := provision.New()
	ctx := context.Background()

	fmt.Println("sparky-agent setup")
	fmt.Println("==================")
	fmt.Println()

	if err := p.EnsureServiceloopUser(ctx); err != nil {
		logger.Fatalf("setup: %v", err)
	}
	fmt.Println("serviceloop system account: OK")

	if err := p.EnsureModelStorageDir(ctx); err != nil {
		logger.Fatalf("setup: %v", err)
	}
	fmt.Println("model storage directory (/opt/sparky/serviceloop): OK")

	if err := p.EnsureGPUGroupMembership(ctx); err != nil {
		logger.Fatalf("setup: %v", err)
	}
	fmt.Println("GPU-passthrough group membership (video/render): OK")

	groupResult, err := p.EnsureContainerRuntimeGroupMembership(ctx)
	if err != nil {
		logger.Fatalf("setup: %v", err)
	}
	switch groupResult {
	case provision.ContainerGroupJoined:
		fmt.Println("Docker socket access (serviceloop joined to the docker group): OK")
		fmt.Println("  If sparky-agent is already running, restart it to pick up the new group.")
	case provision.ContainerGroupMissing:
		fmt.Println("Docker socket access: SKIPPED - SPARKY_RUNTIME_BACKEND is docker but this host has no docker group.")
		fmt.Println("  Install Docker, then re-run: sudo sparky-agent setup")
	default:
		fmt.Println("Docker socket access: not needed yet - SPARKY_RUNTIME_BACKEND is not set to docker.")
		fmt.Println("  If this node uses the docker backend, set it in /etc/sparky-agent/secrets.env,")
		fmt.Println("  then re-run: sudo sparky-agent setup")
	}

	if err := p.EnsureSSHKeypair(ctx); err != nil {
		logger.Fatalf("setup: %v", err)
	}
	fmt.Println("SSH identity for peer-to-peer model transfer (/opt/sparky/serviceloop/.ssh): OK")

	if err := p.EnsurePeerAccess(ctx); err != nil {
		logger.Fatalf("setup: %v", err)
	}
	fmt.Println("peer-transfer account, grant directory and sshd drop-in (sparky-peer): OK")

	fmt.Println()
	fmt.Println("Setup complete.")
}
