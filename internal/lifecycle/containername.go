// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

const (
	// containerNamePrefix marks a container as Sparky's in `docker ps` and
	// keeps the profile-derived part from being mistaken for an image name.
	containerNamePrefix = "sparky-"
	// containerNameRefLayout is the REF part: the instance's start time in
	// UTC, sortable and readable, so which of several containers for one
	// profile is the newest is visible at a glance. UTC rather than the
	// node's local time, so a fleet in different timezones, or DST, never
	// makes two names ambiguous.
	containerNameRefLayout = "20060102-150405"
	// maxContainerNameProfileLen bounds the profile-derived part so a long
	// free-text profile name cannot produce an unwieldy or rejected name.
	maxContainerNameProfileLen = 40
)

// containerName builds the container name for a launch:
// sparky-<sanitized profile name>-<UTC start time>. A profile name is free
// text (spaces, slashes, unicode are all allowed), while Docker and Podman
// only accept [a-zA-Z0-9][a-zA-Z0-9_.-]*, so the profile part is sanitized
// rather than rejected. Two profiles may sanitize to the same text; the
// timestamp, and the instance-id label the agent also sets, keep them
// distinguishable.
func containerName(profileName string, startedAt time.Time) string {
	return containerNamePrefix + sanitizeContainerNamePart(profileName) + "-" + startedAt.UTC().Format(containerNameRefLayout)
}

func sanitizeContainerNamePart(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		// Everything else, including a literal '-', becomes one '-' per run.
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > maxContainerNameProfileLen {
		out = strings.Trim(out[:maxContainerNameProfileLen], "-_.")
	}
	if out == "" {
		return "profile"
	}
	return out
}

// launchTime is the time embedded in a launch's container name: the running
// instance's own started_at, so the name matches the start time shown in the
// UI. The row's default fills it on insert; the fallback only covers a store
// that returns it unset.
func launchTime(inst *db.RunningInstance) time.Time {
	if inst.StartedAt.IsZero() {
		return time.Now()
	}
	return inst.StartedAt
}
