// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NodeStatus is how a node's part of a scan went.
type NodeStatus string

const (
	// ScanDone: the node answered and its containers were evaluated.
	ScanDone NodeStatus = "done"

	// ScanOffline: the node's agent is not connected.
	ScanOffline NodeStatus = "offline"

	// ScanFailed: the node was asked and did not give a usable answer; Error
	// says why. An agent that predates this feature ignores the request and so
	// ends here after the timeout.
	ScanFailed NodeStatus = "failed"
)

// Item is one container on a node, evaluated.
type Item struct {
	Container
	Evaluation

	// ProfileName is the profile the instance belongs to, "" if unknown.
	ProfileName string

	// InstanceStatus is the instance record's status, "" when there is none.
	InstanceStatus string
}

// NodeScan is one node's part of a scan.
type NodeScan struct {
	NodeID   string
	NodeName string
	Status   NodeStatus

	// Error explains ScanOffline and ScanFailed.
	Error string

	// Truncated is true when the node held more containers than it reports.
	Truncated bool

	// Items is every container the node reported, oldest first.
	Items []Item
}

// With returns the node's items with the given verdict.
func (n *NodeScan) With(v Verdict) []Item {
	var out []Item
	for _, it := range n.Items {
		if it.Verdict == v {
			out = append(out, it)
		}
	}
	return out
}

// listWait is a request for one node's container list that has been sent and
// whose parts are still arriving.
type listWait struct {
	nodeID     string
	next       int
	containers []agentproto.ManagedContainer
	truncated  bool
	errMsg     string
	done       chan struct{}
	finished   bool
}

// removeWait is a remove_container request that has been sent.
type removeWait struct {
	nodeID     string
	instanceID string
	ch         chan agentproto.RemoveContainerResult
}

func newRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HandleListContainersResult takes one part of a node's answer to a
// list_containers request. It implements what agentconn's message callback
// calls for TypeListContainersResult. A part from a node other than the one
// asked, for a request nobody is waiting for, or out of order is ignored or
// fails the request - a node can only ever answer for itself.
func (s *Service) HandleListContainersResult(nodeID string, env agentproto.Envelope) {
	var part agentproto.ListContainersResult
	if err := env.DecodePayload(&part); err != nil {
		s.logger.Printf("stalecontainers: node %s sent a malformed list_containers_result: %v", nodeID, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.listWaits[part.ScanID]
	if !ok || w.nodeID != nodeID || w.finished {
		// Late (timed out), unknown, or another node answering for this one.
		return
	}
	switch {
	case part.Part != w.next:
		w.fail(fmt.Sprintf("the agent's answer arrived out of order (part %d, expected %d)", part.Part, w.next))
		return
	case part.Part >= maxListParts:
		w.fail("the agent's answer was longer than expected")
		return
	case len(w.containers)+len(part.Containers) > maxContainersPerNode:
		w.fail("the agent reported more containers than expected")
		return
	}
	w.next++
	w.containers = append(w.containers, part.Containers...)
	if !part.Final {
		return
	}
	w.truncated = part.Truncated
	if part.Error != "" {
		w.errMsg = "the agent could not list its containers: " + part.Error
		w.containers = nil
	}
	w.finish()
}

func (w *listWait) fail(msg string) {
	w.errMsg = msg
	w.containers = nil
	w.finish()
}

func (w *listWait) finish() {
	w.finished = true
	close(w.done)
}

// HandleRemoveContainerResult takes a node's answer to a remove_container
// request, only from the node that was asked and only for the instance that was
// named.
func (s *Service) HandleRemoveContainerResult(nodeID string, env agentproto.Envelope) {
	var res agentproto.RemoveContainerResult
	if err := env.DecodePayload(&res); err != nil {
		s.logger.Printf("stalecontainers: node %s sent a malformed remove_container_result: %v", nodeID, err)
		return
	}
	s.mu.Lock()
	w, ok := s.removeWaits[res.RemoveID]
	if ok && (w.nodeID != nodeID || w.instanceID != res.InstanceID) {
		ok = false
	}
	if ok {
		delete(s.removeWaits, res.RemoveID)
	}
	s.mu.Unlock()
	if ok {
		w.ch <- res // buffered: never blocks
	}
}

// listNode asks one node what containers its runtime holds and waits for the
// whole answer.
func (s *Service) listNode(ctx context.Context, nodeID string) ([]agentproto.ManagedContainer, bool, error) {
	id, err := newRequestID()
	if err != nil {
		return nil, false, err
	}
	w := &listWait{nodeID: nodeID, done: make(chan struct{})}
	s.mu.Lock()
	s.listWaits[id] = w
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listWaits, id)
		s.mu.Unlock()
	}()

	env, err := agentproto.NewEnvelope(agentproto.TypeListContainers, "", agentproto.ListContainers{ScanID: id})
	if err != nil {
		return nil, false, fmt.Errorf("build list_containers: %w", err)
	}
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		return nil, false, fmt.Errorf("send list_containers: %w", err)
	}
	timer := time.NewTimer(s.listTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		return nil, false, fmt.Errorf("the agent did not answer within %s (an agent older than this server ignores the request: upgrade agents before the server)", s.listTimeout)
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.errMsg != "" {
		return nil, false, fmt.Errorf("%s", w.errMsg)
	}
	return w.containers, w.truncated, nil
}

