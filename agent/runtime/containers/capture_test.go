// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

func inspectOf(name string, st *container.State) client.ContainerInspectResult {
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: "abc123", Name: "/" + name, State: st}}
}

func TestCapture_ReturnsLogAndExitReason(t *testing.T) {
	var stream []byte
	stream = append(stream, stdcopyFrame(1, "loading model\n")...)
	stream = append(stream, stdcopyFrame(2, "CUDA out of memory\n")...)
	fake := &fakeDockerClient{
		logsResult: io.NopCloser(bytes.NewReader(stream)),
		inspectResult: inspectOf("sparky-p-20261009-010203", &container.State{
			Status: container.StateExited, ExitCode: 137, OOMKilled: true,
			StartedAt: "2026-10-09T01:02:03.5Z", FinishedAt: "2026-10-09T01:09:00Z",
		}),
	}
	b := &Backend{cli: fake}

	c, err := b.Capture(context.Background(), "instance-1", 2000)
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}
	if c.Log != "loading model\nCUDA out of memory\n" || c.LinesKept != 2 || c.Truncated {
		t.Errorf("log = %q lines %d truncated %v", c.Log, c.LinesKept, c.Truncated)
	}
	if c.ContainerID != "abc123" || c.ContainerName != "sparky-p-20261009-010203" || c.State != "exited" || !c.OOMKilled {
		t.Errorf("capture = %+v", c)
	}
	if c.ExitCode == nil || *c.ExitCode != 137 {
		t.Errorf("exit code = %v, want 137", c.ExitCode)
	}
	if c.StartedAt.IsZero() || c.FinishedAt.IsZero() || !c.FinishedAt.After(c.StartedAt) {
		t.Errorf("times = %v / %v", c.StartedAt, c.FinishedAt)
	}
	if len(fake.logsOptions) != 1 || fake.logsOptions[0].Tail != "2000" || !fake.logsOptions[0].ShowStdout || !fake.logsOptions[0].ShowStderr {
		t.Errorf("log options = %+v, want the last 2000 lines of both streams", fake.logsOptions)
	}
}

func TestCapture_ZeroLinesMeansAll(t *testing.T) {
	fake := &fakeDockerClient{inspectResult: inspectOf("n", &container.State{Status: container.StateExited})}
	b := &Backend{cli: fake}
	if _, err := b.Capture(context.Background(), "instance-1", 0); err != nil {
		t.Fatal(err)
	}
	if fake.logsOptions[0].Tail != "all" {
		t.Errorf("Tail = %q, want all", fake.logsOptions[0].Tail)
	}
}

// A running or never-started container reports exit code 0 meaninglessly;
// Capture must not present that as an exit reason.
func TestCapture_ExitCodeOnlyForAContainerThatExited(t *testing.T) {
	for name, st := range map[string]*container.State{
		"running":    {Status: container.StateRunning, Running: true},
		"created":    {Status: container.StateCreated},
		"restarting": {Status: container.StateRestarting, Restarting: true},
	} {
		t.Run(name, func(t *testing.T) {
			b := &Backend{cli: &fakeDockerClient{inspectResult: inspectOf("n", st)}}
			c, err := b.Capture(context.Background(), "instance-1", 10)
			if err != nil {
				t.Fatal(err)
			}
			if c.ExitCode != nil {
				t.Errorf("exit code = %d, want unset for a %s container", *c.ExitCode, name)
			}
		})
	}
	// A clean exit is a real 0.
	b := &Backend{cli: &fakeDockerClient{inspectResult: inspectOf("n", &container.State{Status: container.StateExited, ExitCode: 0})}}
	c, _ := b.Capture(context.Background(), "instance-1", 10)
	if c.ExitCode == nil || *c.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0 for a container that exited cleanly", c.ExitCode)
	}
}

