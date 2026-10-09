// SPDX-License-Identifier: AGPL-3.0-or-later

package baremetal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/runtime"
)

func startShell(t *testing.T, b *Backend, id, script string) {
	t.Helper()
	if _, err := b.Start(context.Background(), runtime.Spec{InstanceID: id, BinaryPath: "/bin/sh", Args: []string{"-c", script}}); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
}

func waitExited(t *testing.T, b *Backend, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := b.IsRunning(context.Background(), id); !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("process did not exit")
}

// Halt stops the process but keeps the record and its output, so Capture can
// still read what it printed - the whole point of splitting Stop apart.
func TestHaltKeepsTheRecordSoCaptureCanReadIt(t *testing.T) {
	b := New()
	startShell(t, b, "i-1", "echo first; echo second >&2; exec sleep 30")
	// give the shell a moment to print
	time.Sleep(150 * time.Millisecond)

	if err := b.Halt(context.Background(), "i-1"); err != nil {
		t.Fatalf("Halt() error: %v", err)
	}
	if running, _ := b.IsRunning(context.Background(), "i-1"); running {
		t.Error("process still running after Halt")
	}
	c, err := b.Capture(context.Background(), "i-1", 0)
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}
	if !strings.Contains(c.Log, "first") || !strings.Contains(c.Log, "second") || c.LinesKept != 2 || c.Truncated {
		t.Errorf("capture log = %q lines %d truncated %v", c.Log, c.LinesKept, c.Truncated)
	}
	if c.State != "exited" || c.ExitCode == nil || *c.ExitCode != -1 {
		t.Errorf("state %q exit %v, want exited and -1 (killed by a signal)", c.State, c.ExitCode)
	}
	if c.OOMKilled || c.ContainerName != "" || c.ContainerID == "" {
		t.Errorf("capture = %+v", c)
	}

	if err := b.Remove(context.Background(), "i-1"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if _, err := b.Capture(context.Background(), "i-1", 0); !errors.Is(err, runtime.ErrNothingToCapture) {
		t.Errorf("Capture after Remove: err = %v, want ErrNothingToCapture", err)
	}
}

func TestCapture_ProcessThatExitedOnItsOwnReportsItsExitCode(t *testing.T) {
	b := New()
	startShell(t, b, "i-1", "echo boom; exit 3")
	waitExited(t, b, "i-1")

	c, err := b.Capture(context.Background(), "i-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "exited" || c.ExitCode == nil || *c.ExitCode != 3 || !strings.Contains(c.Log, "boom") {
		t.Errorf("capture = %+v", c)
	}
}

func TestCapture_RunningProcessHasNoExitCode(t *testing.T) {
	b := New()
	startShell(t, b, "i-1", "echo up; exec sleep 30")
	defer b.Stop(context.Background(), "i-1")
	time.Sleep(100 * time.Millisecond)

	c, err := b.Capture(context.Background(), "i-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "running" || c.ExitCode != nil {
		t.Errorf("state %q exit %v, want running with no exit code", c.State, c.ExitCode)
	}
}

func TestCapture_UnknownInstanceIsNothingToCapture(t *testing.T) {
	if _, err := New().Capture(context.Background(), "nope", 10); !errors.Is(err, runtime.ErrNothingToCapture) {
		t.Errorf("err = %v, want ErrNothingToCapture", err)
	}
}

func TestHaltAndRemove_UnknownInstanceIsNotAnError(t *testing.T) {
	b := New()
	if err := b.Halt(context.Background(), "nope"); err != nil {
		t.Errorf("Halt() error: %v", err)
	}
	if err := b.Remove(context.Background(), "nope"); err != nil {
		t.Errorf("Remove() error: %v", err)
	}
}

// The buffer is a plain byte ring, so once it has wrapped the first line is
// cut mid-way: Capture drops it and says output was lost.
func TestCapture_WrappedBufferDropsThePartialFirstLineAndSaysTruncated(t *testing.T) {
	b := New()
	startShell(t, b, "i-1", "i=0; while [ $i -lt 2500 ]; do echo line-$i-padding-padding; i=$((i+1)); done")
	waitExited(t, b, "i-1")

	c, err := b.Capture(context.Background(), "i-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Truncated {
		t.Fatal("Truncated = false for a buffer that overflowed")
	}
	if !strings.HasPrefix(c.Log, "line-") || !strings.HasSuffix(c.Log, "line-2499-padding-padding\n") {
		t.Errorf("log starts %q ends %q", c.Log[:20], c.Log[len(c.Log)-30:])
	}
}

func TestCapture_LinesLimitsToTheLastN(t *testing.T) {
	b := New()
	startShell(t, b, "i-1", "for i in 1 2 3 4 5; do echo l$i; done")
	waitExited(t, b, "i-1")

	c, err := b.Capture(context.Background(), "i-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if c.Log != "l4\nl5\n" || c.LinesKept != 2 {
		t.Errorf("log = %q lines %d, want the last two lines", c.Log, c.LinesKept)
	}
}

func TestLastLines(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 0, "a\nb\nc\n"},
		{"a\nb\nc\n", 1, "c\n"},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc\n", 3, "a\nb\nc\n"},
		{"a\nb\nc\n", 9, "a\nb\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"", 3, ""},
		{"only", 1, "only"},
	}
	for _, tc := range cases {
		if got := lastLines(tc.in, tc.n); got != tc.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
