// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// archiveLogLines is how many trailing lines of a container's output are
// archived. A fixed value for now; the configurable default (1 to all)
// arrives with the cleanup settings (PLANNING.md Decisions Log, 2026-10-08
// container cleanup and log archive).
const archiveLogLines = 2000

// defaultArchiveAckTimeout is how long the agent waits for the central app
// to confirm it stored an archived log before giving up and keeping the
// container. Generous because the server writes to Postgres before it
// answers; not tunable per node.
const defaultArchiveAckTimeout = 30 * time.Second

// archiveOutcome is how an attempt to archive an instance's log ended.
type archiveOutcome int

const (
	// archiveStored: the central app confirmed it stored the log. Only this
	// outcome allows the container to be removed.
	archiveStored archiveOutcome = iota

	// archiveNothing: the instance has nothing left to capture (its
	// container or process is already gone), so there is nothing to archive
	// and nothing worth keeping a container for.
	archiveNothing

	// archiveFailed: the log could not be captured, sent, or confirmed. The
	// container stays, so the log is not lost.
	archiveFailed

	// archiveBusy: another archive of the same instance is already running.
	archiveBusy
)

// archiveInstance captures instanceID's output and exit reason, uploads it to
// the central app in small gzip chunks, and waits for the central app to
// confirm it stored the log. It never removes anything itself: the caller
// removes the container only on archiveStored.
func (c *Conn) archiveInstance(ctx context.Context, conn *websocket.Conn, instanceID, reason string) archiveOutcome {
	if !c.beginArchive(instanceID) {
		return archiveBusy
	}
	defer c.endArchive(instanceID)

	captured, err := c.runtime.Capture(ctx, instanceID, archiveLogLines)
	if errors.Is(err, runtime.ErrNothingToCapture) {
		return archiveNothing
	}
	if err != nil {
		c.logger.Printf("agent connection: capture log for instance %s: %v", instanceID, err)
		return archiveFailed
	}

	gz, err := gzipBytes([]byte(captured.Log))
	if err != nil {
		c.logger.Printf("agent connection: compress log for instance %s: %v", instanceID, err)
		return archiveFailed
	}

	uploadID, err := newUploadID()
	if err != nil {
		c.logger.Printf("agent connection: archive upload id for instance %s: %v", instanceID, err)
		return archiveFailed
	}

	// Registered before the first chunk is sent, so an ack that arrives
	// right behind the last chunk cannot be lost - same ordering as
	// registerTransfer for a cancel arriving right behind a start.
	ackCh := c.registerArchiveWait(uploadID)
	defer c.unregisterArchiveWait(uploadID)

	meta := logMeta(instanceID, reason, archiveLogLines, captured)
	if err := c.sendLogChunks(ctx, conn, uploadID, "", meta, gz); err != nil {
		c.logger.Printf("agent connection: upload log for instance %s: %v", instanceID, err)
		return archiveFailed
	}

	timer := time.NewTimer(c.archiveAckTimeout)
	defer timer.Stop()
	select {
	case ack := <-ackCh:
		if !ack.Stored {
			c.logger.Printf("agent connection: central app did not store the log for instance %s: %s", instanceID, ack.Error)
			return archiveFailed
		}
		return archiveStored
	case <-timer.C:
		c.logger.Printf("agent connection: no confirmation that the log for instance %s was stored within %s", instanceID, c.archiveAckTimeout)
		return archiveFailed
	case <-ctx.Done():
		return archiveFailed
	}
}

// logMeta describes a capture for the central app.
func logMeta(instanceID, reason string, linesRequested int, captured runtime.Capture) *agentproto.ContainerLogMeta {
	meta := &agentproto.ContainerLogMeta{
		InstanceID:     instanceID,
		ContainerID:    captured.ContainerID,
		ContainerName:  captured.ContainerName,
		Reason:         reason,
		State:          captured.State,
		ExitCode:       captured.ExitCode,
		OOMKilled:      captured.OOMKilled,
		LinesRequested: linesRequested,
		LinesKept:      captured.LinesKept,
		Truncated:      captured.Truncated,
		SizeBytes:      int64(len(captured.Log)),
	}
	if !captured.StartedAt.IsZero() {
		t := captured.StartedAt
		meta.StartedAt = &t
	}
	if !captured.FinishedAt.IsZero() {
		t := captured.FinishedAt
		meta.FinishedAt = &t
	}
	return meta
}

