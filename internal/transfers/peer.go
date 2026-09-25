// SPDX-License-Identifier: AGPL-3.0-or-later

package transfers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// peerSSHPort is the source node's sshd port. Fixed at the standard port
// for now - the source is a stock distribution sshd, and nothing in the
// node model records a non-default port.
const peerSSHPort = 22

// maxDestIPs bounds how many of the destination's addresses are offered to
// the source's from= restriction.
const maxDestIPs = 16

// nodeDirectory is the subset of *nodes.Service peer transfers need.
type nodeDirectory interface {
	GetNode(ctx context.Context, id string) (*db.Node, error)
	ListInterfaces(ctx context.Context, nodeID string) ([]*db.NodeNetworkInterface, error)
	ResolveTransferSource(ctx context.Context, nodeID, override string) (nodes.TransferSource, error)
}

// sourceInventory is the read the source-presence check needs -
// *inventory.Service.Get satisfies it.
type sourceInventory interface {
	Get(ctx context.Context, nodeID, modelRef, quantization string, format db.ModelFormat) (*db.NodeModelInventory, error)
}

// peerPlan is everything a peer transfer's two dispatches need, derived
// from current node state. It is recomputed (not remembered) when the
// source's authorize reply arrives, so no in-memory state has to survive a
// server restart.
type peerPlan struct {
	dest, source *db.Node
	src          nodes.TransferSource
	// destIPs is the comma-separated list of the destination's known
	// addresses the source will accept the pull from. The source can't be
	// told which local address the destination will connect from (that
	// depends on routing), so every address the destination has reported is
	// offered. from= is an additional restriction on top of key
	// possession, never the only one.
	destIPs string
}

func (s *Service) planPeer(ctx context.Context, destNodeID, sourceNodeID, override string) (*peerPlan, error) {
	dest, err := s.peers.GetNode(ctx, destNodeID)
	if err != nil {
		return nil, fmt.Errorf("look up destination node: %w", err)
	}
	source, err := s.peers.GetNode(ctx, sourceNodeID)
	if err != nil {
		return nil, fmt.Errorf("look up source node: %w", err)
	}
	if dest.SSHPublicKey == nil || *dest.SSHPublicKey == "" {
		return nil, fmt.Errorf("%w: destination node %q has not reported an SSH identity (run sparky-agent setup and restart the agent)", ErrPeerNotReady, dest.Name)
	}
	if source.SSHHostPublicKey == nil || *source.SSHHostPublicKey == "" {
		return nil, fmt.Errorf("%w: source node %q has not reported an sshd host key", ErrPeerNotReady, source.Name)
	}
	src, err := s.peers.ResolveTransferSource(ctx, sourceNodeID, override)
	if err != nil {
		if errors.Is(err, nodes.ErrNoInterfaces) || errors.Is(err, nodes.ErrUnknownInterface) {
			return nil, fmt.Errorf("%w: source node %q: %v", ErrPeerNotReady, source.Name, err)
		}
		return nil, fmt.Errorf("resolve source interface: %w", err)
	}

	ifaces, err := s.peers.ListInterfaces(ctx, destNodeID)
	if err != nil {
		return nil, fmt.Errorf("list destination interfaces: %w", err)
	}
	seen := make(map[string]bool)
	var ips []string
	add := func(raw string) {
		ip := net.ParseIP(raw)
		if ip == nil || seen[ip.String()] || len(ips) >= maxDestIPs {
			return
		}
		seen[ip.String()] = true
		ips = append(ips, ip.String())
	}
	for _, i := range ifaces {
		add(i.IPAddress)
	}
	add(dest.IPAddress)
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: destination node %q has no known IP address", ErrPeerNotReady, dest.Name)
	}
	return &peerPlan{dest: dest, source: source, src: src, destIPs: strings.Join(ips, ",")}, nil
}

