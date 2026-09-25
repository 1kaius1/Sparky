// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// nodeStore is the subset of *db.NodeRepository this package needs,
// narrow enough to fake in tests without a real Postgres instance - same
// pattern as internal/rbac's userStore.
type nodeStore interface {
	Create(ctx context.Context, name, hostname, ipAddress string, runtimeBackend db.RuntimeBackend, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string, bearerTokenHash string) (*db.Node, error)
	List(ctx context.Context) ([]*db.Node, error)
	FindByID(ctx context.Context, id string) (*db.Node, error)
	SetDefaultTransferInterface(ctx context.Context, nodeID string, interfaceName *string) error
}

// interfaceStore is the subset of *db.NodeNetworkInterfaceRepository this
// package needs.
type interfaceStore interface {
	ReplaceForNode(ctx context.Context, nodeID string, interfaces []db.NodeNetworkInterface) error
	ListByNode(ctx context.Context, nodeID string) ([]*db.NodeNetworkInterface, error)
}

// dispatcher is the subset of *agentconn.Registry this package needs to
// reach a node's agent.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
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
	interfaces    interfaceStore
	dispatch      dispatcher
	audit         auditRecorder
	generateToken tokenGenerator
	logger        *log.Logger
}

// NewService constructs a Service, generating each node's bearer token
// with internal/auth.GenerateNodeToken.
// logger is used only by HandleReportInterfaces, which - as an
// agentconn.OnMessageFunc - has no return value to propagate an error
// through.
func NewService(nodes nodeStore, interfaces interfaceStore, dispatch dispatcher, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{nodes: nodes, interfaces: interfaces, dispatch: dispatch, audit: audit, generateToken: auth.GenerateNodeToken, logger: logger}
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
		params.RuntimeBackend, params.GPUMemoryGB, params.CPUMemoryGB, registeredBy,
		auth.HashNodeToken(token))
	if err != nil {
		return nil, "", fmt.Errorf("create node: %w", err)
	}

	detail := map[string]any{
		"name":            n.Name,
		"runtime_backend": string(n.RuntimeBackend),
	}
	if err := s.audit.Record(ctx, registeredBy, actor.IsSuperAdmin, "registered_node", "node", n.ID, detail); err != nil {
		return nil, "", fmt.Errorf("record audit: %w", err)
	}
	return n, token, nil
}

// ListNodes returns every registered node - unguarded by RBAC, since
// viewing the node registry is available at the lowest tier (CLAUDE.md
// Frontend Conventions, Nodes' sidebar tier "Read-only view"). Read/view
// actions are also never audited - see ARCHITECTURE.md Audit Log.
func (s *Service) ListNodes(ctx context.Context) ([]*db.Node, error) {
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}

// ErrNodeNotConnected is returned by RescanInterfaces when the node has no
// live agent connection.
var ErrNodeNotConnected = errors.New("node is not connected")

// ErrUnknownInterface is returned by SetDefaultTransferInterface when the
// named interface is not among those the node currently reports.
var ErrUnknownInterface = errors.New("node does not report that interface")

// GetNode returns one node, or db.ErrNodeNotFound - unguarded, same
// reasoning as ListNodes.
func (s *Service) GetNode(ctx context.Context, id string) (*db.Node, error) {
	n, err := s.nodes.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// ListInterfaces returns a node's currently reported network interfaces -
// unguarded, same reasoning as ListNodes.
func (s *Service) ListInterfaces(ctx context.Context, nodeID string) ([]*db.NodeNetworkInterface, error) {
	ifaces, err := s.interfaces.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list interfaces for node %s: %w", nodeID, err)
	}
	return ifaces, nil
}

const (
	maxReportedInterfaces = 64
	maxInterfaceNameLen   = 64
)

