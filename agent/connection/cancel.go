// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/1kaius1/Sparky/agent/transfer"
)

// statusCancelled is the wire value for a cancelled transfer
// (agentproto.TransferProgress.Status, matching the central app's
// transfer_status enum). agent/transfer defines only the states an executor
// itself can reach, and cancellation is decided here, not there.
const statusCancelled = "cancelled"

// transferRun is one in-flight transfer's cancellation handle - shared by
// an internet download and a peer pull, which both run under a per-transfer
// context.
type transferRun struct {
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// transferRuns tracks the transfers this agent is currently running, so a
// cancel_transfer command can find and stop the right one.
type transferRuns struct {
	mu   sync.Mutex
	runs map[string]*transferRun
}

// registerTransfer creates the context a transfer runs under and records it
// so cancelTransfer can reach it. It is called from dispatch, on the read
// loop, *before* the transfer's goroutine starts - a cancel_transfer that
// arrives right behind its start message is processed by that same loop, so
// registering inside the goroutine would let the cancel find nothing and
// be lost. The returned done must be called when the transfer ends.
func (c *Conn) registerTransfer(parent context.Context, transferID string) (workCtx context.Context, run *transferRun, done func()) {
	workCtx, cancel := context.WithCancel(parent)
	run = &transferRun{cancel: cancel}
	c.runs.mu.Lock()
	if c.runs.runs == nil {
		c.runs.runs = make(map[string]*transferRun)
	}
	c.runs.runs[transferID] = run
	c.runs.mu.Unlock()
	return workCtx, run, func() {
		c.runs.mu.Lock()
		if c.runs.runs[transferID] == run {
			delete(c.runs.runs, transferID)
		}
		c.runs.mu.Unlock()
		cancel()
	}
}

// cancelTransfer stops a running transfer. It reports false if this agent
// is not running that transfer (already finished, or never started here) -
// not an error: the central app may cancel a transfer that just completed.
func (c *Conn) cancelTransfer(transferID string) bool {
	c.runs.mu.Lock()
	run := c.runs.runs[transferID]
	c.runs.mu.Unlock()
	if run == nil {
		return false
	}
	run.cancelled.Store(true)
	run.cancel()
	return true
}

// cancelAwareProgress wraps a transfer's progress callback so that, once
// the transfer has been cancelled, the failure its executor reports as a
// consequence of the cancelled context ("context canceled") is reported as
// what actually happened: a cancellation, with no error message. A failure
// that happened for its own reasons before the cancel is unaffected.
func cancelAwareProgress(run *transferRun, next func(bytesTransferred, bytesTotal int64, status, errMsg string)) func(int64, int64, string, string) {
	return func(bytesTransferred, bytesTotal int64, status, errMsg string) {
		if status == transfer.StatusFailed && run.cancelled.Load() {
			next(bytesTransferred, bytesTotal, statusCancelled, "")
			return
		}
		next(bytesTransferred, bytesTotal, status, errMsg)
	}
}
