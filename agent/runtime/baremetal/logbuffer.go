// SPDX-License-Identifier: AGPL-3.0-or-later

package baremetal

import "sync"

// logBufferCapacity bounds how much of a tracked process's combined
// stdout/stderr this package keeps in memory for Logs to return - enough
// for a real diagnostic message (a Python traceback, a CUDA OOM dump),
// not a full history. Not operator-configurable - this is purely an
// in-memory diagnostic aid, not a substitute for the real output already
// reaching journald via os.Stdout/os.Stderr below.
const logBufferCapacity = 16 * 1024

// logBuffer is a bounded, concurrency-safe io.Writer that keeps only the
// most recent logBufferCapacity bytes written to it - a ring buffer in
// spirit, implemented as a plain byte slice trimmed from the front, since
// writes here are infrequent enough (line-buffered process output, not a
// tight loop) that the occasional O(n) trim cost is irrelevant. Two
// separate goroutines (one copying stdout, one stderr - see exec.Cmd's own
// concurrent-copy behavior when both are set) can write to the same
// instance concurrently, hence the mutex.
type logBuffer struct {
	mu   sync.Mutex
	data []byte
}

func newLogBuffer() *logBuffer {
	return &logBuffer{}
}

// Write implements io.Writer. Always returns len(p), nil - a diagnostic
// buffer must never be the reason a managed process's own output copy
// fails or blocks.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, p...)
	if len(b.data) > logBufferCapacity {
		// Trim from the front - keep the most recent bytes, matching
		// Logs' "tail" framing (the same reasoning as the containers
		// backend's Tail option).
		b.data = append([]byte(nil), b.data[len(b.data)-logBufferCapacity:]...)
	}
	return len(p), nil
}

// String returns a snapshot of what's currently buffered.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
