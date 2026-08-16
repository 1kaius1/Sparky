// SPDX-License-Identifier: AGPL-3.0-or-later

package baremetal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/runtime"
)

func TestStart_NoBinaryPath_FailsClearly(t *testing.T) {
	b := New()

	_, err := b.Start(context.Background(), runtime.Spec{InstanceID: "instance-1", EngineType: "llamacpp"})
	if err == nil {
		t.Fatal("Start() succeeded despite an empty BinaryPath")
	}
	if !strings.Contains(err.Error(), "llamacpp") {
		t.Errorf("error %q does not name the engine type", err.Error())
	}
}

func TestStart_Success_ThenStop(t *testing.T) {
	b := New()

	_, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if err := b.Stop(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	b.mu.Lock()
	_, tracked := b.processes["instance-1"]
	b.mu.Unlock()
	if tracked {
		t.Error("instance-1 is still tracked after Stop()")
	}
}

func TestStart_DuplicateInstanceID_FailsClearly(t *testing.T) {
	b := New()

	if _, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	}); err != nil {
		t.Fatalf("first Start() error: %v", err)
	}
	defer b.Stop(context.Background(), "instance-1")

	_, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	})
	if err == nil {
		t.Fatal("second Start() with the same InstanceID succeeded, want an error")
	}
}

func TestStop_UnknownInstanceID_FailsClearly(t *testing.T) {
	b := New()

	if err := b.Stop(context.Background(), "no-such-instance"); err == nil {
		t.Fatal("Stop() succeeded for an instance with no tracked process")
	}
}

func TestStop_IgnoresSIGTERM_EscalatesToSIGKILL(t *testing.T) {
	b := New()
	restore := stopGracePeriodForTest(50 * time.Millisecond)
	defer restore()

	if _, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "trap '' TERM; sleep 30"},
	}); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- b.Stop(context.Background(), "instance-1") }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop() error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5s of a SIGTERM-ignoring process - SIGKILL escalation did not happen")
	}
}

func TestShutdown_StopsAllTrackedProcesses(t *testing.T) {
	b := New()

	for _, id := range []string{"instance-1", "instance-2"} {
		if _, err := b.Start(context.Background(), runtime.Spec{
			InstanceID: id,
			BinaryPath: "/bin/sh",
			Args:       []string{"-c", "sleep 30"},
		}); err != nil {
			t.Fatalf("Start(%s) error: %v", id, err)
		}
	}

	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	b.mu.Lock()
	remaining := len(b.processes)
	b.mu.Unlock()
	if remaining != 0 {
		t.Errorf("processes remaining after Shutdown() = %d, want 0", remaining)
	}
}

func TestShutdown_NoTrackedProcesses_ReturnsNil(t *testing.T) {
	b := New()

	if err := b.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() error: %v, want nil", err)
	}
}

func TestIsRunning_TrackedAndAlive_ReturnsTrue(t *testing.T) {
	b := New()
	if _, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	}); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer b.Stop(context.Background(), "instance-1")

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if !running {
		t.Error("running = false, want true for a just-started, still-alive process")
	}
}

func TestIsRunning_NeverTracked_ReturnsFalse(t *testing.T) {
	b := New()

	running, err := b.IsRunning(context.Background(), "no-such-instance")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if running {
		t.Error("running = true, want false for an instance this Backend never started")
	}
}

func TestIsRunning_TrackedButExited_ReturnsFalseAndCleansUp(t *testing.T) {
	b := New()
	if _, err := b.Start(context.Background(), runtime.Spec{
		InstanceID: "instance-1",
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "true"}, // exits immediately on its own
	}); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	b.mu.Lock()
	tp := b.processes["instance-1"]
	b.mu.Unlock()
	select {
	case <-tp.done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit and get reaped within 5s")
	}

	running, err := b.IsRunning(context.Background(), "instance-1")
	if err != nil {
		t.Fatalf("IsRunning() error: %v", err)
	}
	if running {
		t.Error("running = true, want false for a process that already exited on its own")
	}

	b.mu.Lock()
	_, stillTracked := b.processes["instance-1"]
	b.mu.Unlock()
	if stillTracked {
		t.Error("instance-1 is still in processes after IsRunning() observed it had exited - want the stale entry cleaned up")
	}
}

// stopGracePeriodForTest temporarily shrinks the package-level grace period
// so SIGKILL-escalation tests don't need to block for the real 15s.
// Restores the original value; not safe for concurrent use across tests
// (package tests in this file run sequentially, not in parallel, so this is
// fine).
func stopGracePeriodForTest(d time.Duration) (restore func()) {
	orig := stopGracePeriod
	stopGracePeriod = d
	return func() { stopGracePeriod = orig }
}
