// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"fmt"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// nodeStore is the subset of *db.NodeRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/rbac's userStore.
type nodeStore interface {
	Create(ctx context.Context, name, hostname, ipAddress string, nodeType db.NodeType, containerRuntime *db.ContainerRuntime, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string, bearerTokenHash string) (*db.Node, error)
}

// tokenGenerator is the subset of internal/auth's node token helpers this
// package needs, narrow enough to fake in tests so a unit test never
// depends on a real random source.
type tokenGenerator func() (string, error)

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
	nodes         nodeStore
	audit         auditRecorder
	generateToken tokenGenerator
}

// NewService constructs a Service, generating each node's bearer token
// with internal/auth.GenerateNodeToken.
func NewService(nodes nodeStore, audit auditRecorder) *Service {
	return &Service{nodes: nodes, audit: audit, generateToken: auth.GenerateNodeToken}
}

// RegisterNode registers a new compute node, if actor is permitted to -
// see rbac.CanManageNodes. A permitted registration is always audited
// ("registered_node" - see SCHEMA.md Audit log) after it persists,
// including when actor is the SuperAdmin - see ARCHITECTURE.md's "no
// exceptions" audit guarantee.
//
// The returned bearer token is plaintext and shown here only once - only
// its hash is persisted (SCHEMA.md Nodes' bearer_token_hash). The caller
// is responsible for surfacing it to the Admin (e.g. for
// SPARKY_BEARER_TOKEN, per docs/AGENT.md Configuration) and must not log
// or store it anywhere else.
func (s *Service) RegisterNode(ctx context.Context, actor rbac.Actor, params RegisterNodeParams) (node *db.Node, bearerToken string, err error) {
	if !rbac.CanManageNodes(actor) {
		return nil, "", rbac.ErrNotPermitted
	}

	if err := params.validate(); err != nil {
		return nil, "", err
	}

	var registeredBy *string
	if !actor.IsSuperAdmin {
		registeredBy = &actor.UserID
	}

	token, err := s.generateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate bearer token: %w", err)
	}

	n, err := s.nodes.Create(ctx, params.Name, params.Hostname, params.IPAddress,
		params.NodeType, params.ContainerRuntime, params.GPUMemoryGB, params.CPUMemoryGB, registeredBy,
		auth.HashNodeToken(token))
	if err != nil {
		return nil, "", fmt.Errorf("create node: %w", err)
	}

	detail := map[string]any{
		"name":      n.Name,
		"node_type": string(n.NodeType),
	}
	if err := s.audit.Record(ctx, registeredBy, actor.IsSuperAdmin, "registered_node", "node", n.ID, detail); err != nil {
		return nil, "", fmt.Errorf("record audit: %w", err)
	}
	return n, token, nil
}
