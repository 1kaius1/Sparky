// SPDX-License-Identifier: AGPL-3.0-or-later

package schedule

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"
)

type fakeZone struct {
	loc *time.Location
	err error
}

func (z fakeZone) Location(context.Context) (*time.Location, error) { return z.loc, z.err }

// fakeJob keeps the state a real job keeps in its settings row: Begin stamps
// LastRun, so a second check right behind the first must find nothing owed.
type fakeJob struct {
	state    State
	loadErr  error
	beginErr error
	runErr   error
	begins   []time.Time
	runs     []time.Time
}

func (f *fakeJob) job() Job {
	return Job{
		Name: "test",
		Load: func(context.Context) (State, error) { return f.state, f.loadErr },
		Begin: func(_ context.Context, at time.Time) error {
			if f.beginErr != nil {
				return f.beginErr
			}
			f.begins = append(f.begins, at)
			f.state.LastRun = at
			return nil
		},
		Run: func(_ context.Context, occurrence time.Time) error {
			f.runs = append(f.runs, occurrence)
			return f.runErr
		},
	}
}

func newTestRunner(now time.Time, zone ZoneSource) *Runner {
	r := NewRunner(zone, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return now }
	return r
}

var daily3am = Spec{Kind: KindWeekly, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Times: []string{"03:00"}}

func TestCheck_RunsADueJobExactlyOnce(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor}}
	now := time.Date(2026, 10, 26, 3, 0, 20, 0, time.UTC)
	r := newTestRunner(now, fakeZone{loc: time.UTC})

	if problem := r.check(context.Background(), f.job()); problem != "" {
		t.Fatalf("first check: %s", problem)
	}
	if len(f.runs) != 1 || !f.runs[0].Equal(time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("runs = %v, want one for 2026-10-26 03:00", f.runs)
	}
	r.now = func() time.Time { return now.Add(30 * time.Second) }
	if problem := r.check(context.Background(), f.job()); problem != "" {
		t.Fatalf("second check: %s", problem)
	}
	if len(f.runs) != 1 {
		t.Errorf("the same occurrence ran %d times", len(f.runs))
	}
}

func TestCheck_AMissedRunAtStartupRunsOnceNotOncePerMiss(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 10, 10, 3, 0, 5, 0, time.UTC)
	f := &fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor, LastRun: last}}
	r := newTestRunner(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), fakeZone{loc: time.UTC})
	for i := 0; i < 3; i++ {
		r.check(context.Background(), f.job())
	}
	if len(f.runs) != 1 {
		t.Errorf("ten missed days ran the job %d times, want 1", len(f.runs))
	}
}

func TestCheck_NotDueDisabledAndBrokenInputsDoNotRun(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC) // before 03:00, and yesterday's already ran
	ran := time.Date(2026, 10, 25, 3, 0, 1, 0, time.UTC)

	cases := map[string]struct {
		job     *fakeJob
		zone    ZoneSource
		problem bool
	}{
		"not due yet": {&fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor, LastRun: ran}}, fakeZone{loc: time.UTC}, false},
		"disabled":    {&fakeJob{state: State{Enabled: false, Spec: daily3am, Anchor: anchor}}, fakeZone{loc: time.UTC}, false},
		"load fails":  {&fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor}, loadErr: errors.New("db down")}, fakeZone{loc: time.UTC}, true},
		"zone fails":  {&fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor}}, fakeZone{err: errors.New("no such zone")}, true},
	}
	for name, c := range cases {
		r := newTestRunner(now, c.zone)
		problem := r.check(context.Background(), c.job.job())
		if len(c.job.runs) != 0 || len(c.job.begins) != 0 {
			t.Errorf("%s: ran (%d) or began (%d)", name, len(c.job.runs), len(c.job.begins))
		}
		if (problem != "") != c.problem {
			t.Errorf("%s: problem = %q, want a problem: %v", name, problem, c.problem)
		}
	}
}

func TestCheck_AJobThatCannotRecordItsRunDoesNotRun(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor}, beginErr: errors.New("db down")}
	r := newTestRunner(time.Date(2026, 10, 26, 3, 0, 20, 0, time.UTC), fakeZone{loc: time.UTC})
	problem := r.check(context.Background(), f.job())
	if len(f.runs) != 0 {
		t.Errorf("ran %d times though the run could not be recorded", len(f.runs))
	}
	if problem == "" {
		t.Error("an unrecorded run was not reported")
	}
}

func TestCheck_AFailedRunIsReportedAndNotRetriedAtOnce(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeJob{state: State{Enabled: true, Spec: daily3am, Anchor: anchor}, runErr: errors.New("agent unreachable")}
	now := time.Date(2026, 10, 26, 3, 0, 20, 0, time.UTC)
	r := newTestRunner(now, fakeZone{loc: time.UTC})
	if problem := r.check(context.Background(), f.job()); problem == "" {
		t.Error("a failed run was not reported")
	}
	r.now = func() time.Time { return now.Add(30 * time.Second) }
	r.check(context.Background(), f.job())
	if len(f.runs) != 1 {
		t.Errorf("a failed run was retried: %d runs", len(f.runs))
	}
}

func TestCheck_ChangingTheScheduleTakesEffectWithoutARestart(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeJob{state: State{Enabled: false, Spec: daily3am, Anchor: anchor}}
	r := newTestRunner(time.Date(2026, 10, 26, 3, 0, 20, 0, time.UTC), fakeZone{loc: time.UTC})
	r.check(context.Background(), f.job())
	if len(f.runs) != 0 {
		t.Fatal("a disabled job ran")
	}
	f.state.Enabled = true
	r.check(context.Background(), f.job())
	if len(f.runs) != 1 {
		t.Errorf("enabling the job did not run it: %d runs", len(f.runs))
	}
}

func TestRun_StopsWhenTheContextIsCancelled(t *testing.T) {
	r := NewRunner(fakeZone{loc: time.UTC}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, (&fakeJob{}).job())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
