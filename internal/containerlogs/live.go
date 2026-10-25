// SPDX-License-Identifier: AGPL-3.0-or-later

package containerlogs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// defaultLiveTimeout is how long a live log request waits for the node.
	defaultLiveTimeout = 15 * time.Second

	// DefaultLiveLines and MaxLiveLines bound how many trailing lines a live
	// log request asks for.
	DefaultLiveLines = 500
	MaxLiveLines     = 5000

	// maxLiveFetches and maxLiveFetchesPerNode bound the live requests in
	// flight, so a page that is refreshed hard cannot pile work onto a node.
	maxLiveFetches        = 8
	maxLiveFetchesPerNode = 2
)

var (
	// ErrNodeOffline is returned when the node an instance runs on has no
	// agent connected, so there is no one to ask.
	ErrNodeOffline = errors.New("the node this instance runs on is not connected")

	// ErrBusy is returned when too many live log requests are in flight.
	ErrBusy = errors.New("too many log requests are in progress; try again in a moment")

	// ErrAgentSilent is returned when the node did not answer in time - an
	// agent too old to know the request, or one that is stuck.
	ErrAgentSilent = errors.New("the node did not answer in time")
)

// AgentError is the node's own reason it could not give the log - for
// example that the container is gone. Its message is safe to show.
type AgentError struct{ Message string }

func (e *AgentError) Error() string { return e.Message }

// LiveLog is an instance's current output, read from its node just now.
type LiveLog struct {
	InstanceID  string
	ProfileName string
	Meta        agentproto.ContainerLogMeta
	Text        string
}

// liveFetch is one live request waiting for its answer.
type liveFetch struct {
	nodeID string
	ch     chan liveResult
}

type liveResult struct {
	meta *agentproto.ContainerLogMeta
	text string
	err  error
}

// Live reads instanceID's current output from its node and returns it,
// waiting for the node's answer (a few seconds at most). Requires
// rbac.CanViewInstanceLogs. Nothing is stored. lines is clamped to
// 1..MaxLiveLines, with 0 meaning DefaultLiveLines.
//
// Returns ErrNotFound for an unknown instance, ErrNodeOffline,
// ErrBusy, ErrAgentSilent, or an *AgentError carrying the node's own reason.
func (s *Service) Live(ctx context.Context, actor rbac.Actor, instanceID string, lines int) (*LiveLog, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if !uuidPattern.MatchString(instanceID) {
		return nil, ErrNotFound
	}
	if lines <= 0 {
		lines = DefaultLiveLines
	}
	if lines > MaxLiveLines {
		lines = MaxLiveLines
	}

	inst, err := s.instances.FindByID(ctx, instanceID)
	if err != nil {
		if errors.Is(err, db.ErrRunningInstanceNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("look up instance %s: %w", instanceID, err)
	}
	if !s.dispatch.Connected(inst.PrimaryNodeID) {
		return nil, ErrNodeOffline
	}

	fetchID, err := newFetchID()
	if err != nil {
		return nil, err
	}
	f := &liveFetch{nodeID: inst.PrimaryNodeID, ch: make(chan liveResult, 1)}
	s.mu.Lock()
	perNode := 0
	for _, o := range s.live {
		if o.nodeID == f.nodeID {
			perNode++
		}
	}
	if len(s.live) >= maxLiveFetches || perNode >= maxLiveFetchesPerNode {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.live[fetchID] = f
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.live, fetchID)
		s.mu.Unlock()
	}()

	env, err := agentproto.NewEnvelope(agentproto.TypeFetchLogs, "", agentproto.FetchLogs{FetchID: fetchID, InstanceID: instanceID, Lines: lines})
	if err != nil {
		return nil, fmt.Errorf("build fetch_logs: %w", err)
	}
	if err := s.dispatch.Send(ctx, f.nodeID, env); err != nil {
		return nil, fmt.Errorf("send fetch_logs to node %s: %w", f.nodeID, err)
	}

	timer := time.NewTimer(s.liveTimeout)
	defer timer.Stop()
	var res liveResult
	select {
	case res = <-f.ch:
	case <-timer.C:
		return nil, ErrAgentSilent
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}

	out := &LiveLog{InstanceID: instanceID, Text: res.text}
	if res.meta != nil {
		out.Meta = *res.meta
	}
	if profile, err := s.profiles.FindByID(ctx, inst.ProfileID); err == nil {
		out.ProfileName = profile.Name
	}
	return out, nil
}

// deliverLive hands a result to the live request waiting for it, if it is
// still waiting and was sent to nodeID. A late or duplicate answer is dropped.
func (s *Service) deliverLive(fetchID, nodeID string, res liveResult) {
	s.mu.Lock()
	f := s.live[fetchID]
	s.mu.Unlock()
	if f == nil || f.nodeID != nodeID {
		return
	}
	select {
	case f.ch <- res:
	default:
	}
}

// finishLive verifies a fully received live reply and hands the text to the
// request waiting for it. Nothing is stored and no confirmation is sent.
func (s *Service) finishLive(u *upload, totalBytes int64, sha string) {
	gz := u.buf.Bytes()
	sum := sha256.Sum256(gz)
	if int64(len(gz)) != totalBytes || !strings.EqualFold(hex.EncodeToString(sum[:]), sha) {
		s.logger.Printf("containerlogs: live reply %s from node %s failed verification", u.fetchID, u.nodeID)
		s.deliverLive(u.fetchID, u.nodeID, liveResult{err: &AgentError{Message: "the node's reply arrived damaged"}})
		return
	}
	text, err := gunzip(gz, s.maxUncompressed)
	if err != nil {
		s.logger.Printf("containerlogs: live reply %s from node %s is not a valid log: %v", u.fetchID, u.nodeID, err)
		s.deliverLive(u.fetchID, u.nodeID, liveResult{err: &AgentError{Message: "the node's reply was not valid"}})
		return
	}
	meta := u.meta
	s.deliverLive(u.fetchID, u.nodeID, liveResult{meta: &meta, text: text})
}

func newFetchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
