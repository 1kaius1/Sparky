// SPDX-License-Identifier: AGPL-3.0-or-later

// Package schedule is the shared scheduler: a small, pure description of
// when a recurring job is due (Spec), the one global time zone every schedule
// is read in (Service), and a runner that fires a job when it is owed
// (Runner). Stale container monitoring is the first user; scheduled model
// loads will be the second. See PLANNING.md's 2026-10-26 Decisions Log.
//
// Two schedule forms are offered, not a free-form cron expression: chosen
// weekdays at chosen times of day, or every N days. They cover the stated
// need and are easy to validate and to explain on a settings page.
package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind selects which form of schedule a Spec is.
type Kind string

const (
	// KindWeekly runs on chosen weekdays at chosen times of day, in the
	// global time zone.
	KindWeekly Kind = "weekly"

	// KindEveryDays runs every N days. The first run is the moment the
	// schedule is saved (its anchor); each later run is N calendar days
	// after the previous one at the same local time of day.
	KindEveryDays Kind = "every_days"
)

const (
	// MaxTimesPerDay bounds how many times of day a weekly schedule may name.
	MaxTimesPerDay = 24

	// MaxEveryDays bounds the every-N-days form to a year.
	MaxEveryDays = 365
)

// ErrInvalidSpec is wrapped by Validate's error for a schedule that cannot be
// used.
var ErrInvalidSpec = errors.New("invalid schedule")

var timeOfDayPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// Spec describes when a job recurs. It is stored as JSON in the settings row
// of whatever job owns it: opaque to the database and validated here, the
// same split Model profiles' engine_params uses.
type Spec struct {
	Kind Kind `json:"kind"`

	// Weekdays are the days a weekly schedule runs on, numbered as
	// time.Weekday (0 is Sunday). Weekly only.
	Weekdays []int `json:"weekdays,omitempty"`

	// Times are the "HH:MM" times of day (24-hour, in the global time zone)
	// a weekly schedule runs at on each of its Weekdays. Weekly only.
	Times []string `json:"times,omitempty"`

	// EveryDays is N for the every-N-days form (1 to MaxEveryDays).
	EveryDays int `json:"every_days,omitempty"`
}

// Validate reports whether the Spec is usable, wrapping ErrInvalidSpec with a
// message safe to show an operator.
func (s Spec) Validate() error {
	switch s.Kind {
	case KindWeekly:
		if s.EveryDays != 0 {
			return fmt.Errorf("%w: a weekly schedule does not take an interval in days", ErrInvalidSpec)
		}
		if len(s.Weekdays) == 0 {
			return fmt.Errorf("%w: choose at least one weekday", ErrInvalidSpec)
		}
		seenDay := make(map[int]bool, len(s.Weekdays))
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				return fmt.Errorf("%w: weekday %d is not between 0 (Sunday) and 6 (Saturday)", ErrInvalidSpec, d)
			}
			if seenDay[d] {
				return fmt.Errorf("%w: weekday %d is listed twice", ErrInvalidSpec, d)
			}
			seenDay[d] = true
		}
		if len(s.Times) == 0 {
			return fmt.Errorf("%w: choose at least one time of day", ErrInvalidSpec)
		}
		if len(s.Times) > MaxTimesPerDay {
			return fmt.Errorf("%w: at most %d times of day", ErrInvalidSpec, MaxTimesPerDay)
		}
		seenTime := make(map[string]bool, len(s.Times))
		for _, t := range s.Times {
			if _, _, ok := parseTimeOfDay(t); !ok {
				return fmt.Errorf("%w: %q is not a time of day written HH:MM (24-hour)", ErrInvalidSpec, t)
			}
			if seenTime[t] {
				return fmt.Errorf("%w: time %s is listed twice", ErrInvalidSpec, t)
			}
			seenTime[t] = true
		}
		return nil
	case KindEveryDays:
		if len(s.Weekdays) != 0 || len(s.Times) != 0 {
			return fmt.Errorf("%w: an every-N-days schedule does not take weekdays or times of day", ErrInvalidSpec)
		}
		if s.EveryDays < 1 || s.EveryDays > MaxEveryDays {
			return fmt.Errorf("%w: every N days needs N between 1 and %d", ErrInvalidSpec, MaxEveryDays)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown schedule kind %q", ErrInvalidSpec, s.Kind)
	}
}

