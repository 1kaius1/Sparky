// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
)

const (
	nodeOne = "aaaaaaaa-0000-4000-8000-000000000001"
	nodeTwo = "aaaaaaaa-0000-4000-8000-000000000002"
	instOne = "11111111-1111-4111-8111-111111111111"
	instTwo = "22222222-2222-4222-8222-222222222222"
	profOne = "99999999-9999-4999-8999-999999999999"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func exited(finishedAgo time.Duration) Container {
	return Container{InstanceID: instOne, State: "exited", Labelled: true, FinishedAt: ago(finishedAgo), CreatedAt: ago(finishedAgo + time.Hour)}
}

func instance(status db.RunningInstanceStatus, node string, stoppedAgo time.Duration) *db.RunningInstance {
	inst := &db.RunningInstance{ID: instOne, ProfileID: profOne, Status: status, PrimaryNodeID: node}
	if stoppedAgo > 0 {
		t := ago(stoppedAgo)
		inst.StoppedAt = &t
	}
	return inst
}

func TestClassify(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name string
		c    Container
		inst *db.RunningInstance
		min  time.Duration
		want Verdict
		why  string // a phrase the reason must contain
	}{
		// Never touch anything live.
		{"running container, instance running", Container{InstanceID: instOne, State: "running", Labelled: true}, instance(db.RunningInstanceStatusRunning, nodeOne, 0), day, VerdictLive, "running"},
		{"running container, instance already stopped in the database", Container{InstanceID: instOne, State: "running", Labelled: true}, instance(db.RunningInstanceStatusStopped, nodeOne, 90*day), day, VerdictLive, "running"},
		{"paused container", Container{InstanceID: instOne, State: "paused", Labelled: true}, instance(db.RunningInstanceStatusStopped, nodeOne, 90*day), day, VerdictLive, "paused"},
		{"restarting container", Container{InstanceID: instOne, State: "restarting", Labelled: true}, instance(db.RunningInstanceStatusFailed, nodeOne, 90*day), day, VerdictLive, "restarting"},
		{"running container with no instance record", Container{InstanceID: instOne, State: "running", Labelled: true}, nil, day, VerdictOrphanLive, "no record"},
		{"paused container with no instance record", Container{InstanceID: instOne, State: "paused", Labelled: true}, nil, day, VerdictOrphanLive, "no record"},
		{"exited container whose instance is running (a dead instance)", exited(90 * day), instance(db.RunningInstanceStatusRunning, nodeOne, 0), day, VerdictLive, "Unload"},
		{"exited container whose instance is starting", exited(90 * day), instance(db.RunningInstanceStatusStarting, nodeOne, 0), day, VerdictLive, "starting"},
		{"exited container whose instance is stopping", exited(90 * day), instance(db.RunningInstanceStatusStopping, nodeOne, 0), day, VerdictLive, "stopping"},

		// The data does not add up.
		{"record names another node", exited(90 * day), instance(db.RunningInstanceStatusStopped, nodeTwo, 90*day), day, VerdictOther, "different node"},

		// States that are never removed.
		{"removing", Container{InstanceID: instOne, State: "removing", Labelled: true, FinishedAt: ago(90 * day)}, instance(db.RunningInstanceStatusStopped, nodeOne, 90*day), day, VerdictOther, "removing"},
		{"an unknown state word", Container{InstanceID: instOne, State: "teleporting", Labelled: true, FinishedAt: ago(90 * day)}, nil, day, VerdictOther, "teleporting"},
		{"unlabelled and dead", Container{InstanceID: instOne, State: "dead", Labelled: false, FinishedAt: ago(90 * day)}, nil, day, VerdictOther, "unlabelled"},
		{"unlabelled and created", Container{InstanceID: instOne, State: "created", Labelled: false, CreatedAt: ago(90 * day)}, nil, day, VerdictOther, "unlabelled"},

		// Stale.
		{"exited, instance stopped, old", exited(3 * day), instance(db.RunningInstanceStatusStopped, nodeOne, 3*day), day, VerdictStale, ""},
		{"exited, instance failed, old", exited(3 * day), instance(db.RunningInstanceStatusFailed, nodeOne, 3*day), day, VerdictStale, ""},
		{"exited orphan, old", exited(3 * day), nil, day, VerdictStale, ""},
		{"dead and labelled, old", Container{InstanceID: instOne, State: "dead", Labelled: true, FinishedAt: ago(3 * day)}, nil, day, VerdictStale, ""},
		{"created and labelled, old", Container{InstanceID: instOne, State: "created", Labelled: true, CreatedAt: ago(3 * day)}, nil, day, VerdictStale, ""},
		{"unlabelled legacy but exited, old", Container{InstanceID: instOne, State: "exited", Labelled: false, FinishedAt: ago(3 * day)}, nil, day, VerdictStale, ""},

		// The grace period.
		{"exited 23 hours ago, minimum 24", exited(23 * time.Hour), instance(db.RunningInstanceStatusStopped, nodeOne, 23*time.Hour), day, VerdictRecent, "23 hours"},
		{"exited exactly at the minimum", exited(day), nil, day, VerdictStale, ""},
		{"zero minimum makes anything stopped stale at once", exited(time.Second), nil, 0, VerdictStale, ""},
		{"clock moved back: finished in the future", Container{InstanceID: instOne, State: "exited", Labelled: true, FinishedAt: now.Add(time.Hour)}, nil, day, VerdictRecent, "less than a minute"},

		// Where the age comes from.
		{"no finish time: the instance's stop time", Container{InstanceID: instOne, State: "exited", Labelled: true, CreatedAt: ago(90 * day)}, instance(db.RunningInstanceStatusStopped, nodeOne, time.Hour), day, VerdictRecent, "1 hour"},
		{"no finish or stop time: creation time", Container{InstanceID: instOne, State: "exited", Labelled: true, CreatedAt: ago(5 * day)}, instance(db.RunningInstanceStatusStopped, nodeOne, 0), day, VerdictStale, ""},
		{"no time known at all is recent, never stale", Container{InstanceID: instOne, State: "exited", Labelled: true}, nil, day, VerdictRecent, "not known"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(nodeOne, c.c, c.inst, now, c.min)
			if got.Verdict != c.want {
				t.Fatalf("verdict = %q (%s), want %q", got.Verdict, got.Reason, c.want)
			}
			if c.why != "" && !strings.Contains(got.Reason, c.why) {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, c.why)
			}
			if got.Reason == "" {
				t.Error("no reason given")
			}
		})
	}
}