func TestCapture_MissingContainerIsNothingToCapture(t *testing.T) {
	gone := cerrdefs.ErrNotFound.WithMessage("no such container")
	for name, fake := range map[string]*fakeDockerClient{
		"inspect not found": {inspectErr: gone},
		"logs not found":    {inspectResult: inspectOf("n", &container.State{Status: container.StateExited}), logsErr: gone},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Backend{cli: fake}).Capture(context.Background(), "instance-1", 10)
			if !errors.Is(err, runtime.ErrNothingToCapture) {
				t.Errorf("err = %v, want ErrNothingToCapture", err)
			}
		})
	}
}

func TestCapture_OtherErrorsAreErrors(t *testing.T) {
	_, err := (&Backend{cli: &fakeDockerClient{inspectErr: errors.New("daemon unreachable")}}).Capture(context.Background(), "instance-1", 10)
	if err == nil || errors.Is(err, runtime.ErrNothingToCapture) {
		t.Errorf("err = %v, want a real error that is not ErrNothingToCapture", err)
	}
	fake := &fakeDockerClient{inspectResult: inspectOf("n", &container.State{Status: container.StateExited}), logsErr: errors.New("log driver broken")}
	if _, err := (&Backend{cli: fake}).Capture(context.Background(), "instance-1", 10); err == nil || errors.Is(err, runtime.ErrNothingToCapture) {
		t.Errorf("err = %v, want a real error", err)
	}
}

func TestCapture_OverTheSizeCapKeepsTheNewestAndSaysSo(t *testing.T) {
	old := strings.Repeat("o", maxCaptureBytes) // alone fills the cap
	var stream []byte
	stream = append(stream, stdcopyFrame(1, old)...)
	stream = append(stream, stdcopyFrame(1, "newest line\n")...)
	fake := &fakeDockerClient{
		logsResult:    io.NopCloser(bytes.NewReader(stream)),
		inspectResult: inspectOf("n", &container.State{Status: container.StateExited}),
	}
	c, err := (&Backend{cli: fake}).Capture(context.Background(), "instance-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Truncated || len(c.Log) != maxCaptureBytes || !strings.HasSuffix(c.Log, "newest line\n") {
		t.Errorf("truncated=%v len=%d tail=%q", c.Truncated, len(c.Log), c.Log[len(c.Log)-20:])
	}
}