var weekdayNames = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// WeekdayName returns the English name of a weekday numbered as time.Weekday
// (0 is Sunday), or "" for a number outside 0 to 6.
func WeekdayName(d int) string {
	if d < 0 || d > 6 {
		return ""
	}
	return weekdayNames[d]
}

// Describe writes the schedule as a sentence for a person: "Mondays and
// Wednesdays at 02:00 and 14:30" or "Every 2 days, starting when it was saved".
// An invalid Spec describes as "no valid schedule".
func (s Spec) Describe() string {
	if s.Validate() != nil {
		return "no valid schedule"
	}
	n := s.Normalized()
	switch n.Kind {
	case KindEveryDays:
		if n.EveryDays == 1 {
			return "Every day, starting when it was saved"
		}
		return fmt.Sprintf("Every %d days, starting when it was saved", n.EveryDays)
	default:
		days := make([]string, len(n.Weekdays))
		for i, d := range n.Weekdays {
			days[i] = WeekdayName(d) + "s"
		}
		return joinAnd(days) + " at " + joinAnd(n.Times)
	}
}

// joinAnd writes items as "a", "a and b" or "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// Normalized returns a copy with Weekdays and Times sorted, so equal
// schedules store identically however the form listed them. Call it after
// Validate.
func (s Spec) Normalized() Spec {
	out := s
	out.Weekdays = append([]int(nil), s.Weekdays...)
	sort.Ints(out.Weekdays)
	out.Times = append([]string(nil), s.Times...)
	sort.Strings(out.Times)
	return out
}

// Latest returns the most recent occurrence after `after` and no later than
// `upTo`, or false when there is none. anchor is when the schedule was saved:
// a weekly schedule ignores everything before it (saving at 10:00 must not
// run today's 09:00), an every-N-days schedule counts it as its first
// occurrence. A Spec that fails Validate has no occurrences.
//
// Only the latest occurrence is returned, never a list: a job that was
// missed several times over (the server was down) is owed one run, not one
// per miss.
func (s Spec) Latest(anchor, after, upTo time.Time, loc *time.Location) (time.Time, bool) {
	if s.Validate() != nil || loc == nil || anchor.IsZero() {
		return time.Time{}, false
	}
	switch s.Kind {
	case KindWeekly:
		lower := after
		if anchor.After(lower) {
			lower = anchor
		}
		return s.latestWeekly(lower, upTo, loc)
	default:
		return s.latestEveryDays(anchor, after, upTo, loc)
	}
}

// Next returns the first occurrence strictly after `after`, or false for an
// invalid Spec. It is for showing an operator when the job will run next.
func (s Spec) Next(anchor, after time.Time, loc *time.Location) (time.Time, bool) {
	if s.Validate() != nil || loc == nil || anchor.IsZero() {
		return time.Time{}, false
	}
	switch s.Kind {
	case KindWeekly:
		from := after
		if anchor.After(from) {
			from = anchor
		}
		return s.nextWeekly(from, loc)
	default:
		return s.nextEveryDays(anchor, after, loc)
	}
}

func (s Spec) latestWeekly(lower, upTo time.Time, loc *time.Location) (time.Time, bool) {
	days := weekdaySet(s.Weekdays)
	times := s.sortedTimes()
	y, m, d := upTo.In(loc).Date()
	lowerY, lowerM, lowerD := lower.In(loc).Date()
	floor := time.Date(lowerY, lowerM, lowerD, 0, 0, 0, 0, time.UTC)
	for offset := 0; ; offset++ {
		day := time.Date(y, m, d-offset, 0, 0, 0, 0, time.UTC)
		if day.Before(floor) {
			return time.Time{}, false
		}
		if !days[int(day.Weekday())] {
			continue
		}
		for i := len(times) - 1; i >= 0; i-- {
			at := wallClock(day.Year(), day.Month(), day.Day(), times[i].h, times[i].m, 0, loc)
			if at.After(upTo) {
				continue
			}
			if !at.After(lower) {
				// Times run newest first, so this one is at or before the
				// lower bound and so is everything earlier, on this day and
				// on every earlier day.
				return time.Time{}, false
			}
			return at, true
		}
	}
}

