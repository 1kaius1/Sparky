// SPDX-License-Identifier: AGPL-3.0-or-later

// Package containerlogs stores the container logs agents archive before they
// remove a container, and serves them back to people allowed to read them.
// An agent gzips a container's output and sends it as a series of small
// chunks (agentproto.ContainerLogChunk, because a WebSocket message over
// 32768 bytes closes the connection); this package reassembles the series,
// verifies it, stores it, and only then tells the agent it may remove the
// container (agentproto.ContainerLogAck). See docs/AGENT.md Container log
// archive and PLANNING.md's 2026-10-08 Decisions Log.
package containerlogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// MaxCompressedBytes caps one archived log's gzip size. A node cannot
	// make the central app hold more than this per upload in memory or in
	// Postgres.
	MaxCompressedBytes = 16 << 20

	// MaxUncompressedBytes caps what a stored log may expand to, checked
	// when it is stored and again when it is read, so a crafted gzip stream
	// cannot be turned into a memory bomb.
	MaxUncompressedBytes = 64 << 20

	// maxUploadsPerNode bounds how many unfinished uploads one node may
	// hold at once.
	maxUploadsPerNode = 4

	// uploadIdleTimeout drops an upload that has gone quiet - an agent that
	// died or reconnected mid-upload - so its chunks are not held forever.
	uploadIdleTimeout = 2 * time.Minute

	// storeTimeout and ackTimeout bound the database write and the reply
	// that follow the last chunk.
	storeTimeout = 30 * time.Second
	ackTimeout   = 10 * time.Second
)

// ErrNotFound is returned for an archive that does not exist, including an
// id that is not a valid uuid.
var ErrNotFound = errors.New("container log not found")

