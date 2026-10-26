// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

const (
	instA = "11111111-1111-1111-1111-111111111111"
	instB = "22222222-2222-2222-2222-222222222222"
	instC = "33333333-3333-3333-3333-333333333333"
	profP = "99999999-9999-9999-9999-999999999999"
)

func managedSummary(id, instance, state string, created int64) container.Summary {
	return container.Summary{
		ID:      id,
		Names:   []string{"/sparky-model-20261010-101010"},
		Created: created,
		State:   container.ContainerState(state),
		Labels: map[string]string{
			labelManaged:    "true",
			labelInstanceID: instance,
			labelProfileID:  profP,
		},
	}
}

func exitedInspect(code int, oom bool, finished string) client.ContainerInspectResult {
	return client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{
		Status: container.StateExited, ExitCode: code, OOMKilled: oom, FinishedAt: finished,
	}}}
}

func TestListManaged_DescribesLabelledAndLegacyContainers(t *testing.T) {
	legacy := container.Summary{ID: "legacy1", Names: []string{"/sparky-instance-" + instC}, Created: 50, State: container.StateExited}
	fake := &fakeDockerClient{
		listResult: client.ContainerListResult{Items: []container.Summary{
			managedSummary("run1", instA, "running", 300),
			managedSummary("dead1", instB, "exited", 200),
			legacy,
		}},
		inspectFunc: func(ref string) (client.ContainerInspectResult, error) {
			switch ref {
			case "dead1":
				return exitedInspect(137, true, "2026-10-20T08:00:00.5Z"), nil
			case "legacy1":
				return exitedInspect(1, false, "2026-09-01T00:00:00Z"), nil
			}
			return client.ContainerInspectResult{}, fmt.Errorf("a running container must not be inspected: %s", ref)
		},
	}
	got, truncated, err := (&Backend{cli: fake}).ListManaged(context.Background())
	if err != nil || truncated {
		t.Fatalf("ListManaged() = %v, truncated %v", err, truncated)
	}
	if len(got) != 3 {
		t.Fatalf("%d containers, want 3: %+v", len(got), got)
	}
	// Oldest first.
	if got[0].InstanceID != instC || got[1].InstanceID != instB || got[2].InstanceID != instA {
		t.Errorf("order = %s, %s, %s; want oldest first", got[0].InstanceID, got[1].InstanceID, got[2].InstanceID)
	}

	l := got[0]
	if l.Labelled || l.ContainerName != "sparky-instance-"+instC || l.State != "exited" || l.ExitCode == nil || *l.ExitCode != 1 || l.ProfileID != "" {
		t.Errorf("legacy = %+v", l)
	}
	d := got[1]
	if !d.Labelled || d.ProfileID != profP || d.ExitCode == nil || *d.ExitCode != 137 || !d.OOMKilled || d.FinishedAt.IsZero() || d.FinishedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-20T08:00:00Z" {
		t.Errorf("exited labelled = %+v", d)
	}
	r := got[2]
	if r.State != "running" || r.ExitCode != nil || !r.FinishedAt.IsZero() || r.ContainerName != "sparky-model-20261010-101010" {
		t.Errorf("running = %+v", r)
	}
	for _, ref := range fake.inspectRefs {
		if ref == "run1" {
			t.Error("a running container was inspected for exit details it cannot have")
		}
	}
}

