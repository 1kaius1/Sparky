// SPDX-License-Identifier: AGPL-3.0-or-later

// Package nodes is the Node registry - see ARCHITECTURE.md Node & Fabric
// Registry and SCHEMA.md Nodes. It never accesses the database directly
// (see CLAUDE.md); the validation in this file is pure, and Service in
// service.go is the thin orchestration layer that persists a validated,
// permitted registration via internal/db.
package nodes

import (
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrInvalidNode is returned when RegisterNodeParams fails validation -
// wrapped with a specific reason, so callers can render it directly.
var ErrInvalidNode = errors.New("invalid node")

// RegisterNodeParams is the input to Service.RegisterNode.
type RegisterNodeParams struct {
	Name           string
	Hostname       string
	IPAddress      string
	RuntimeBackend db.RuntimeBackend
	GPUMemoryGB    float64
	CPUMemoryGB    float64
}

// validate checks RegisterNodeParams against SCHEMA.md Nodes' invariants.
func (p RegisterNodeParams) validate() error {
	if p.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidNode)
	}
	if p.Hostname == "" {
		return fmt.Errorf("%w: hostname is required", ErrInvalidNode)
	}
	if p.IPAddress == "" {
		return fmt.Errorf("%w: ip_address is required", ErrInvalidNode)
	}

	switch p.RuntimeBackend {
	case db.RuntimeBackendDocker, db.RuntimeBackendPodman, db.RuntimeBackendBareMetal:
	default:
		return fmt.Errorf("%w: unknown runtime_backend %q", ErrInvalidNode, p.RuntimeBackend)
	}

	if p.GPUMemoryGB <= 0 {
		return fmt.Errorf("%w: gpu_memory_gb must be positive", ErrInvalidNode)
	}
	if p.CPUMemoryGB <= 0 {
		return fmt.Errorf("%w: cpu_memory_gb must be positive", ErrInvalidNode)
	}

	return nil
}
