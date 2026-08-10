// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// nodeStore is the subset of *db.NodeRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/rbac's userStore.
type nodeStore interface {
	Create(ctx context.Context, name, hostname, ipAddress string, nodeType db.NodeType, containerRuntime *db.ContainerRuntime, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string) (*db.Node, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs -
// same pattern as internal/rbac's auditRecorder.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service is the Node registry's orchestration layer: check the rule,
// validate the input, then persist and audit. See CLAUDE.md's Handler ->
// Service Layer -> Repository pattern - callers should never call
// NodeRepository.Create directly; this is the only path a new node
// registration should take.
type Service struct {
	nodes nodeStore
	audit auditRecorder
}

// NewService constructs a Service.
func NewService(nodes nodeStore, audit auditRecorder) *Service {
	return &Service{nodes: nodes, audit: audit}
}

// RegisterNode registers a new compute node, if actor is permitted to -
// see rbac.CanManageNodes. A permitted registration is always audited
// ("registered_node" - see SCHEMA.md Audit log) after it persists,
// including when actor is the SuperAdmin - see ARCHITECTURE.md's "no
// exceptions" audit guarantee.
func (s *Service) RegisterNode(ctx context.Context, actor rbac.Actor, params RegisterNodeParams) (*db.Node, error) {
	if !rbac.CanManageNodes(actor) {
		return nil, rbac.ErrNotPermitted
	}

	if err := params.validate(); err != nil {
		return nil, err
	}

	var registeredBy *string
	if !actor.IsSuperAdmin {
		registeredBy = &actor.UserID
	}

	n, err := s.nodes.Create(ctx, params.Name, params.Hostname, params.IPAddress,
		params.NodeType, params.ContainerRuntime, params.GPUMemoryGB, params.CPUMemoryGB, registeredBy)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}

	detail := map[string]any{
		"name":      n.Name,
		"node_type": string(n.NodeType),
	}
	if err := s.audit.Record(ctx, registeredBy, actor.IsSuperAdmin, "registered_node", "node", n.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return n, nil
}