var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:@-]+$`)

// HandleReportInterfaces implements agentconn.OnMessageFunc for
// agentproto.TypeReportInterfaces: an agent-authenticated system report,
// same category as an agent_status update, so unguarded and unaudited
// (nobody "did" anything). nodeID is the sending connection's
// authenticated identity, never a wire value. The report replaces the
// node's whole interface set. Entries are validated because the reported
// IP is later what a peer dials: a name outside a conservative charset or
// an unparseable IP is skipped, duplicates keep the first, and the count
// is capped - one bad entry never discards the rest of the report.
func (s *Service) HandleReportInterfaces(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeReportInterfaces {
		return
	}
	var report agentproto.ReportInterfaces
	if err := env.DecodePayload(&report); err != nil {
		s.logger.Printf("nodes: node %s sent a malformed report_interfaces: %v", nodeID, err)
		return
	}

	seen := make(map[string]bool)
	var valid []db.NodeNetworkInterface
	for _, i := range report.Interfaces {
		if len(valid) >= maxReportedInterfaces {
			s.logger.Printf("nodes: node %s reported more than %d interfaces - ignoring the rest", nodeID, maxReportedInterfaces)
			break
		}
		ip := net.ParseIP(i.IPAddress)
		switch {
		case i.Name == "" || len(i.Name) > maxInterfaceNameLen || !interfaceNamePattern.MatchString(i.Name):
			s.logger.Printf("nodes: node %s reported an interface with an invalid name - skipping it", nodeID)
			continue
		case ip == nil:
			s.logger.Printf("nodes: node %s reported interface %q with an invalid IP - skipping it", nodeID, i.Name)
			continue
		case seen[i.Name]:
			continue
		case i.LinkSpeedMbps != nil && *i.LinkSpeedMbps <= 0:
			i.LinkSpeedMbps = nil
		}
		seen[i.Name] = true
		valid = append(valid, db.NodeNetworkInterface{NodeID: nodeID, InterfaceName: i.Name, IPAddress: ip.String(), LinkSpeedMbps: i.LinkSpeedMbps})
	}

	if err := s.interfaces.ReplaceForNode(context.Background(), nodeID, valid); err != nil {
		s.logger.Printf("nodes: store interfaces for node %s: %v", nodeID, err)
	}
}

// RescanInterfaces asks a node's agent to re-enumerate and re-report its
// interfaces. Gated by rbac.CanManageNodes. Not audited: it changes no
// state by itself - the agent's report_interfaces answer refreshes a
// derived, agent-reported list, the same category as the report an agent
// already sends on every connect.
func (s *Service) RescanInterfaces(ctx context.Context, actor rbac.Actor, nodeID string) error {
	if !rbac.CanManageNodes(actor) {
		return rbac.ErrNotPermitted
	}
	if !s.dispatch.Connected(nodeID) {
		return ErrNodeNotConnected
	}
	env, err := agentproto.NewEnvelope(agentproto.TypeRescanInterfaces, "", agentproto.RescanInterfaces{})
	if err != nil {
		return fmt.Errorf("build rescan_interfaces envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		return fmt.Errorf("dispatch rescan_interfaces to node %s: %w", nodeID, err)
	}
	return nil
}

// SetDefaultTransferInterface records which of a node's reported
// interfaces peer-to-peer transfers use by default. interfaceName "" clears
// it back to "Fastest" (auto-pick by link speed). A non-empty name must be
// among the interfaces the node currently reports (ErrUnknownInterface
// otherwise) - the column has no FK by design, so this Go check is the
// only guard. Gated by rbac.CanManageNodes and always audited
// ("set_default_transfer_interface", including for the SuperAdmin).
func (s *Service) SetDefaultTransferInterface(ctx context.Context, actor rbac.Actor, nodeID, interfaceName string) error {
	if !rbac.CanManageNodes(actor) {
		return rbac.ErrNotPermitted
	}
	if _, err := s.nodes.FindByID(ctx, nodeID); err != nil {
		return err
	}

	var name *string
	if interfaceName != "" {
		ifaces, err := s.interfaces.ListByNode(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("list interfaces for node %s: %w", nodeID, err)
		}
		known := false
		for _, i := range ifaces {
			if i.InterfaceName == interfaceName {
				known = true
				break
			}
		}
		if !known {
			return ErrUnknownInterface
		}
		name = &interfaceName
	}

	if err := s.nodes.SetDefaultTransferInterface(ctx, nodeID, name); err != nil {
		return fmt.Errorf("set default transfer interface: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	detail := map[string]any{"default_transfer_interface": interfaceName}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "set_default_transfer_interface", "node", nodeID, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}