func TestListManaged_OnlyCountsWhatMatchesExactly(t *testing.T) {
	probe := managedSummary("probe1", instA, "exited", 10)
	delete(probe.Labels, labelManaged)
	probe.Labels[labelProbe] = "true"

	items := []container.Summary{
		probe,
		// Someone else's containers: a substring and a regexp match of the
		// legacy name filter, and an unrelated label.
		{ID: "other1", Names: []string{"/my-sparky-instance-" + instA}, State: container.StateExited},
		{ID: "other2", Names: []string{"/sparky-instance-" + instA + "-backup"}, State: container.StateExited},
		{ID: "other3", Names: []string{"/redis"}, Labels: map[string]string{"app": "cache"}, State: container.StateExited},
		// Managed label but no usable instance id: cannot be attributed.
		{ID: "noid", Names: []string{"/x"}, Labels: map[string]string{labelManaged: "true"}, State: container.StateExited},
		{ID: "badid", Names: []string{"/x"}, Labels: map[string]string{labelManaged: "true", labelInstanceID: "a b;c"}, State: container.StateExited},
		// Managed=false is not managed.
		{ID: "notmanaged", Names: []string{"/x"}, Labels: map[string]string{labelManaged: "false", labelInstanceID: instB}, State: container.StateExited},
		managedSummary("ok1", instC, "exited", 20),
	}
	fake := &fakeDockerClient{
		listResult:    client.ContainerListResult{Items: items},
		inspectResult: exitedInspect(0, false, "2026-10-20T08:00:00Z"),
	}
	got, _, err := (&Backend{cli: fake}).ListManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContainerID != "ok1" {
		t.Errorf("got %+v, want only ok1", got)
	}
}

func TestListManaged_MergesTheTwoQueriesWithoutDuplicates(t *testing.T) {
	// The fake answers both daemon queries with the same list.
	fake := &fakeDockerClient{
		listResult:    client.ContainerListResult{Items: []container.Summary{managedSummary("ok1", instA, "exited", 20)}},
		inspectResult: exitedInspect(0, false, "2026-10-20T08:00:00Z"),
	}
	got, _, err := (&Backend{cli: fake}).ListManaged(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v; want the container once", got, err)
	}
	if len(fake.listOptions) != 2 {
		t.Fatalf("%d list queries, want 2 (managed label, legacy name)", len(fake.listOptions))
	}
	for _, o := range fake.listOptions {
		if !o.All {
			t.Error("a list query left out stopped containers")
		}
	}
}

func TestListManaged_AContainerRemovedMeanwhileIsSkippedButOtherErrorsAreReported(t *testing.T) {
	items := []container.Summary{managedSummary("gone1", instA, "exited", 10), managedSummary("ok1", instB, "exited", 20)}
	fake := &fakeDockerClient{
		listResult: client.ContainerListResult{Items: items},
		inspectFunc: func(ref string) (client.ContainerInspectResult, error) {
			if ref == "gone1" {
				return client.ContainerInspectResult{}, cerrdefs.ErrNotFound
			}
			return exitedInspect(0, false, "2026-10-20T08:00:00Z"), nil
		},
	}
	got, _, err := (&Backend{cli: fake}).ListManaged(context.Background())
	if err != nil || len(got) != 1 || got[0].ContainerID != "ok1" {
		t.Fatalf("got %+v, %v; want only ok1", got, err)
	}

	fake.inspectFunc = func(string) (client.ContainerInspectResult, error) {
		return client.ContainerInspectResult{}, errors.New("daemon hiccup")
	}
	if _, _, err := (&Backend{cli: fake}).ListManaged(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon hiccup") {
		t.Errorf("an inspect failure = %v, want it reported rather than a partial list", err)
	}

	fake.listErr = errors.New("daemon unreachable")
	if _, _, err := (&Backend{cli: fake}).ListManaged(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon unreachable") {
		t.Errorf("a list failure = %v", err)
	}
}

func TestListManaged_CapsTheListAndSaysSo(t *testing.T) {
	var items []container.Summary
	for i := 0; i < runtime.MaxManaged+25; i++ {
		items = append(items, managedSummary(fmt.Sprintf("c%04d", i), fmt.Sprintf("%08d-0000-0000-0000-000000000000", i), "exited", int64(1000+i)))
	}
	fake := &fakeDockerClient{
		listResult:    client.ContainerListResult{Items: items},
		inspectResult: exitedInspect(0, false, "2026-10-20T08:00:00Z"),
	}
	got, truncated, err := (&Backend{cli: fake}).ListManaged(context.Background())
	if err != nil || !truncated || len(got) != runtime.MaxManaged {
		t.Fatalf("got %d containers, truncated %v, err %v; want %d and truncated", len(got), truncated, err, runtime.MaxManaged)
	}
	if got[0].ContainerID != "c0000" {
		t.Errorf("first = %s, want the oldest kept", got[0].ContainerID)
	}
}