// sendLogChunks sends gz as an ordered series of container_log_chunk
// messages of at most agentproto.ContainerLogChunkSize bytes each: Meta on the
// first, the total size and SHA-256 on the last. fetchID is set only when the
// series answers a fetch_logs request. It returns the first send error.
func (c *Conn) sendLogChunks(ctx context.Context, conn *websocket.Conn, uploadID, fetchID string, meta *agentproto.ContainerLogMeta, gz []byte) error {
	sum := sha256.Sum256(gz)
	for seq, off := 0, 0; ; seq++ {
		end := off + agentproto.ContainerLogChunkSize
		if end > len(gz) {
			end = len(gz)
		}
		chunk := agentproto.ContainerLogChunk{UploadID: uploadID, FetchID: fetchID, Seq: seq, Data: gz[off:end]}
		if seq == 0 {
			chunk.Meta = meta
		}
		last := end == len(gz)
		if last {
			chunk.Final = true
			chunk.TotalBytes = int64(len(gz))
			chunk.SHA256 = hex.EncodeToString(sum[:])
		}
		if err := c.trySend(ctx, conn, agentproto.TypeContainerLogChunk, "", chunk); err != nil {
			return err
		}
		if last {
			return nil
		}
		off = end
	}
}

// Bounds on a live log request's line count: the central app's own limits are
// the same, but the agent does not trust the wire.
const (
	defaultLiveLogLines = 500
	maxLiveLogLines     = 5000
)

// runFetchLogs answers a fetch_logs request: read the instance's current
// output and send it back as a chunk series tagged with the request's id.
// Nothing is stored, removed or waited on. An instance that cannot be read
// (its container is gone, the daemon is unreachable) is answered with a single
// error chunk so the person waiting is told promptly instead of timing out.
func (c *Conn) runFetchLogs(ctx context.Context, conn *websocket.Conn, req agentproto.FetchLogs) {
	reply := func(errMsg string) {
		chunk := agentproto.ContainerLogChunk{UploadID: req.FetchID, FetchID: req.FetchID, Seq: 0, Final: true, Error: errMsg}
		if err := c.trySend(ctx, conn, agentproto.TypeContainerLogChunk, "", chunk); err != nil {
			c.logger.Printf("agent connection: answer fetch_logs %s: %v", req.FetchID, err)
		}
	}

	lines := req.Lines
	if lines <= 0 {
		lines = defaultLiveLogLines
	}
	if lines > maxLiveLogLines {
		lines = maxLiveLogLines
	}

	captured, err := c.runtime.Capture(ctx, req.InstanceID, lines)
	if errors.Is(err, runtime.ErrNothingToCapture) {
		reply("this instance has no container or process on the node any more")
		return
	}
	if err != nil {
		c.logger.Printf("agent connection: fetch logs for instance %s: %v", req.InstanceID, err)
		reply("the log could not be read from the node")
		return
	}
	gz, err := gzipBytes([]byte(captured.Log))
	if err != nil {
		c.logger.Printf("agent connection: compress live log for instance %s: %v", req.InstanceID, err)
		reply("the log could not be prepared")
		return
	}
	if err := c.sendLogChunks(ctx, conn, req.FetchID, req.FetchID, logMeta(req.InstanceID, "live", lines, captured), gz); err != nil {
		c.logger.Printf("agent connection: send live log for instance %s: %v", req.InstanceID, err)
	}
}

// archiveAndRemove archives instanceID's log and removes what the backend
// holds for it only if the central app confirmed storing the log. A failed
// archive leaves the container in place and says so in the agent's log: the
// rule is that no container is removed without a stored log. A container the
// backend no longer has is simply cleared.
func (c *Conn) archiveAndRemove(ctx context.Context, conn *websocket.Conn, instanceID, reason string) {
	switch c.archiveInstance(ctx, conn, instanceID, reason) {
	case archiveStored, archiveNothing:
		if err := c.runtime.Remove(ctx, instanceID); err != nil {
			c.logger.Printf("agent connection: remove instance %s: %v", instanceID, err)
		}
	case archiveBusy:
		// Whoever is archiving it owns the removal.
	default:
		c.logger.Printf("agent connection: keeping the stopped container for instance %s because its log was not archived", instanceID)
	}
}