// requestRemove asks a node to remove one container and waits for its answer.
func (s *Service) requestRemove(ctx context.Context, nodeID, instanceID string, lines int) (agentproto.RemoveContainerResult, error) {
	id, err := newRequestID()
	if err != nil {
		return agentproto.RemoveContainerResult{}, err
	}
	w := &removeWait{nodeID: nodeID, instanceID: instanceID, ch: make(chan agentproto.RemoveContainerResult, 1)}
	s.mu.Lock()
	s.removeWaits[id] = w
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.removeWaits, id)
		s.mu.Unlock()
	}()

	env, err := agentproto.NewEnvelope(agentproto.TypeRemoveContainer, "", agentproto.RemoveContainer{RemoveID: id, InstanceID: instanceID, Lines: lines})
	if err != nil {
		return agentproto.RemoveContainerResult{}, fmt.Errorf("build remove_container: %w", err)
	}
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		return agentproto.RemoveContainerResult{}, fmt.Errorf("send remove_container: %w", err)
	}
	timer := time.NewTimer(s.removeTimeout)
	defer timer.Stop()
	select {
	case res := <-w.ch:
		switch res.Outcome {
		case agentproto.RemoveOutcomeRemoved, agentproto.RemoveOutcomeGone, agentproto.RemoveOutcomeRefused,
			agentproto.RemoveOutcomeKept, agentproto.RemoveOutcomeFailed:
		default:
			res.Outcome = agentproto.RemoveOutcomeFailed
			res.Detail = "the agent gave an answer this server does not understand"
		}
		return res, nil
	case <-timer.C:
		return agentproto.RemoveContainerResult{}, fmt.Errorf("the agent did not answer within %s", s.removeTimeout)
	case <-ctx.Done():
		return agentproto.RemoveContainerResult{}, ctx.Err()
	}
}

// validContainer checks one container a node reported and converts it. A node
// is trusted to describe its own containers, not to send well-formed data:
// anything that does not look like what Sparky itself creates is dropped, so
// it can never be shown or acted on.
func validContainer(m agentproto.ManagedContainer) (Container, bool) {
	if !uuidPattern.MatchString(m.InstanceID) {
		return Container{}, false
	}
	c := Container{
		InstanceID:    strings.ToLower(m.InstanceID),
		ContainerID:   printable(m.ContainerID, agentproto.MaxContainerIDLen),
		ContainerName: printable(m.ContainerName, agentproto.MaxContainerNameLen),
		State:         strings.ToLower(printable(m.State, 32)),
		Labelled:      m.Labelled,
		ExitCode:      m.ExitCode,
		OOMKilled:     m.OOMKilled,
	}
	if uuidPattern.MatchString(m.ProfileID) {
		c.ProfileID = strings.ToLower(m.ProfileID)
	}
	if m.CreatedAt != nil {
		c.CreatedAt = *m.CreatedAt
	}
	if m.FinishedAt != nil {
		c.FinishedAt = *m.FinishedAt
	}
	return c, true
}

