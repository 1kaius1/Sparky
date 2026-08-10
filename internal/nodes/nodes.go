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
	Name             string
	Hostname         string
	IPAddress        string
	NodeType         db.NodeType
	ContainerRuntime *db.ContainerRuntime
	GPUMemoryGB      float64
	CPUMemoryGB      float64
}

// validate checks RegisterNodeParams against SCHEMA.md Nodes' invariants.
// It duplicates the database's nodes_container_runtime_matches_type CHECK
// constraint deliberately: failing fast here gives a specific error
// message instead of an opaque constraint-violation error surfaced from
// Postgres.
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

	switch p.NodeType {
	case db.NodeTypeSpark:
		if p.ContainerRuntime != nil {
			return fmt.Errorf("%w: container_runtime must be unset for a %s node", ErrInvalidNode, db.NodeTypeSpark)
		}
	case db.NodeTypeDockerGPU:
		if p.ContainerRuntime == nil {
			return fmt.Errorf("%w: container_runtime is required for a %s node", ErrInvalidNode, db.NodeTypeDockerGPU)
		}
	default:
		return fmt.Errorf("%w: unknown node_type %q", ErrInvalidNode, p.NodeType)
	}

	if p.GPUMemoryGB <= 0 {
		return fmt.Errorf("%w: gpu_memory_gb must be positive", ErrInvalidNode)
	}
	if p.CPUMemoryGB <= 0 {
		return fmt.Errorf("%w: cpu_memory_gb must be positive", ErrInvalidNode)
	}

	return nil
}