// cleanUpFailedLaunch is what happens to an instance whose launch failed -
// the engine could not be started, or started and never became ready. Its
// container (or process) is stopped so a hung engine gives back its GPU memory
// and port, then archived and removed like any other. Nothing is left running
// or piled up, and the log that says why it failed is saved first. A container
// that was never created is simply cleared. If it cannot even be stopped it is
// left alone: removing a running container is not on offer, and the operator's
// failed report has already gone out.
func (c *Conn) cleanUpFailedLaunch(ctx context.Context, conn *websocket.Conn, instanceID string) {
	if err := c.runtime.Halt(ctx, instanceID); err != nil {
		c.logger.Printf("agent connection: stop failed launch %s: %v", instanceID, err)
		return
	}
	c.archiveAndRemove(ctx, conn, instanceID, "failed_launch")
}

// replaceStaleContainers is replace-on-launch: before a profile's new
// container is created, every older container the same profile left behind -
// an earlier run, a failed launch whose archive did not go through, an engine
// that was OOM-killed - is stopped, archived and removed. A container's
// command line, image and mounts are fixed when it is created, so one left
// over from before the profile was edited would otherwise sit there looking
// current and run stale settings if anyone started it. The central app only
// sends the load once it has no active instance for the profile, so any
// container found here is stale; the agent additionally skips an instance it
// is itself tracking as running. A failure here is logged and never blocks the
// launch.
func (c *Conn) replaceStaleContainers(ctx context.Context, conn *websocket.Conn, profileID, currentInstanceID string) {
	if profileID == "" {
		return
	}
	ids, err := c.runtime.InstancesForProfile(ctx, profileID)
	if err != nil {
		c.logger.Printf("agent connection: look for stale containers of profile %s: %v", profileID, err)
		return
	}
	for _, id := range ids {
		if id == currentInstanceID || c.isActiveInstance(id) {
			continue
		}
		if err := c.runtime.Halt(ctx, id); err != nil {
			c.logger.Printf("agent connection: stop stale instance %s of profile %s: %v", id, profileID, err)
			continue
		}
		c.archiveAndRemove(ctx, conn, id, "replaced")
	}
}

// deliverArchiveAck hands a central app confirmation to the archive waiting
// for it. An ack nobody waits for (it timed out, or a duplicate) is ignored.
func (c *Conn) deliverArchiveAck(ack agentproto.ContainerLogAck) {
	c.archiveMu.Lock()
	ch := c.archiveWaiters[ack.UploadID]
	c.archiveMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ack:
	default:
	}
}

func (c *Conn) registerArchiveWait(uploadID string) chan agentproto.ContainerLogAck {
	ch := make(chan agentproto.ContainerLogAck, 1)
	c.archiveMu.Lock()
	c.archiveWaiters[uploadID] = ch
	c.archiveMu.Unlock()
	return ch
}

func (c *Conn) unregisterArchiveWait(uploadID string) {
	c.archiveMu.Lock()
	delete(c.archiveWaiters, uploadID)
	c.archiveMu.Unlock()
}

// beginArchive marks instanceID as being archived, reporting false if it
// already is, so two paths that both want to archive and remove the same
// container cannot upload it twice.
func (c *Conn) beginArchive(instanceID string) bool {
	c.archiveMu.Lock()
	defer c.archiveMu.Unlock()
	if _, busy := c.archiving[instanceID]; busy {
		return false
	}
	c.archiving[instanceID] = struct{}{}
	return true
}

func (c *Conn) endArchive(instanceID string) {
	c.archiveMu.Lock()
	delete(c.archiving, instanceID)
	c.archiveMu.Unlock()
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// newUploadID returns 128 random bits as hex. The central app uses it as the
// idempotency key for the stored row, so a retry never stores twice.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
