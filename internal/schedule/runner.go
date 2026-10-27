// SPDX-License-Identifier: AGPL-3.0-or-later

package schedule

import (
	"context"
	"log"
	"time"
)

const (
	// startupDelay holds the first check back after the server starts, so a
	// run that was missed while it was down does not fire before the agents
	// have reconnected and a job that talks to them would find nobody.
	startupDelay = time.Minute

	// checkInterval is how often a job is checked. Schedules are minute
	// resolution, so a run starts within about this long after it is due.
	checkInterval = 30 * time.Second
)

// State is what a Job's schedule currently is, read fresh at every check so
// a settings change takes effect without a restart or a signal.
type State struct {
	// Enabled false means the job is off and never runs.
	Enabled bool

	Spec Spec

	// Anchor is when the schedule was last saved, see Spec.Latest.
	Anchor time.Time

	// LastRun is when the job last began a run; the zero time if never.
	LastRun time.Time
}

// Job is one recurring task. The runner owns the timing; the job owns what
// "a run" means and where its own schedule is kept.
type Job struct {
	// Name appears in log lines.
	Name string

	// Load reads the job's current State.
	Load func(ctx context.Context) (State, error)

	// Begin records that a run is starting at startedAt. It is called before
	// Run, and a run whose Begin fails does not happen: a job that cannot
	// remember it ran would run again at the next check, and "never twice"
	// matters more than "never missed" for a job that removes things.
	Begin func(ctx context.Context, startedAt time.Time) error

	// Run does the work for the occurrence that was due. An error is logged;
	// the next occurrence is not affected.
	Run func(ctx context.Context, occurrence time.Time) error
}

// ZoneSource supplies the global time zone, read fresh at every check so a
// change on the Settings page applies to the next one.
type ZoneSource interface {
	Location(ctx context.Context) (*time.Location, error)
}

// Runner runs Jobs when their schedules say they are owed.
type Runner struct {
	zone   ZoneSource
	logger *log.Logger
	now    func() time.Time
}

// NewRunner constructs a Runner.
func NewRunner(zone ZoneSource, logger *log.Logger) *Runner {
	return &Runner{zone: zone, logger: logger, now: time.Now}
}

// Run checks job until ctx is cancelled: first shortly after startup, then
// every checkInterval. It blocks, so a caller starts one goroutine per job; a
// job that runs for a long time therefore delays only its own next check.
//
// A run missed because the server was down starts once at the first check
// after startup and never more than once, however many occurrences passed.
func (r *Runner) Run(ctx context.Context, job Job) {
	timer := time.NewTimer(startupDelay)
	defer timer.Stop()
	var lastProblem string
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if problem := r.check(ctx, job); problem != "" {
			// Logged when it changes, not at every check, so a persistent
			// problem is one line and not two a minute.
			if problem != lastProblem {
				r.logger.Printf("schedule: %s: %s", job.Name, problem)
			}
			lastProblem = problem
		} else {
			lastProblem = ""
		}
		timer.Reset(checkInterval)
	}
}

// check runs job if it is owed and returns a short description of anything
// that stopped it from being checked or run, "" when all went well.
func (r *Runner) check(ctx context.Context, job Job) string {
	state, err := job.Load(ctx)
	if err != nil {
		return "load schedule: " + err.Error()
	}
	if !state.Enabled {
		return ""
	}
	loc, err := r.zone.Location(ctx)
	if err != nil {
		return "time zone: " + err.Error()
	}
	now := r.now()
	occurrence, due := state.Spec.Latest(state.Anchor, state.LastRun, now, loc)
	if !due {
		return ""
	}
	if err := job.Begin(ctx, now); err != nil {
		return "record run start (run skipped): " + err.Error()
	}
	if err := job.Run(ctx, occurrence); err != nil {
		return "run: " + err.Error()
	}
	return ""
}
