// SPDX-License-Identifier: AGPL-3.0-or-later

// Package baremetal is the bare-metal runtime backend - direct process
// exec, running under whatever account owns the agent process (serviceloop
// on a packaged install - see docs/AGENT.md Install (bare metal)). Used
// when GPU passthrough isn't viable for a node - see ARCHITECTURE.md
// Runtime Backends and SCHEMA.md Nodes' runtime_backend.
//
// Unlike agent/runtime/containers, an exec'd engine process is a real
// child of the agent process - docs/AGENT.md Signal Handling calls this
// out explicitly: "an unclean agent exit risks orphaning or corrupting
// it." Shutdown is what makes a clean agent exit not do that.
package baremetal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// stopGracePeriod is how long Stop/Shutdown wait after SIGTERM before
// escalating to SIGKILL. Generous headroom under the systemd unit's default
// TimeoutStopSec (90s) - not operator-configurable, same reasoning as
// agent/connection's heartbeatInterval: no case yet for making it tunable.
// A var, not a const, solely so tests can shrink it rather than block for
// the real duration.
var stopGracePeriod = 15 * time.Second

// trackedProcess is one exec'd engine instance this Backend owns.
type trackedProcess struct {
	cmd *exec.Cmd

	// done is closed by the reaper goroutine once cmd.Wait() returns,
	// whether the process exited on its own or was signaled.
	done chan struct{}

	// waitErr is cmd.Wait()'s return value, set before done is closed (so
	// reading it after <-done is race-free).
	waitErr error

	// logs captures the most recent combined stdout/stderr this process
	// produced - see logbuffer.go and Logs below. The health-reporting
	// pass this type's own former doc comment anticipated.
	logs *logBuffer
}

// Backend execs engine processes directly and tracks them by instance ID,
// since - unlike a container - there is no daemon to re-query for a live
// process's identity later; this package is the only record of it.
type Backend struct {
	mu        sync.Mutex
	processes map[string]*trackedProcess
}

// New constructs a Backend with no tracked processes.
func New() *Backend {
	return &Backend{processes: make(map[string]*trackedProcess)}
}

// Start execs spec.BinaryPath with spec.Args and tracks the resulting
// process under spec.InstanceID, returning its PID (informational only -
// see runtime.Backend's doc comment). spec.Image, spec.Mounts, and
// spec.CDIDevices are containers-backend concepts and are ignored here: a
// bare-metal process already has direct filesystem and GPU access, with no
// passthrough boundary to cross.
func (b *Backend) Start(ctx context.Context, spec runtime.Spec) (string, error) {
	if spec.BinaryPath == "" {
		return "", fmt.Errorf("no local binary configured for engine type %q - set the matching SPARKY_<ENGINE>_BINARY_PATH on this node (docs/AGENT.md Configuration)", spec.EngineType)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.processes[spec.InstanceID]; exists {
		return "", fmt.Errorf("instance %s already has a tracked process running", spec.InstanceID)
	}

	cmd := exec.Command(spec.BinaryPath, spec.Args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	// Engine server output still reaches journald exactly as before
	// (alongside the agent's own output) via os.Stdout/os.Stderr - logs
	// additionally captures the same bytes into a bounded in-memory
	// buffer (logbuffer.go) so Logs can return real diagnostic evidence
	// for a launch-readiness failure without needing a separate log
	// shipper or journalctl access from this process.
	logs := newLogBuffer()
	cmd.Stdout = io.MultiWriter(os.Stdout, logs)
	cmd.Stderr = io.MultiWriter(os.Stderr, logs)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}

	tp := &trackedProcess{cmd: cmd, done: make(chan struct{}), logs: logs}
	b.processes[spec.InstanceID] = tp
	go func() {
		tp.waitErr = cmd.Wait()
		close(tp.done)
	}()

	return fmt.Sprintf("%d", cmd.Process.Pid), nil
}

// Stop stops the tracked process for instanceID, removing it from
// tracking regardless of whether the stop itself succeeds - a process
// Stop failed to cleanly signal is not retried later, consistent with
// every other real launch/stop failure in this system being reported once
// via InstanceResult and left for an operator to notice, not silently
// retried.
func (b *Backend) Stop(ctx context.Context, instanceID string) error {
	b.mu.Lock()
	tp, exists := b.processes[instanceID]
	if exists {
		delete(b.processes, instanceID)
	}
	b.mu.Unlock()

	if !exists {
		return fmt.Errorf("instance %s has no tracked process", instanceID)
	}
	return stopProcess(tp, stopGracePeriod)
}

// Shutdown stops every process this Backend is still tracking, concurrently,
// and waits for all of them - called once as the agent process exits (see
// agent/connection.Conn.Run). Errors from individual stops are joined
// rather than dropped, so the caller can log the full picture.
func (b *Backend) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	processes := b.processes
	b.processes = make(map[string]*trackedProcess)
	b.mu.Unlock()

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for instanceID, tp := range processes {
		wg.Add(1)
		go func(instanceID string, tp *trackedProcess) {
			defer wg.Done()
			if err := stopProcess(tp, stopGracePeriod); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("instance %s: %w", instanceID, err))
				mu.Unlock()
			}
		}(instanceID, tp)
	}
	wg.Wait()

	return errors.Join(errs...)
}

