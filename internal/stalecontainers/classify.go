// SPDX-License-Identifier: AGPL-3.0-or-later

// Package stalecontainers finds the containers a node's runtime still holds
// for instances that have ended, reports them as alerts, and removes them on a
// schedule or on request. A container is stale when its instance is over and
// it has sat stopped for the configured minimum age; see Classify for the exact
// rules, which are the safety boundary of the whole feature.
//
// The server decides and triggers every removal. The agent only describes what
// it holds and acts on a named instance, re-checking the rules that keep a
// live instance safe (agent/connection's removeStale). A container is removed
// only after its log has been archived and the central app has confirmed it
// stored it, so nothing that explains a failure is lost with it. See
// PLANNING.md's 2026-10-08 and 2026-10-26 Decisions Log entries.
package stalecontainers

import (
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

// Verdict is what Classify decides about one container.
type Verdict string

const (
	// VerdictStale: the container may be removed - its instance is over, it is
	// stopped, and it has been stopped long enough.
	VerdictStale Verdict = "stale"

	// VerdictRecent: it would be stale but stopped within the minimum age (or
	// its age is unknown), so it is left for now. The grace period keeps a
	// crash's evidence in place for whoever is looking into it.
	VerdictRecent Verdict = "recent"

	// VerdictLive: it belongs to an instance Sparky is running, starting or
	// stopping, or the container itself is running. Never touched.
	VerdictLive Verdict = "live"

	// VerdictOrphanLive: Sparky's container, running, with no instance record
	// at all (the database was restored from a backup, or the record was
	// deleted). Never touched; reported so a person looks at it.
	VerdictOrphanLive Verdict = "orphan_running"

	// VerdictOther: something this feature does not act on - a container in a
	// state it does not remove (removing), an unlabelled one that has not
	// exited, or one whose record says it belongs to a different node.
	VerdictOther Verdict = "other"
)

// Container is one container as a node reported it, after validation.
type Container struct {
	InstanceID    string
	ProfileID     string
	ContainerID   string
	ContainerName string
	State         string

	// Labelled is false for a container found only by its legacy name.
	Labelled bool

	ExitCode  *int
	OOMKilled bool

	// CreatedAt and FinishedAt are zero when the node did not know them.
	CreatedAt  time.Time
	FinishedAt time.Time
}

// Evaluation is Classify's answer.
type Evaluation struct {
	Verdict Verdict

	// Reason says why, in words fit to show an operator.
	Reason string

	// StoppedAt is the moment the container is aged from: when it finished,
	// else when its instance was recorded stopped, else when it was created.
	// Zero when none is known.
	StoppedAt time.Time
}

// containerRunning reports whether state means the container may be doing
// work now or about to: such a container is never a removal candidate whatever
// the database says.
func containerRunning(state string) bool {
	switch state {
	case "running", "paused", "restarting":
		return true
	}
	return false
}

// Classify decides what to do about one container on node nodeID. inst is the
// running_instances row for the container's instance id, nil if there is none.
// minAge is how long a container must have been stopped to count as stale.
//
// These rules decide what may ever be removed, so they err toward leaving a
// container alone. In order:
//
//  1. A container whose own state is running, paused or restarting is live
//     (or, with no instance record, an orphan that is running). A running
//     container is never removed, whatever the database says.
//  2. A container whose instance is starting, running or stopping is live. An
//     instance recorded running whose container has exited is a dead instance:
//     the Dashboard shows it and Unload clears it, so it is not stale either.
//  3. A record that names a different node than the one reporting means the
//     data does not add up; leave it alone.
//  4. Only exited, dead and created containers are ever stale, and an
//     unlabelled (legacy-named) one only once it has exited.
//  5. It must have been stopped for minAge. Its age comes from the container's
//     finish time, else the instance's recorded stop time, else the container's
//     creation time; if none is known it is recent, never stale.
func Classify(nodeID string, c Container, inst *db.RunningInstance, now time.Time, minAge time.Duration) Evaluation {
	if containerRunning(c.State) {
		if inst == nil {
			return Evaluation{Verdict: VerdictOrphanLive, Reason: fmt.Sprintf("the container is %s and Sparky has no record of its instance", c.State)}
		}
		return Evaluation{Verdict: VerdictLive, Reason: fmt.Sprintf("the container is %s", c.State)}
	}

	if inst != nil {
		switch inst.Status {
		case db.RunningInstanceStatusStarting, db.RunningInstanceStatusRunning, db.RunningInstanceStatusStopping:
			return Evaluation{Verdict: VerdictLive, Reason: fmt.Sprintf("its instance is %s; Unload it from the Dashboard to clear it", inst.Status)}
		}
		if inst.PrimaryNodeID != nodeID {
			return Evaluation{Verdict: VerdictOther, Reason: "its instance record belongs to a different node"}
		}
	}

	switch c.State {
	case "exited":
	case "dead", "created":
		if !c.Labelled {
			return Evaluation{Verdict: VerdictOther, Reason: fmt.Sprintf("an unlabelled container is removed only once it has exited, and this one is %s", c.State)}
		}
	default:
		return Evaluation{Verdict: VerdictOther, Reason: fmt.Sprintf("the container is %s", c.State)}
	}

	stoppedAt := c.FinishedAt
	if stoppedAt.IsZero() && inst != nil && inst.StoppedAt != nil {
		stoppedAt = *inst.StoppedAt
	}
	if stoppedAt.IsZero() {
		stoppedAt = c.CreatedAt
	}
	if stoppedAt.IsZero() {
		return Evaluation{Verdict: VerdictRecent, Reason: "when it stopped is not known"}
	}
	if age := now.Sub(stoppedAt); age < minAge {
		return Evaluation{Verdict: VerdictRecent, StoppedAt: stoppedAt, Reason: fmt.Sprintf("stopped %s ago, less than the %s minimum", humanDuration(age), humanDuration(minAge))}
	}
	return Evaluation{Verdict: VerdictStale, StoppedAt: stoppedAt, Reason: "its instance is over and it has been stopped long enough"}
}

// humanDuration writes d as a short phrase for a person: "3 days", "5 hours",
// "20 minutes". Negative (a clock that moved back) reads as "0 minutes".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= time.Hour:
		return "1 hour"
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	case d >= time.Minute:
		return "1 minute"
	default:
		return "less than a minute"
	}
}