var (
	uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reasonPattern   = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

// archiveStore is the subset of *db.ContainerLogArchiveRepository this
// package needs.
type archiveStore interface {
	Create(ctx context.Context, in db.NewContainerLogArchive) (*db.ContainerLogArchive, error)
	List(ctx context.Context, limit int) ([]*db.ContainerLogArchive, error)
	FindByID(ctx context.Context, id string) (*db.ContainerLogArchive, []byte, error)
	LatestByInstanceIDs(ctx context.Context, instanceIDs []string) (map[string]string, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// settingsStore is the subset of *db.ContainerLogSettingsRepository this
// package needs: the retention period.
type settingsStore interface {
	Get(ctx context.Context) (*db.ContainerLogSettings, error)
	Update(ctx context.Context, retentionMonths int, updatedBy *string) error
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// instanceLookup is the subset of *db.RunningInstanceRepository used to
// attribute a log to a profile.
type instanceLookup interface {
	FindByID(ctx context.Context, id string) (*db.RunningInstance, error)
}

// profileLookup is the subset of *db.ProfileRepository used to name the
// profile.
type profileLookup interface {
	FindByID(ctx context.Context, id string) (*db.Profile, error)
}

// dispatcher sends the confirmation back to the agent, and a live log request
// out to it.
type dispatcher interface {
	Connected(nodeID string) bool
	Send(ctx context.Context, nodeID string, env agentproto.Envelope) error
}

// upload is one agent upload being reassembled.
type upload struct {
	nodeID   string
	uploadID string
	fetchID  string // set when the chunks answer a live log request, not an archive
	meta     agentproto.ContainerLogMeta
	next     int
	buf      bytes.Buffer
	lastSeen time.Time
}

// Service reassembles, stores and serves archived container logs.
type Service struct {
	store     archiveStore
	settings  settingsStore
	audit     auditRecorder
	instances instanceLookup
	profiles  profileLookup
	dispatch  dispatcher
	logger    *log.Logger
	now       func() time.Time

	// maxCompressed is MaxCompressedBytes, a field only so a test can lower it.
	maxCompressed int

	// maxUncompressed is MaxUncompressedBytes, likewise.
	maxUncompressed int64

	// liveTimeout is how long a live log request waits for the node to
	// answer; a field so tests can shorten it.
	liveTimeout time.Duration

	mu      sync.Mutex
	uploads map[string]*upload
	live    map[string]*liveFetch
}

// NewService constructs a Service.
func NewService(store archiveStore, settings settingsStore, audit auditRecorder, instances instanceLookup, profiles profileLookup, dispatch dispatcher, logger *log.Logger) *Service {
	return &Service{
		store: store, settings: settings, audit: audit, instances: instances, profiles: profiles, dispatch: dispatch, logger: logger,
		now: time.Now, maxCompressed: MaxCompressedBytes, maxUncompressed: MaxUncompressedBytes, liveTimeout: defaultLiveTimeout,
		uploads: make(map[string]*upload), live: make(map[string]*liveFetch),
	}
}

// HandleChunk takes one container_log_chunk from nodeID, the authenticated
// connection it arrived on. It runs on that node's read loop, so it only
// buffers; the verification, database write and reply that follow the last
// chunk run in their own goroutine. An upload that breaks a rule (out of
// order, too large, too many at once) is dropped and answered with a
// not-stored ack, so the agent keeps its container without waiting out its
// timeout.
func (s *Service) HandleChunk(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeContainerLogChunk {
		return
	}
	var chunk agentproto.ContainerLogChunk
	if err := env.DecodePayload(&chunk); err != nil {
		s.logger.Printf("containerlogs: node %s sent a malformed container_log_chunk: %v", nodeID, err)
		return
	}
	if !uploadIDPattern.MatchString(chunk.UploadID) {
		s.logger.Printf("containerlogs: node %s sent a container_log_chunk with an invalid upload id", nodeID)
		return
	}

	key := nodeID + "/" + chunk.UploadID
	s.mu.Lock()
	s.sweepLocked()

	// A chunk that answers a live log request is only accepted from the node
	// the request was sent to, and only while someone is still waiting for it.
	if chunk.FetchID != "" {
		f := s.live[chunk.FetchID]
		if f == nil || f.nodeID != nodeID || chunk.UploadID != chunk.FetchID {
			s.mu.Unlock()
			s.logger.Printf("containerlogs: ignoring a live log chunk from node %s that nobody is waiting for", nodeID)
			return
		}
		if chunk.Error != "" {
			s.mu.Unlock()
			s.deliverLive(chunk.FetchID, nodeID, liveResult{err: &AgentError{Message: clip(chunk.Error, 300)}})
			return
		}
	}

	u := s.uploads[key]
	var reject string
	switch {
	case chunk.Seq == 0 && u != nil:
		reject = "upload restarted"
	case chunk.Seq == 0:
		switch {
		case chunk.Meta == nil:
			reject = "first chunk has no description"
		case s.countForNodeLocked(nodeID) >= maxUploadsPerNode:
			reject = "too many uploads in progress"
		default:
			u = &upload{nodeID: nodeID, uploadID: chunk.UploadID, fetchID: chunk.FetchID, meta: *chunk.Meta}
			s.uploads[key] = u
		}
	case u == nil:
		reject = "unknown upload"
	case chunk.Seq != u.next:
		reject = "chunk out of order"
	}
	if reject == "" && u.buf.Len()+len(chunk.Data) > s.maxCompressed {
		reject = "log too large"
	}
	if reject == "" && len(chunk.Data) > agentproto.ContainerLogChunkSize {
		reject = "chunk too large"
	}
	if reject != "" {
		delete(s.uploads, key)
		s.mu.Unlock()
		s.logger.Printf("containerlogs: rejecting upload %s from node %s: %s", chunk.UploadID, nodeID, reject)
		if chunk.FetchID != "" {
			s.deliverLive(chunk.FetchID, nodeID, liveResult{err: &AgentError{Message: "the node's reply was not valid: " + reject}})
			return
		}
		go s.ack(nodeID, chunk.UploadID, false, reject)
		return
	}

	u.buf.Write(chunk.Data)
	u.next++
	u.lastSeen = s.now()
	if !chunk.Final {
		s.mu.Unlock()
		return
	}
	delete(s.uploads, key)
	s.mu.Unlock()

	if u.fetchID != "" {
		go s.finishLive(u, chunk.TotalBytes, chunk.SHA256)
		return
	}
	go s.finish(u, chunk.TotalBytes, chunk.SHA256)
}

// countForNodeLocked counts nodeID's unfinished uploads. Caller holds s.mu.
func (s *Service) countForNodeLocked(nodeID string) int {
	n := 0
	for _, u := range s.uploads {
		if u.nodeID == nodeID {
			n++
		}
	}
	return n
}

// sweepLocked drops uploads that have gone idle. Caller holds s.mu.
func (s *Service) sweepLocked() {
	cutoff := s.now().Add(-uploadIdleTimeout)
	for k, u := range s.uploads {
		if u.lastSeen.Before(cutoff) {
			delete(s.uploads, k)
		}
	}
}

// finish verifies a fully received upload, stores it, and answers the agent.
func (s *Service) finish(u *upload, totalBytes int64, sha string) {
	gz := u.buf.Bytes()
	sum := sha256.Sum256(gz)
	if int64(len(gz)) != totalBytes || !strings.EqualFold(hex.EncodeToString(sum[:]), sha) {
		s.logger.Printf("containerlogs: upload %s from node %s failed verification (%d bytes received, %d announced)", u.uploadID, u.nodeID, len(gz), totalBytes)
		s.ack(u.nodeID, u.uploadID, false, "the log arrived damaged")
		return
	}
	size, err := uncompressedSize(gz, s.maxUncompressed)
	if err != nil {
		s.logger.Printf("containerlogs: upload %s from node %s is not a valid log: %v", u.uploadID, u.nodeID, err)
		s.ack(u.nodeID, u.uploadID, false, "the log is not valid gzip or is too large")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	in := s.describe(ctx, u, gz, size)
	if _, err := s.store.Create(ctx, in); err != nil {
		s.logger.Printf("containerlogs: store upload %s from node %s: %v", u.uploadID, u.nodeID, err)
		s.ack(u.nodeID, u.uploadID, false, "the central app could not store the log")
		return
	}
	s.ack(u.nodeID, u.uploadID, true, "")
}

// describe builds the row to store. Everything the central app can know for
// itself it takes from its own records, not from the node's description: the
// node is the authenticated connection, the profile comes from the instance
// row, and an instance that belongs to a different node is not linked at all.
func (s *Service) describe(ctx context.Context, u *upload, gz []byte, size int64) db.NewContainerLogArchive {
	m := u.meta
	in := db.NewContainerLogArchive{
		UploadID:      u.uploadID,
		NodeID:        u.nodeID,
		ContainerName: clip(m.ContainerName, 128),
		ContainerID:   clip(m.ContainerID, 64),
		Reason:        "unknown",
		State:         clip(m.State, 32),
		ExitCode:      m.ExitCode,
		OOMKilled:     m.OOMKilled,
		StartedAt:     m.StartedAt,
		FinishedAt:    m.FinishedAt,
		LinesKept:     max(m.LinesKept, 0),
		Truncated:     m.Truncated,
		SizeBytes:     size,
		LogGz:         gz,
	}
	if reasonPattern.MatchString(m.Reason) {
		in.Reason = m.Reason
	}
	if m.LinesRequested > 0 {
		n := m.LinesRequested
		in.LinesRequested = &n
	}

	if !uuidPattern.MatchString(m.InstanceID) {
		return in
	}
	inst, err := s.instances.FindByID(ctx, m.InstanceID)
	if err != nil {
		if !errors.Is(err, db.ErrRunningInstanceNotFound) {
			s.logger.Printf("containerlogs: look up instance %s: %v", m.InstanceID, err)
		}
		return in
	}
	if inst.PrimaryNodeID != u.nodeID {
		s.logger.Printf("containerlogs: node %s archived a log for instance %s, which belongs to another node; not linking it", u.nodeID, m.InstanceID)
		return in
	}
	id := inst.ID
	in.InstanceID = &id
	if profile, err := s.profiles.FindByID(ctx, inst.ProfileID); err == nil {
		pid := profile.ID
		in.ProfileID = &pid
		in.ProfileName = profile.Name
	} else if !errors.Is(err, db.ErrProfileNotFound) {
		s.logger.Printf("containerlogs: look up profile %s: %v", inst.ProfileID, err)
	}
	return in
}

// ack tells the agent whether its log is stored.
func (s *Service) ack(nodeID, uploadID string, stored bool, reason string) {
	env, err := agentproto.NewEnvelope(agentproto.TypeContainerLogAck, "", agentproto.ContainerLogAck{UploadID: uploadID, Stored: stored, Error: reason})
	if err != nil {
		s.logger.Printf("containerlogs: build ack for upload %s: %v", uploadID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := s.dispatch.Send(ctx, nodeID, env); err != nil {
		s.logger.Printf("containerlogs: send ack for upload %s to node %s: %v", uploadID, nodeID, err)
	}
}

// List returns the newest archived logs, without their bodies. Requires
// rbac.CanViewInstanceLogs.
func (s *Service) List(ctx context.Context, actor rbac.Actor, limit int) ([]*db.ContainerLogArchive, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	return s.store.List(ctx, limit)
}

// LatestForInstances returns, for each of instanceIDs that has an archived
// log, the id of its newest one. Requires rbac.CanViewInstanceLogs. Ids that
// are not uuids are skipped rather than failing the whole lookup.
func (s *Service) LatestForInstances(ctx context.Context, actor rbac.Actor, instanceIDs []string) (map[string]string, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, rbac.ErrNotPermitted
	}
	valid := make([]string, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		if uuidPattern.MatchString(id) {
			valid = append(valid, id)
		}
	}
	return s.store.LatestByInstanceIDs(ctx, valid)
}

// Read returns one archived log with its text. Requires
// rbac.CanViewInstanceLogs. Returns ErrNotFound for a missing archive or an
// id that is not a uuid.
func (s *Service) Read(ctx context.Context, actor rbac.Actor, id string) (*db.ContainerLogArchive, string, error) {
	if !rbac.CanViewInstanceLogs(actor) {
		return nil, "", rbac.ErrNotPermitted
	}
	if !uuidPattern.MatchString(id) {
		return nil, "", ErrNotFound
	}
	a, gz, err := s.store.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrContainerLogArchiveNotFound) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("read container log: %w", err)
	}
	text, err := gunzip(gz, s.maxUncompressed)
	if err != nil {
		return nil, "", fmt.Errorf("read container log %s: %w", id, err)
	}
	return a, text, nil
}

// uncompressedSize returns what gz expands to, failing if it is not valid
// gzip or expands past limit bytes.
func uncompressedSize(gz []byte, limit int64) (int64, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return 0, fmt.Errorf("open gzip: %w", err)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, limit+1))
	if err != nil {
		return 0, fmt.Errorf("read gzip: %w", err)
	}
	if n > limit {
		return 0, fmt.Errorf("log expands past %d bytes", limit)
	}
	return n, nil
}

func gunzip(gz []byte, limit int64) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return "", fmt.Errorf("open gzip: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return "", fmt.Errorf("read gzip: %w", err)
	}
	if int64(len(raw)) > limit {
		return "", fmt.Errorf("log expands past %d bytes", limit)
	}
	return string(raw), nil
}

// clip shortens s to at most n bytes without cutting a character in half.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