// A container is only ever stale or recent when it is stopped and its instance
// is over: no combination of a live signal with an old timestamp may produce
// either. This walks every state against every instance status.
func TestClassify_NothingLiveIsEverStaleOrRecent(t *testing.T) {
	states := []string{"running", "paused", "restarting", "exited", "dead", "created", "removing", "", "weird"}
	statuses := []*db.RunningInstance{
		nil,
		instance(db.RunningInstanceStatusStarting, nodeOne, 0),
		instance(db.RunningInstanceStatusRunning, nodeOne, 0),
		instance(db.RunningInstanceStatusStopping, nodeOne, 0),
		instance(db.RunningInstanceStatusStopped, nodeOne, 400*24*time.Hour),
		instance(db.RunningInstanceStatusFailed, nodeOne, 400*24*time.Hour),
	}
	for _, state := range states {
		for _, labelled := range []bool{true, false} {
			for _, inst := range statuses {
				c := Container{InstanceID: instOne, State: state, Labelled: labelled, FinishedAt: ago(400 * 24 * time.Hour), CreatedAt: ago(401 * 24 * time.Hour)}
				got := Classify(nodeOne, c, inst, now, 0)
				if got.Verdict != VerdictStale && got.Verdict != VerdictRecent {
					continue
				}
				live := containerRunning(state) ||
					(inst != nil && (inst.Status == db.RunningInstanceStatusStarting || inst.Status == db.RunningInstanceStatusRunning || inst.Status == db.RunningInstanceStatusStopping))
				if live || (state != "exited" && state != "dead" && state != "created") || (!labelled && state != "exited") {
					t.Errorf("state %q labelled %v instance %v was classified %s", state, labelled, inst, got.Verdict)
				}
			}
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Hour:                     "less than a minute",
		30 * time.Second:               "less than a minute",
		time.Minute:                    "1 minute",
		20 * time.Minute:               "20 minutes",
		time.Hour:                      "1 hour",
		5 * time.Hour:                  "5 hours",
		47 * time.Hour:                 "47 hours",
		48 * time.Hour:                 "2 days",
		100*24*time.Hour + 5*time.Hour: "100 days",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