func TestCountLines(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n": 1, "\n\n": 2} {
		if got := countLines(in); got != want {
			t.Errorf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{limit: 5}
	tb.Write([]byte("abc"))
	if tb.String() != "abc" || tb.dropped {
		t.Fatalf("after first write: %q dropped=%v", tb.String(), tb.dropped)
	}
	tb.Write([]byte("defg"))
	if tb.String() != "cdefg" || !tb.dropped {
		t.Errorf("after overflow: %q dropped=%v, want cdefg", tb.String(), tb.dropped)
	}
	tb.Write([]byte("0123456789"))
	if tb.String() != "56789" {
		t.Errorf("after oversized write: %q, want 56789", tb.String())
	}
}

// Halt must stop the container and leave it in place so its log can still be
// read; Remove is the separate, later step.
func TestHaltLeavesTheContainerAndRemoveDeletesIt(t *testing.T) {
	fake := &fakeDockerClient{}
	b := &Backend{cli: fake}
	if err := b.Halt(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Halt() error: %v", err)
	}
	if len(fake.stopRefs) != 1 || len(fake.removeRefs) != 0 {
		t.Errorf("after Halt: stops=%v removes=%v, want one stop and no remove", fake.stopRefs, fake.removeRefs)
	}
	if err := b.Remove(context.Background(), "instance-1"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if len(fake.removeRefs) != 1 || len(fake.stopRefs) != 1 {
		t.Errorf("after Remove: stops=%v removes=%v", fake.stopRefs, fake.removeRefs)
	}
}

func TestHaltAndRemove_AlreadyGoneIsNotAnError(t *testing.T) {
	gone := cerrdefs.ErrNotFound.WithMessage("no such container")
	b := &Backend{cli: &fakeDockerClient{stopErr: gone, removeErr: gone}}
	if err := b.Halt(context.Background(), "instance-1"); err != nil {
		t.Errorf("Halt() error: %v", err)
	}
	if err := b.Remove(context.Background(), "instance-1"); err != nil {
		t.Errorf("Remove() error: %v", err)
	}
}

func TestHaltAndRemove_OtherErrorsAreErrors(t *testing.T) {
	denied := cerrdefs.ErrPermissionDenied.WithMessage("denied")
	if err := (&Backend{cli: &fakeDockerClient{stopErr: denied}}).Halt(context.Background(), "i"); err == nil {
		t.Error("Halt() swallowed a non-not-found error")
	}
	if err := (&Backend{cli: &fakeDockerClient{removeErr: denied}}).Remove(context.Background(), "i"); err == nil {
		t.Error("Remove() swallowed a non-not-found error")
	}
}

func TestStart_LabelsTheContainerWithTheProfileID(t *testing.T) {
	got := startCapturing(t, runtime.Spec{InstanceID: "instance-1", Image: "img", ProfileID: "11111111-aaaa-bbbb-cccc-000000000001"})
	if got.Config.Labels[labelProfileID] != "11111111-aaaa-bbbb-cccc-000000000001" {
		t.Errorf("Labels = %v, want the profile id label", got.Config.Labels)
	}
	if got.Config.Labels[labelInstanceID] != "instance-1" || got.Config.Labels[labelManaged] != "true" {
		t.Errorf("Labels = %v, want the existing labels kept", got.Config.Labels)
	}
}

// An absent or malformed id must not become a label, since it later becomes a
// daemon filter value.
func TestStart_NoProfileLabelWithoutAValidProfileID(t *testing.T) {
	for _, id := range []string{"", "short", "has space in it 123", "bad=value-1234", strings.Repeat("a", 65)} {
		got := startCapturing(t, runtime.Spec{InstanceID: "instance-1", Image: "img", ProfileID: id})
		if _, ok := got.Config.Labels[labelProfileID]; ok {
			t.Errorf("ProfileID %q produced the label %q", id, got.Config.Labels[labelProfileID])
		}
	}
}

func TestInstancesForProfile_ListsEveryManagedContainerOfTheProfile(t *testing.T) {
	fake := &fakeDockerClient{listResult: client.ContainerListResult{Items: []container.Summary{
		{ID: "c1", Labels: map[string]string{labelInstanceID: "inst-1", labelProfileID: "p"}},
		{ID: "c2", Labels: map[string]string{labelInstanceID: "inst-2", labelProfileID: "p"}},
		{ID: "c3", Labels: map[string]string{labelProfileID: "p"}}, // no instance label: not something to act on
	}}}
	ids, err := (&Backend{cli: fake}).InstancesForProfile(context.Background(), "11111111-aaaa-bbbb-cccc-000000000001")
	if err != nil {
		t.Fatalf("InstancesForProfile() error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "inst-1" || ids[1] != "inst-2" {
		t.Errorf("ids = %v, want [inst-1 inst-2]", ids)
	}
	if len(fake.listOptions) != 1 || !fake.listOptions[0].All {
		t.Fatalf("list options = %+v, want one call including stopped containers", fake.listOptions)
	}
	f := fake.listOptions[0].Filters
	if !f["label"]["sparky.managed=true"] || !f["label"]["sparky.profile_id=11111111-aaaa-bbbb-cccc-000000000001"] {
		t.Errorf("filters = %v, want managed and profile labels", f)
	}
}

func TestInstancesForProfile_InvalidIDMatchesNothingWithoutAskingTheDaemon(t *testing.T) {
	fake := &fakeDockerClient{}
	ids, err := (&Backend{cli: fake}).InstancesForProfile(context.Background(), "x=y")
	if err != nil || len(ids) != 0 || len(fake.listOptions) != 0 {
		t.Errorf("ids=%v err=%v list calls=%d, want nothing", ids, err, len(fake.listOptions))
	}
}

func TestInstancesForProfile_ListFailureIsAnError(t *testing.T) {
	fake := &fakeDockerClient{listErr: errors.New("daemon unreachable")}
	if _, err := (&Backend{cli: fake}).InstancesForProfile(context.Background(), "11111111-aaaa-bbbb-cccc-000000000001"); err == nil {
		t.Error("InstancesForProfile() swallowed a list failure")
	}
}
