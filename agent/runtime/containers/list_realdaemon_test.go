// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build realdaemon

package containers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRealDaemon_ListManagedAndUnforcedRemoval needs a container daemon but no
// GPU. It shows two things a fake cannot: that ListManaged finds exactly
// Sparky's containers (labelled, and legacy-named) among a daemon's others,
// and that Remove, which never forces, is refused by the daemon for a running
// container - the backstop that makes a stale-cleanup race harmless.
//
//	DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
//	    SPARKY_REALDAEMON_IMAGE=docker.io/library/postgres:16 \
//	    go test -tags realdaemon -run TestRealDaemon_ListManaged -v ./agent/runtime/containers
func TestRealDaemon_ListManagedAndUnforcedRemoval(t *testing.T) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		t.Skip("DOCKER_HOST is not set - no container daemon to test against")
	}
	image := os.Getenv("SPARKY_REALDAEMON_IMAGE")
	if image == "" {
		image = "docker.io/library/ubuntu:24.04"
	}
	runtimeCLI := "podman"
	if _, err := exec.LookPath(runtimeCLI); err != nil {
		runtimeCLI = "docker"
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer b.Close()

	const (
		exitedID  = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
		legacyID  = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
		runningID = "cccccccc-3333-4333-8333-cccccccccccc"
	)
	cli := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(runtimeCLI, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", runtimeCLI, args, err, out)
		}
	}
	names := []string{"sparky-realtest-exited", "sparky-instance-" + legacyID, "sparky-realtest-running", "my-sparky-instance-" + legacyID, "sparky-instance-" + legacyID + "-backup"}
	cleanup := func() {
		for _, n := range names {
			_ = exec.Command(runtimeCLI, "rm", "-f", n).Run()
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	// Detached and then waited for, because the CLI's own exit status would
	// otherwise be the container's (1 for the deliberately failing one).
	runToExit := func(name string, args ...string) {
		t.Helper()
		cli(append([]string{"run", "-d", "--name", name}, args...)...)
		cli("wait", name)
	}
	runToExit(names[0], "--label", "sparky.managed=true", "--label", "sparky.instance_id="+exitedID, "--entrypoint", "/bin/false", image)
	runToExit(names[1], "--entrypoint", "/bin/true", image)
	cli("run", "-d", "--name", names[2], "--label", "sparky.managed=true", "--label", "sparky.instance_id="+runningID, "--entrypoint", "sleep", image, "300")
	// Lookalikes that are not Sparky's and must never be listed.
	runToExit(names[3], "--entrypoint", "/bin/true", image)
	runToExit(names[4], "--entrypoint", "/bin/true", image)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, truncated, err := b.ListManaged(ctx)
	if err != nil || truncated {
		t.Fatalf("ListManaged() = %v, truncated %v", err, truncated)
	}
	byID := map[string]string{}
	for _, c := range got {
		byID[c.InstanceID] = fmt.Sprintf("%s labelled=%v", c.State, c.Labelled)
		if c.ContainerName == names[3] || c.ContainerName == names[4] {
			t.Errorf("listed a container that is not Sparky's: %+v", c)
		}
	}
	if byID[exitedID] != "exited labelled=true" || byID[legacyID] != "exited labelled=false" || byID[runningID] != "running labelled=true" {
		t.Errorf("listed %v, want the exited, legacy and running containers described", byID)
	}
	for _, c := range got {
		if c.InstanceID == exitedID && (c.ExitCode == nil || *c.ExitCode != 1 || c.FinishedAt.IsZero()) {
			t.Errorf("exited container's exit details = %+v, want exit code 1 and a finish time", c)
		}
		if c.InstanceID == runningID && (c.ExitCode != nil || !c.FinishedAt.IsZero()) {
			t.Errorf("running container reports exit details: %+v", c)
		}
	}

	// The backstop: an unforced removal of a running container is refused.
	if err := b.Remove(ctx, runningID); err == nil {
		t.Error("Remove() of a running container succeeded; the daemon must refuse an unforced removal")
	}
	if running, err := b.IsRunning(ctx, runningID); err != nil || !running {
		t.Errorf("after the refused removal IsRunning() = %v, %v, want still running", running, err)
	}
	// And a stopped one is removed.
	if err := b.Remove(ctx, exitedID); err != nil {
		t.Errorf("Remove() of the exited container: %v", err)
	}
	after, _, _ := b.ListManaged(ctx)
	for _, c := range after {
		if c.InstanceID == exitedID {
			t.Error("the removed container is still listed")
		}
	}
}
