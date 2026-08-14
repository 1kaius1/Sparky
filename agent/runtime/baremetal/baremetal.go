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
	// reading it after <-done is race-free). Deliberately captured rather
	// than silently discarded even though nothing consumes it today -
	// there is no crash-detection/restart feature yet for a bare-metal
	// engine process that exits on its own outside of Stop/Shutdown; a
	// future health-reporting pass has a value to read here instead of
	// needing to add this plumbing from scratch.
	waitErr error
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
	// Engine server output has no other destination configured anywhere
	// in this project (LOG_LEVEL/LOG_FORMAT govern the agent's own
	// structured logging, not a managed child's) - surfaced via journald
	// alongside the agent's own output, same as the agent's own stderr.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}

	tp := &trackedProcess{cmd: cmd, done: make(chan struct{})}
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
