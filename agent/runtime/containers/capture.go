// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// maxCaptureBytes bounds how much output Capture keeps in memory. Docker's
// own log driver can hold far more than a reasonable archive row; when the
// output is larger, the newest bytes are kept and Capture reports the loss.
const maxCaptureBytes = 32 << 20

// Capture reads what instanceID's container left behind: its exit reason
// from inspect, and its last lines of output - see runtime.Backend.Capture.
// A container that does not exist reports runtime.ErrNothingToCapture.
func (b *Backend) Capture(ctx context.Context, instanceID string, lines int) (runtime.Capture, error) {
	ref, err := b.resolve(ctx, instanceID)
	if err != nil {
		return runtime.Capture{}, err
	}
	inspected, err := b.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return runtime.Capture{}, runtime.ErrNothingToCapture
		}
		return runtime.Capture{}, fmt.Errorf("inspect container %s: %w", ref, err)
	}

	tail := "all"
	if lines > 0 {
		tail = strconv.Itoa(lines)
	}
	rc, err := b.cli.ContainerLogs(ctx, ref, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: tail})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return runtime.Capture{}, runtime.ErrNothingToCapture
		}
		return runtime.Capture{}, fmt.Errorf("get logs for container %s: %w", ref, err)
	}
	defer rc.Close()

	out := &tailBuffer{limit: maxCaptureBytes}
	if _, err := stdcopy.StdCopy(out, out, rc); err != nil && !errors.Is(err, io.EOF) {
		return runtime.Capture{}, fmt.Errorf("read logs for container %s: %w", ref, err)
	}

	c := runtime.Capture{
		ContainerID:   inspected.Container.ID,
		ContainerName: strings.TrimPrefix(inspected.Container.Name, "/"),
		Log:           out.String(),
		Truncated:     out.dropped,
	}
	c.LinesKept = countLines(c.Log)
	applyState(&c, inspected.Container.State)
	return c, nil
}

// applyState copies the container's own state and exit reason into c. The
// exit code is left unset for a container that has not exited (created or
// still running), where Docker reports 0 without it meaning anything.
func applyState(c *runtime.Capture, st *container.State) {
	if st == nil {
		return
	}
	c.State = string(st.Status)
	c.OOMKilled = st.OOMKilled
	if !st.Running && st.Status != container.StateCreated && st.Status != container.StateRestarting {
		code := st.ExitCode
		c.ExitCode = &code
	}
	c.StartedAt = parseDockerTime(st.StartedAt)
	c.FinishedAt = parseDockerTime(st.FinishedAt)
}

// parseDockerTime parses the daemon's RFC 3339 timestamps. The daemon reports
// "0001-01-01T00:00:00Z" for "never", which parses to the zero time, as does
// anything unparseable - callers treat the zero time as unknown.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// countLines counts the lines in s, counting a final line with no trailing
// newline.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// tailBuffer is an io.Writer that keeps only the last limit bytes written to
// it and remembers whether it had to drop earlier ones. It is written to by
// stdcopy for both streams, one goroutine, so it needs no lock.
type tailBuffer struct {
	limit   int
	buf     bytes.Buffer
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.limit {
		t.buf.Reset()
		t.buf.Write(p[len(p)-t.limit:])
		t.dropped = true
		return n, nil
	}
	if over := t.buf.Len() + len(p) - t.limit; over > 0 {
		kept := append([]byte(nil), t.buf.Bytes()[over:]...)
		t.buf.Reset()
		t.buf.Write(kept)
		t.dropped = true
	}
	t.buf.Write(p)
	return n, nil
}

func (t *tailBuffer) String() string { return t.buf.String() }
