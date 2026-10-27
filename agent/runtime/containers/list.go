// SPDX-License-Identifier: AGPL-3.0-or-later

package containers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/1kaius1/Sparky/agent/runtime"
)

// instanceUUID is the exact shape of an instance id (a running_instances
// uuid). Stale container monitoring may act on what ListManaged returns, so
// "is this Sparky's" must not rest on a loose pattern: a container someone
// else named sparky-instance-<uuid>-backup is not ours.
const instanceUUID = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

var (
	validInstanceID = regexp.MustCompile(`^` + instanceUUID + `$`)

	// legacyContainerName matches the name older agents gave every container,
	// before the instance-id label existed (InstanceContainerName).
	legacyContainerName = regexp.MustCompile(`^/?sparky-instance-(` + instanceUUID + `)$`)
)

// ListManaged returns every container Sparky started on this host, running or
// not - see runtime.Backend.ListManaged. It never changes anything.
//
// Two daemon queries find them (the managed label, and the legacy name), but
// what is returned is decided here from each container's own labels and names,
// not from the daemon's filtering: a name filter is a substring match on
// Docker and a regular expression on Podman, and the central app may later act
// on this list, so a container only counts when it matches exactly. The
// diagnostic probe (agent/gpucheck) has the probe label and no managed one, so
// it is never listed.
func (b *Backend) ListManaged(ctx context.Context) ([]runtime.ManagedContainer, bool, error) {
	seen := make(map[string]bool)
	var found []container.Summary
	for _, f := range []client.Filters{
		make(client.Filters).Add("label", labelManaged+"=true"),
		make(client.Filters).Add("name", "sparky-instance-"),
	} {
		res, err := b.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
		if err != nil {
			return nil, false, fmt.Errorf("list containers: %w", err)
		}
		for _, s := range res.Items {
			if !seen[s.ID] {
				seen[s.ID] = true
				found = append(found, s)
			}
		}
	}

	var out []runtime.ManagedContainer
	for _, s := range found {
		mc, ok := managedFromSummary(s)
		if !ok {
			continue
		}
		if !stillLive(mc.State) {
			// A stopped container's exit code, OOM flag and finish time are
			// not in the list reply; the finish time is what the central app
			// ages it by.
			inspected, err := b.cli.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					// Removed since the list was taken: nothing to report.
					continue
				}
				return nil, false, fmt.Errorf("inspect container %s: %w", s.ID, err)
			}
			var c runtime.Capture
			applyState(&c, inspected.Container.State)
			mc.ExitCode, mc.OOMKilled, mc.FinishedAt = c.ExitCode, c.OOMKilled, c.FinishedAt
		}
		out = append(out, mc)
	}

	// Oldest first, so a host that hits the cap loses its newest containers,
	// which are the ones least likely to be stale.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].InstanceID < out[j].InstanceID
	})
	truncated := false
	if len(out) > runtime.MaxManaged {
		out, truncated = out[:runtime.MaxManaged], true
	}
	return out, truncated, nil
}

// stillLive reports whether a container state means it may be doing work (or
// about to): such a container's exit details are meaningless and it is never
// a candidate for removal.
func stillLive(state string) bool {
	switch container.ContainerState(state) {
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return true
	}
	return false
}

// managedFromSummary decides whether a listed container is Sparky's and, if
// so, which instance it belongs to. A container with the managed label counts
// only if it also carries a well-formed instance id; one without the label
// counts only if its name is exactly the legacy one.
func managedFromSummary(s container.Summary) (runtime.ManagedContainer, bool) {
	if _, probe := s.Labels[labelProbe]; probe {
		return runtime.ManagedContainer{}, false
	}
	mc := runtime.ManagedContainer{
		ContainerID: s.ID,
		State:       string(s.State),
		CreatedAt:   time.Unix(s.Created, 0).UTC(),
	}
	if len(s.Names) > 0 {
		mc.ContainerName = strings.TrimPrefix(s.Names[0], "/")
	}
	if s.Labels[labelManaged] == "true" {
		id := s.Labels[labelInstanceID]
		if !validInstanceID.MatchString(id) {
			return runtime.ManagedContainer{}, false
		}
		mc.InstanceID, mc.Labelled = id, true
		if p := s.Labels[labelProfileID]; validProfileID.MatchString(p) {
			mc.ProfileID = p
		}
		return mc, true
	}
	for _, name := range s.Names {
		if m := legacyContainerName.FindStringSubmatch(name); m != nil {
			mc.InstanceID = m[1]
			return mc, true
		}
	}
	return runtime.ManagedContainer{}, false
}