// IsRunning reports whether instanceID's process is still alive - see
// runtime.Backend's doc comment. A closed tp.done means the reaper
// goroutine already observed cmd.Wait() return (the process exited on its
// own, outside of Stop/Shutdown, e.g. a crash) - the map entry itself
// isn't proof of liveness, only Start/Stop/Shutdown ever remove it, so a
// non-blocking check of done is what actually answers the question.
//
// Deliberately does not remove an already-exited entry on observation
// (an earlier version of this method did) - agent/connection's
// load-readiness check calls IsRunning and, on a false result, immediately
// calls Logs for the same instanceID to build a real diagnostic failure
// message; deleting the entry here would make that Logs call always find
// nothing. The trade-off: a crashed instance nobody ever unloads keeps its
// entry (and up to logBufferCapacity bytes of captured output) in memory
// indefinitely - accepted, since a real crash still surfaces as a
// running_instances row an operator sees and eventually unloads, which
// reaches Stop and removes it same as any other instance.
func (b *Backend) IsRunning(ctx context.Context, instanceID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	tp, exists := b.processes[instanceID]
	if !exists {
		return false, nil
	}

	select {
	case <-tp.done:
		return false, nil
	default:
		return true, nil
	}
}

// Logs returns instanceID's most recently captured combined stdout/stderr,
// up to logBufferCapacity bytes - see runtime.Backend's own doc comment.
// tailLines is accepted for interface parity with the containers backend
// but unused here: logBuffer is a plain byte-capped ring, not line-aware,
// since a bare-metal engine's own output has no framing this package can
// safely split on (unlike Docker's multiplexed stream) - byte-capping
// already keeps this bounded to a reasonable diagnostic size.
func (b *Backend) Logs(ctx context.Context, instanceID string, tailLines int) (string, error) {
	b.mu.Lock()
	tp, exists := b.processes[instanceID]
	b.mu.Unlock()

	if !exists {
		return "", fmt.Errorf("instance %s has no tracked process", instanceID)
	}
	return tp.logs.String(), nil
}

// stopProcess sends SIGTERM and waits up to grace for the process to exit
// on its own before escalating to SIGKILL.
func stopProcess(tp *trackedProcess, grace time.Duration) error {
	if err := tp.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	select {
	case <-tp.done:
		return nil
	case <-time.After(grace):
	}

	if err := tp.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal SIGKILL after %s grace period: %w", grace, err)
	}
	<-tp.done
	return nil
}
