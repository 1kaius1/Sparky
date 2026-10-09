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
	sum := sha256.Sum256(gz)

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

	meta := &agentproto.ContainerLogMeta{
		InstanceID:     instanceID,
		ContainerID:    captured.ContainerID,
		ContainerName:  captured.ContainerName,
		Reason:         reason,
		State:          captured.State,
		ExitCode:       captured.ExitCode,
		OOMKilled:      captured.OOMKilled,
		LinesRequested: archiveLogLines,
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

	for seq, off := 0, 0; ; seq++ {
		end := off + agentproto.ContainerLogChunkSize
		if end > len(gz) {
			end = len(gz)
		}
		chunk := agentproto.ContainerLogChunk{UploadID: uploadID, Seq: seq, Data: gz[off:end]}
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
			c.logger.Printf("agent connection: upload log for instance %s: %v", instanceID, err)
			return archiveFailed
		}
		if last {
			break
		}
		off = end
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
