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
// serviceloop system account, its model storage home directory, and its
// GPU-passthrough group membership. Idempotent and safe to re-run - see
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

	fmt.Println()
	fmt.Println("Setup complete.")
}