// printable keeps only printable ASCII and clips to n bytes. What a node sends
// ends up on a page and in an alert; this keeps a hostile or broken agent from
// putting anything else there.
func printable(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= n {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// containerNodes picks the nodes that can hold containers.
func containerNodes(all []*db.Node) []*db.Node {
	var out []*db.Node
	for _, n := range all {
		if n.RuntimeBackend == db.RuntimeBackendDocker || n.RuntimeBackend == db.RuntimeBackendPodman {
			out = append(out, n)
		}
	}
	return out
}

// scan asks each of nodes for its containers, concurrently, and evaluates what
// comes back. A node that is offline or does not answer is reported as such and
// its containers are simply unknown - never assumed absent.
func (s *Service) scan(ctx context.Context, nodes []*db.Node, minAge time.Duration) []NodeScan {
	scans := make([]NodeScan, len(nodes))
	raw := make([][]agentproto.ManagedContainer, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		scans[i] = NodeScan{NodeID: n.ID, NodeName: n.Name}
		if !s.dispatch.Connected(n.ID) {
			scans[i].Status, scans[i].Error = ScanOffline, "the node's agent is not connected"
			continue
		}
		wg.Add(1)
		go func(i int, nodeID string) {
			defer wg.Done()
			containers, truncated, err := s.listNode(ctx, nodeID)
			if err != nil {
				scans[i].Status, scans[i].Error = ScanFailed, err.Error()
				return
			}
			scans[i].Status, scans[i].Truncated = ScanDone, truncated
			raw[i] = containers
		}(i, n.ID)
	}
	wg.Wait()

	// One lookup of every instance named, across all nodes.
	var ids []string
	valid := make([][]Container, len(nodes))
	for i := range nodes {
		for _, m := range raw[i] {
			if c, ok := validContainer(m); ok {
				valid[i] = append(valid[i], c)
				ids = append(ids, c.InstanceID)
			}
		}
	}
	instances, err := s.instances.FindByIDs(ctx, ids)
	if err != nil {
		// Without the database's side nothing can be told apart from a live
		// instance, so report every answered node as failed rather than guess.
		for i := range scans {
			if scans[i].Status == ScanDone {
				scans[i].Status, scans[i].Error = ScanFailed, "could not look up the instances these containers belong to"
			}
		}
		s.logger.Printf("stalecontainers: look up instances for a scan: %v", err)
		return scans
	}
	profileNames := make(map[string]string)
	nameOf := func(profileID string) string {
		if profileID == "" {
			return ""
		}
		if name, ok := profileNames[profileID]; ok {
			return name
		}
		name := ""
		if p, err := s.profiles.FindByID(ctx, profileID); err == nil && p != nil {
			name = p.Name
		}
		profileNames[profileID] = name
		return name
	}

	now := s.now()
	for i := range nodes {
		for _, c := range valid[i] {
			inst := instances[c.InstanceID]
			item := Item{Container: c, Evaluation: Classify(nodes[i].ID, c, inst, now, minAge)}
			profileID := c.ProfileID
			if inst != nil {
				item.InstanceStatus = string(inst.Status)
				profileID = inst.ProfileID
			}
			item.ProfileName = nameOf(profileID)
			scans[i].Items = append(scans[i].Items, item)
		}
		sort.SliceStable(scans[i].Items, func(a, b int) bool {
			ia, ib := scans[i].Items[a], scans[i].Items[b]
			if !ia.StoppedAt.Equal(ib.StoppedAt) {
				return ia.StoppedAt.Before(ib.StoppedAt)
			}
			return ia.InstanceID < ib.InstanceID
		})
	}
	return scans
}

// PreviewResult is what the manual cleanup page shows.
type PreviewResult struct {
	MinAge       time.Duration
	ArchiveLines int
	Nodes        []NodeScan
}

// Preview scans every container node now and returns what is stale, what is
// too recent to be, and what is protected, without changing anything. Requires
// rbac.CanManageStaleContainers. Reading is not audited.
func (s *Service) Preview(ctx context.Context, actor rbac.Actor) (*PreviewResult, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get stale container settings: %w", err)
	}
	all, err := s.nodes.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	minAge := time.Duration(cfg.MinAgeHours) * time.Hour
	return &PreviewResult{
		MinAge:       minAge,
		ArchiveLines: cfg.ArchiveLines,
		Nodes:        s.scan(ctx, containerNodes(all), minAge),
	}, nil
}