// initiatePeer starts a peer_node transfer: validate everything up front
// (both ends connected, the source really has the model, both have the SSH
// material the pull needs), persist the row, then ask the source to
// authorize the destination's key. The destination is only told to start
// after the source accepts - see HandlePeerAuthorizeResult.
func (s *Service) initiatePeer(ctx context.Context, actor rbac.Actor, params InitiateTransferParams) (*db.ModelTransfer, error) {
	if s.peers == nil || s.sourceInv == nil {
		return nil, fmt.Errorf("%w: peer transfers are not enabled", ErrInvalidTransfer)
	}
	if !s.dispatch.Connected(params.DestNodeID) {
		return nil, ErrDestNodeOffline
	}
	if !s.dispatch.Connected(params.SourceNodeID) {
		return nil, ErrSourceNodeOffline
	}

	entry, err := s.sourceInv.Get(ctx, params.SourceNodeID, params.ModelRef, params.Quantization, params.Format)
	if errors.Is(err, db.ErrNodeModelInventoryNotFound) {
		return nil, ErrSourceNotPresent
	}
	if err != nil {
		return nil, fmt.Errorf("check source inventory: %w", err)
	}
	if entry.Status != db.InventoryStatusPresent {
		return nil, ErrSourceNotPresent
	}

	plan, err := s.planPeer(ctx, params.DestNodeID, params.SourceNodeID, params.SourceInterface)
	if err != nil {
		return nil, err
	}

	var requestedBy *string
	if !actor.IsSuperAdmin {
		requestedBy = &actor.UserID
	}
	var quantization *string
	if params.Quantization != "" {
		quantization = &params.Quantization
	}
	sourceID := params.SourceNodeID
	t, err := s.transfers.Create(ctx, params.DestNodeID, params.ModelRef, db.TransferSourcePeerNode, &sourceID, quantization, requestedBy)
	if err != nil {
		return nil, fmt.Errorf("create model transfer: %w", err)
	}
	format := params.Format
	if err := s.transfers.SetFormat(ctx, t.ID, &format); err != nil {
		s.failPeer(ctx, t, "could not record transfer format")
		return nil, fmt.Errorf("record transfer format: %w", err)
	}
	t.Format = &format
	// Only an explicit per-transfer override is recorded - a node default
	// or Fastest auto-selection leave source_interface NULL (SCHEMA.md).
	if params.SourceInterface != "" {
		override := params.SourceInterface
		if err := s.transfers.SetSourceInterface(ctx, t.ID, &override); err != nil {
			s.failPeer(ctx, t, "could not record source interface")
			return nil, fmt.Errorf("record source interface: %w", err)
		}
		t.SourceInterface = &override
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeAuthorizePeerPull, "", agentproto.AuthorizePeerPull{
		TransferID:    t.ID,
		DestPublicKey: *plan.dest.SSHPublicKey,
		DestIPAddress: plan.destIPs,
		ModelRef:      t.ModelRef,
		Quantization:  params.Quantization,
		Format:        string(params.Format),
	})
	if err != nil {
		s.failPeer(ctx, t, "could not build authorization request")
		return nil, fmt.Errorf("build authorize_peer_pull envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, params.SourceNodeID, env); err != nil {
		s.failPeer(ctx, t, "could not reach the source node")
		return nil, fmt.Errorf("dispatch authorize_peer_pull to node %s: %w", params.SourceNodeID, err)
	}

	detail := map[string]any{
		"dest_node_id":   t.DestNodeID,
		"model_ref":      t.ModelRef,
		"source_type":    string(db.TransferSourcePeerNode),
		"source_node_id": sourceID,
		"quantization":   params.Quantization,
		"format":         string(params.Format),
	}
	if err := s.audit.Record(ctx, requestedBy, actor.IsSuperAdmin, "initiated_transfer", "model_transfer", t.ID, detail); err != nil {
		return nil, fmt.Errorf("record audit: %w", err)
	}
	return t, nil
}

// HandlePeerAuthorizeResult implements agentconn.OnMessageFunc for
// agentproto.TypePeerAuthorizeResult. nodeID is the sending connection's
// authenticated identity; the reply is honored only if that node is the
// transfer's recorded source and the transfer is still queued - a node can
// never answer for a transfer that isn't sourced from it. A rejection fails
// the transfer with the source's reason; an acceptance triggers the
// destination's start_peer_transfer.
func (s *Service) HandlePeerAuthorizeResult(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypePeerAuthorizeResult {
		return
	}
	var result agentproto.PeerAuthorizeResult
	if err := env.DecodePayload(&result); err != nil {
		s.logger.Printf("transfers: node %s sent a malformed peer_authorize_result: %v", nodeID, err)
		return
	}
	ctx := context.Background()
	t, err := s.transfers.FindByID(ctx, result.TransferID)
	if err != nil {
		s.logger.Printf("transfers: peer_authorize_result from node %s for unknown transfer %s: %v", nodeID, result.TransferID, err)
		return
	}
	if t.SourceType != db.TransferSourcePeerNode || t.SourceNodeID == nil || *t.SourceNodeID != nodeID || t.Status != db.TransferStatusQueued {
		s.logger.Printf("transfers: ignoring peer_authorize_result from node %s for transfer %s (not its queued source)", nodeID, t.ID)
		return
	}

	if !result.Accepted {
		reason := boundedReason(result.Reason)
		s.failPeer(ctx, t, "source node declined the pull: "+reason)
		return
	}

	override := ""
	if t.SourceInterface != nil {
		override = *t.SourceInterface
	}
	plan, err := s.planPeer(ctx, t.DestNodeID, nodeID, override)
	if err != nil {
		s.failPeer(ctx, t, err.Error())
		return
	}
	if !s.dispatch.Connected(t.DestNodeID) {
		s.failPeer(ctx, t, "destination node disconnected before the transfer could start")
		return
	}

	start := agentproto.StartPeerTransfer{
		TransferID:          t.ID,
		SourceNodeID:        nodeID,
		SourceHost:          plan.src.IPAddress,
		SourceSSHPort:       peerSSHPort,
		SourceHostPublicKey: *plan.source.SSHHostPublicKey,
		ModelRef:            t.ModelRef,
		Format:              string(*t.Format),
	}
	if t.Quantization != nil {
		start.Quantization = *t.Quantization
	}
	startEnv, err := agentproto.NewEnvelope(agentproto.TypeStartPeerTransfer, "", start)
	if err != nil {
		s.failPeer(ctx, t, "could not build the start request")
		return
	}
	if err := s.dispatch.Send(ctx, t.DestNodeID, startEnv); err != nil {
		s.logger.Printf("transfers: dispatch start_peer_transfer for %s: %v", t.ID, err)
		s.failPeer(ctx, t, "could not reach the destination node")
	}
}

// failPeer marks a peer transfer failed and asks the source to drop its
// authorization right away (best-effort - the source's grant also
// self-expires).
func (s *Service) failPeer(ctx context.Context, t *db.ModelTransfer, message string) {
	if err := s.transfers.SetStatus(ctx, t.ID, db.TransferStatusFailed, &message); err != nil {
		s.logger.Printf("transfers: mark transfer %s failed: %v", t.ID, err)
	}
	s.revokePeer(ctx, t)
}

// revokePeer sends revoke_peer_pull to a peer transfer's source. A no-op
// for anything but a peer transfer, and silently skipped if the source is
// offline (its grant self-expires and is swept at its next startup).
func (s *Service) revokePeer(ctx context.Context, t *db.ModelTransfer) {
	if t.SourceType != db.TransferSourcePeerNode || t.SourceNodeID == nil {
		return
	}
	if !s.dispatch.Connected(*t.SourceNodeID) {
		return
	}
	env, err := agentproto.NewEnvelope(agentproto.TypeRevokePeerPull, "", agentproto.RevokePeerPull{TransferID: t.ID})
	if err != nil {
		s.logger.Printf("transfers: build revoke_peer_pull for %s: %v", t.ID, err)
		return
	}
	if err := s.dispatch.Send(ctx, *t.SourceNodeID, env); err != nil {
		s.logger.Printf("transfers: dispatch revoke_peer_pull for %s: %v", t.ID, err)
	}
}

func boundedReason(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ConnectivityResult is the outcome of a destination-side reachability
// check - see CheckConnectivity.
type ConnectivityResult struct {
	Reachable bool
	LatencyMs int64
	Reason    string
}

type pendingCheck struct {
	destNodeID string
	created    time.Time
	result     *ConnectivityResult
}

const checkRetention = 10 * time.Minute

type checkRegistry struct {
	mu     sync.Mutex
	checks map[string]*pendingCheck
}

func (r *checkRegistry) add(id, destNodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checks == nil {
		r.checks = make(map[string]*pendingCheck)
	}
	now := time.Now()
	for k, c := range r.checks {
		if now.Sub(c.created) > checkRetention {
			delete(r.checks, k)
		}
	}
	r.checks[id] = &pendingCheck{destNodeID: destNodeID, created: now}
}

func (r *checkRegistry) complete(id, fromNodeID string, res ConnectivityResult) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.checks[id]
	if !ok || c.destNodeID != fromNodeID || c.result != nil {
		return false
	}
	c.result = &res
	return true
}

func (r *checkRegistry) get(id string) (*ConnectivityResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.checks[id]
	if !ok {
		return nil, false
	}
	return c.result, true
}

// CheckConnectivity asks the destination node to test whether it can open a
// TCP connection to the chosen source interface's sshd - the "Check
// Destination" step gating a peer transfer's submit control. Gated by
// rbac.CanManageModelStore, like initiating the transfer itself. Returns a
// check ID; poll GetConnectivityResult. This proves the network path is
// open at check time only - not that the later authenticated pull will
// succeed - and consumes no SSH authorization.
func (s *Service) CheckConnectivity(ctx context.Context, actor rbac.Actor, destNodeID, sourceNodeID, sourceInterface string) (string, error) {
	permitted, err := s.canManageModelStore(ctx, actor)
	if err != nil {
		return "", err
	}
	if !permitted {
		return "", rbac.ErrNotPermitted
	}
	if s.peers == nil {
		return "", fmt.Errorf("%w: peer transfers are not enabled", ErrInvalidTransfer)
	}
	if destNodeID == "" || sourceNodeID == "" || destNodeID == sourceNodeID {
		return "", fmt.Errorf("%w: a destination and a different source node are required", ErrInvalidTransfer)
	}
	if !s.dispatch.Connected(destNodeID) {
		return "", ErrDestNodeOffline
	}
	src, err := s.peers.ResolveTransferSource(ctx, sourceNodeID, sourceInterface)
	if err != nil {
		if errors.Is(err, nodes.ErrNoInterfaces) || errors.Is(err, nodes.ErrUnknownInterface) {
			return "", fmt.Errorf("%w: %v", ErrPeerNotReady, err)
		}
		return "", fmt.Errorf("resolve source interface: %w", err)
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate check id: %w", err)
	}
	checkID := hex.EncodeToString(raw[:])
	s.checks.add(checkID, destNodeID)

	env, err := agentproto.NewEnvelope(agentproto.TypeCheckPeerConnectivity, "", agentproto.CheckPeerConnectivity{
		CheckID: checkID, SourceHost: src.IPAddress, SourceSSHPort: peerSSHPort,
	})
	if err != nil {
		return "", fmt.Errorf("build check_peer_connectivity envelope: %w", err)
	}
	if err := s.dispatch.Send(ctx, destNodeID, env); err != nil {
		return "", fmt.Errorf("dispatch check_peer_connectivity to node %s: %w", destNodeID, err)
	}
	return checkID, nil
}

// HandleConnectivityCheckResult implements agentconn.OnMessageFunc for
// agentproto.TypeConnectivityCheckResult. Only the node the check was sent
// to can complete it, and only once.
func (s *Service) HandleConnectivityCheckResult(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeConnectivityCheckResult {
		return
	}
	var r agentproto.ConnectivityCheckResult
	if err := env.DecodePayload(&r); err != nil {
		s.logger.Printf("transfers: node %s sent a malformed connectivity_check_result: %v", nodeID, err)
		return
	}
	res := ConnectivityResult{Reachable: r.Reachable, LatencyMs: r.LatencyMs, Reason: boundedReason(r.Reason)}
	if !s.checks.complete(r.CheckID, nodeID, res) {
		s.logger.Printf("transfers: ignoring connectivity_check_result %q from node %s (unknown, foreign, or already answered)", r.CheckID, nodeID)
	}
}

// GetConnectivityResult returns a check's outcome; ok is false for an
// unknown check, and the result is nil while the check is still pending.
func (s *Service) GetConnectivityResult(checkID string) (result *ConnectivityResult, ok bool) {
	return s.checks.get(checkID)
}