func (s Spec) nextWeekly(from time.Time, loc *time.Location) (time.Time, bool) {
	days := weekdaySet(s.Weekdays)
	times := s.sortedTimes()
	y, m, d := from.In(loc).Date()
	for offset := 0; offset <= 8; offset++ {
		day := time.Date(y, m, d+offset, 0, 0, 0, 0, time.UTC)
		if !days[int(day.Weekday())] {
			continue
		}
		for _, t := range times {
			at := wallClock(day.Year(), day.Month(), day.Day(), t.h, t.m, 0, loc)
			if at.After(from) {
				return at, true
			}
		}
	}
	return time.Time{}, false
}

// everyDaysAt is the k-th occurrence of an every-N-days schedule: k*N
// calendar days after the anchor's local date, at the anchor's local time of
// day. k=0 is the anchor itself.
func (s Spec) everyDaysAt(anchor time.Time, k int, loc *time.Location) time.Time {
	if k == 0 {
		return anchor
	}
	local := anchor.In(loc)
	y, m, d := local.Date()
	return wallClock(y, m, d+k*s.EveryDays, local.Hour(), local.Minute(), local.Second(), loc)
}

func (s Spec) latestEveryDays(anchor, after, upTo time.Time, loc *time.Location) (time.Time, bool) {
	if upTo.Before(anchor) {
		return time.Time{}, false
	}
	// The estimate is exact except around a daylight saving change, where a
	// local day is 23 or 25 hours, so look a step either side of it.
	estimate := int(upTo.Sub(anchor) / (time.Duration(s.EveryDays) * 24 * time.Hour))
	for k := estimate + 1; k >= 0 && k >= estimate-2; k-- {
		at := s.everyDaysAt(anchor, k, loc)
		if at.After(upTo) {
			continue
		}
		if !at.After(after) {
			return time.Time{}, false
		}
		return at, true
	}
	return time.Time{}, false
}

func (s Spec) nextEveryDays(anchor, after time.Time, loc *time.Location) (time.Time, bool) {
	if after.Before(anchor) {
		return anchor, true
	}
	estimate := int(after.Sub(anchor) / (time.Duration(s.EveryDays) * 24 * time.Hour))
	for k := max(estimate-2, 0); k <= estimate+3; k++ {
		if at := s.everyDaysAt(anchor, k, loc); at.After(after) {
			return at, true
		}
	}
	return time.Time{}, false
}

type timeOfDay struct{ h, m int }

func (s Spec) sortedTimes() []timeOfDay {
	out := make([]timeOfDay, 0, len(s.Times))
	for _, t := range s.Times {
		if h, m, ok := parseTimeOfDay(t); ok {
			out = append(out, timeOfDay{h, m})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].h != out[j].h {
			return out[i].h < out[j].h
		}
		return out[i].m < out[j].m
	})
	return out
}

func weekdaySet(days []int) map[int]bool {
	set := make(map[int]bool, len(days))
	for _, d := range days {
		set[d] = true
	}
	return set
}

func parseTimeOfDay(s string) (hour, minute int, ok bool) {
	m := timeOfDayPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	hour, _ = strconv.Atoi(m[1])
	minute, _ = strconv.Atoi(m[2])
	return hour, minute, true
}

// wallClock is the instant at which loc's clock first reads the given local
// date and time. A local time that does not exist (the hour skipped when
// daylight saving starts) resolves to the first instant after the gap, so a
// job set for 02:30 runs at 03:00 that one day instead of being skipped or
// moved an hour early (time.Date alone gives the latter). A local time that
// happens twice (the hour repeated when it ends) is the earlier of the two, so
// it fires once. Day overflow, such as a day of 35, rolls into the next month
// as time.Date does.
func wallClock(year int, month time.Month, day, hour, minute, second int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, hour, minute, second, 0, loc)
	if t.Hour() == hour && t.Minute() == minute && t.Second() == second {
		return t
	}
	// The wall time was skipped. Walk forward from before the change to the
	// first minute whose local reading is not earlier than the wanted time.
	wanted := time.Date(year, month, day, hour, minute, second, 0, time.UTC)
	for probe := t.Add(-3 * time.Hour).Truncate(time.Minute); probe.Before(t.Add(6 * time.Hour)); probe = probe.Add(time.Minute) {
		l := probe.In(loc)
		reading := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
		if !reading.Before(wanted) {
			return probe
		}
	}
	return t
}
