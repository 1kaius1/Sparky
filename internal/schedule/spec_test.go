// SPDX-License-Identifier: AGPL-3.0-or-later

package schedule

import (
	"errors"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone database has no %s: %v", name, err)
	}
	return loc
}

func at(loc *time.Location, y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, loc)
}

func TestSpecValidate(t *testing.T) {
	good := []Spec{
		{Kind: KindWeekly, Weekdays: []int{1, 3, 5}, Times: []string{"02:00", "14:30"}},
		{Kind: KindWeekly, Weekdays: []int{0}, Times: []string{"00:00"}},
		{Kind: KindWeekly, Weekdays: []int{6}, Times: []string{"23:59"}},
		{Kind: KindEveryDays, EveryDays: 1},
		{Kind: KindEveryDays, EveryDays: 365},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: unexpected error %v", s, err)
		}
	}
	bad := map[string]Spec{
		"no kind":              {},
		"unknown kind":         {Kind: "cron", EveryDays: 1},
		"weekly no days":       {Kind: KindWeekly, Times: []string{"02:00"}},
		"weekly no times":      {Kind: KindWeekly, Weekdays: []int{1}},
		"weekday 7":            {Kind: KindWeekly, Weekdays: []int{7}, Times: []string{"02:00"}},
		"weekday -1":           {Kind: KindWeekly, Weekdays: []int{-1}, Times: []string{"02:00"}},
		"weekday twice":        {Kind: KindWeekly, Weekdays: []int{1, 1}, Times: []string{"02:00"}},
		"time 24:00":           {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"24:00"}},
		"time 2:00":            {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"2:00"}},
		"time 02:60":           {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:60"}},
		"time with seconds":    {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:00:00"}},
		"time twice":           {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:00", "02:00"}},
		"weekly with interval": {Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:00"}, EveryDays: 2},
		"interval 0":           {Kind: KindEveryDays},
		"interval 366":         {Kind: KindEveryDays, EveryDays: 366},
		"interval with days":   {Kind: KindEveryDays, EveryDays: 2, Weekdays: []int{1}},
		"interval with times":  {Kind: KindEveryDays, EveryDays: 2, Times: []string{"02:00"}},
	}
	for name, s := range bad {
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v, want ErrInvalidSpec", name, err)
		}
	}
	tooMany := Spec{Kind: KindWeekly, Weekdays: []int{1}}
	for h := 0; h < 24; h++ {
		tooMany.Times = append(tooMany.Times, time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC).Format("15:04"))
	}
	if err := tooMany.Validate(); err != nil {
		t.Errorf("24 times: %v, want it accepted", err)
	}
	tooMany.Times = append(tooMany.Times, "23:30")
	if err := tooMany.Validate(); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("25 times: %v, want ErrInvalidSpec", err)
	}
}

func TestSpecDescribe(t *testing.T) {
	cases := map[string]struct {
		spec Spec
		want string
	}{
		"one day one time":   {Spec{Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:00"}}, "Mondays at 02:00"},
		"two days two times": {Spec{Kind: KindWeekly, Weekdays: []int{3, 1}, Times: []string{"14:30", "02:00"}}, "Mondays and Wednesdays at 02:00 and 14:30"},
		"three days":         {Spec{Kind: KindWeekly, Weekdays: []int{0, 6, 3}, Times: []string{"23:59"}}, "Sundays, Wednesdays and Saturdays at 23:59"},
		"every day":          {Spec{Kind: KindEveryDays, EveryDays: 1}, "Every day, starting when it was saved"},
		"every 3 days":       {Spec{Kind: KindEveryDays, EveryDays: 3}, "Every 3 days, starting when it was saved"},
		"invalid":            {Spec{Kind: KindWeekly}, "no valid schedule"},
		"empty":              {Spec{}, "no valid schedule"},
	}
	for name, c := range cases {
		if got := c.spec.Describe(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if WeekdayName(0) != "Sunday" || WeekdayName(6) != "Saturday" || WeekdayName(7) != "" || WeekdayName(-1) != "" {
		t.Error("WeekdayName is wrong at its edges")
	}
}

func TestSpecNormalizedSortsWithoutTouchingTheOriginal(t *testing.T) {
	s := Spec{Kind: KindWeekly, Weekdays: []int{5, 1, 3}, Times: []string{"14:30", "02:00"}}
	n := s.Normalized()
	if n.Weekdays[0] != 1 || n.Weekdays[2] != 5 || n.Times[0] != "02:00" {
		t.Errorf("Normalized() = %+v", n)
	}
	if s.Weekdays[0] != 5 || s.Times[0] != "14:30" {
		t.Errorf("Normalized() modified the original: %+v", s)
	}
}

func TestWeekly_LatestIsTheNewestOccurrenceAndOnlyOne(t *testing.T) {
	loc := time.UTC
	s := Spec{Kind: KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00", "14:30"}} // Mon and Wed
	anchor := at(loc, 2026, 1, 1, 0, 0)
	// Wednesday 2026-10-28 15:00: the newest occurrence is that day's 14:30.
	got, ok := s.Latest(anchor, time.Time{}, at(loc, 2026, 10, 28, 15, 0), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 28, 14, 30)) {
		t.Errorf("got %v, %v; want 2026-10-28 14:30", got, ok)
	}
	// Thursday: nothing happened since Wednesday 14:30, still that one.
	got, ok = s.Latest(anchor, time.Time{}, at(loc, 2026, 10, 29, 9, 0), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 28, 14, 30)) {
		t.Errorf("thursday: got %v, %v; want wednesday 14:30", got, ok)
	}
	// Already run for that occurrence: nothing is owed.
	if got, ok := s.Latest(anchor, at(loc, 2026, 10, 28, 14, 30), at(loc, 2026, 10, 29, 9, 0), loc); ok {
		t.Errorf("owed %v after the occurrence had run", got)
	}
	// A run begun a few seconds late still counts as having run it.
	lastRun := at(loc, 2026, 10, 28, 14, 30).Add(20 * time.Second)
	if got, ok := s.Latest(anchor, lastRun, at(loc, 2026, 10, 29, 9, 0), loc); ok {
		t.Errorf("owed %v after a late run", got)
	}
}

func TestWeekly_MissedRunsCollapseToOne(t *testing.T) {
	loc := time.UTC
	s := Spec{Kind: KindWeekly, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Times: []string{"03:00"}}
	anchor := at(loc, 2026, 10, 1, 0, 0)
	lastRun := at(loc, 2026, 10, 10, 3, 0)
	// The server was down for ten days: one run is owed, the newest.
	got, ok := s.Latest(anchor, lastRun, at(loc, 2026, 10, 20, 12, 0), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 20, 3, 0)) {
		t.Errorf("got %v, %v; want only 2026-10-20 03:00", got, ok)
	}
	// After it begins, nothing more is owed.
	if got, ok := s.Latest(anchor, at(loc, 2026, 10, 20, 12, 0), at(loc, 2026, 10, 20, 12, 1), loc); ok {
		t.Errorf("owed %v a second time", got)
	}
}

func TestWeekly_NothingBeforeTheScheduleWasSaved(t *testing.T) {
	loc := time.UTC
	s := Spec{Kind: KindWeekly, Weekdays: []int{3}, Times: []string{"09:00"}}
	savedAt := at(loc, 2026, 10, 28, 10, 0) // a Wednesday, after 09:00
	if got, ok := s.Latest(savedAt, time.Time{}, at(loc, 2026, 10, 28, 10, 5), loc); ok {
		t.Errorf("a schedule saved at 10:00 owes %v, which is before it was saved", got)
	}
	// The following Wednesday it does run.
	got, ok := s.Latest(savedAt, time.Time{}, at(loc, 2026, 11, 4, 9, 1), loc)
	if !ok || !got.Equal(at(loc, 2026, 11, 4, 9, 0)) {
		t.Errorf("got %v, %v; want 2026-11-04 09:00", got, ok)
	}
}

func TestWeekly_ReadInTheGlobalZoneNotUTC(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	s := Spec{Kind: KindWeekly, Weekdays: []int{1}, Times: []string{"02:00"}}
	anchor := at(ny, 2026, 1, 1, 0, 0)
	// Monday 02:00 in New York in October is 06:00 UTC.
	got, ok := s.Latest(anchor, time.Time{}, time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC), ny)
	if !ok || !got.Equal(time.Date(2026, 10, 26, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v, %v; want 06:00 UTC", got, ok)
	}
	// At 05:00 UTC it is still Sunday night there: Monday's run is not yet due.
	got, ok = s.Latest(anchor, time.Time{}, time.Date(2026, 10, 26, 5, 0, 0, 0, time.UTC), ny)
	if ok && !got.Before(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("owed %v before Monday 02:00 New York", got)
	}
}

func TestEveryDays_FirstRunIsTheMomentItWasSaved(t *testing.T) {
	loc := time.UTC
	s := Spec{Kind: KindEveryDays, EveryDays: 3}
	anchor := at(loc, 2026, 10, 26, 10, 15)
	got, ok := s.Latest(anchor, time.Time{}, anchor.Add(time.Second), loc)
	if !ok || !got.Equal(anchor) {
		t.Errorf("got %v, %v; want the anchor itself", got, ok)
	}
	// Once it has begun, nothing until three days later.
	if got, ok := s.Latest(anchor, anchor.Add(time.Second), at(loc, 2026, 10, 28, 23, 0), loc); ok {
		t.Errorf("owed %v before day three", got)
	}
	got, ok = s.Latest(anchor, anchor.Add(time.Second), at(loc, 2026, 10, 29, 10, 16), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 29, 10, 15)) {
		t.Errorf("got %v, %v; want 2026-10-29 10:15", got, ok)
	}
}

func TestEveryDays_MissedRunsCollapseToOne(t *testing.T) {
	loc := time.UTC
	s := Spec{Kind: KindEveryDays, EveryDays: 2}
	anchor := at(loc, 2026, 10, 1, 8, 0)
	got, ok := s.Latest(anchor, at(loc, 2026, 10, 3, 8, 0), at(loc, 2026, 10, 20, 9, 0), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 19, 8, 0)) {
		t.Errorf("got %v, %v; want only 2026-10-19 08:00", got, ok)
	}
}

func TestEveryDays_KeepsLocalTimeOfDayAcrossDaylightSaving(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	s := Spec{Kind: KindEveryDays, EveryDays: 7}
	anchor := at(ny, 2026, 10, 28, 9, 0)
	// 7 days after 2026-10-28 09:00 EDT is 2026-11-04 09:00 EST: the clocks went
	// back on 11-01, and the run stays at 09:00 on the wall clock.
	got, ok := s.Latest(anchor, anchor.Add(time.Second), at(ny, 2026, 11, 4, 9, 30), ny)
	if !ok || !got.Equal(at(ny, 2026, 11, 4, 9, 0)) {
		t.Errorf("got %v, %v; want 2026-11-04 09:00 local", got, ok)
	}
}

func TestDaylightSavingGap_RunsAtTheNextValidMinute(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-03-08: 02:00 EST jumps to 03:00 EDT, so 02:30 does not exist.
	s := Spec{Kind: KindWeekly, Weekdays: []int{0}, Times: []string{"02:30"}}
	anchor := at(ny, 2026, 1, 1, 0, 0)
	want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC) // 03:00 EDT
	got, ok := s.Latest(anchor, time.Time{}, time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), ny)
	if !ok || !got.Equal(want) {
		t.Errorf("got %v, %v; want %v (03:00 EDT)", got, ok, want)
	}
	// Not yet due an hour before the gap ends.
	if got, ok := s.Latest(anchor, time.Time{}, time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC), ny); ok && got.Equal(want) {
		t.Errorf("owed the gap run %v before the gap ended", got)
	}
	// Runs once: after it, nothing more that day.
	if got, ok := s.Latest(anchor, want, time.Date(2026, 3, 8, 23, 0, 0, 0, time.UTC), ny); ok {
		t.Errorf("owed %v after the gap run", got)
	}
}

func TestDaylightSavingRepeat_RunsOnce(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-11-01: 02:00 EDT falls back to 01:00 EST, so 01:30 happens twice.
	s := Spec{Kind: KindWeekly, Weekdays: []int{0}, Times: []string{"01:30"}}
	anchor := at(ny, 2026, 1, 1, 0, 0)
	first, ok := s.Latest(anchor, time.Time{}, time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC), ny) // 01:45 EDT
	if !ok || !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Fatalf("first pass: got %v, %v; want 05:30 UTC (01:30 EDT)", first, ok)
	}
	// An hour later the wall clock reads 01:30 again (EST): not a second run.
	if got, ok := s.Latest(anchor, first, time.Date(2026, 11, 1, 6, 45, 0, 0, time.UTC), ny); ok {
		t.Errorf("owed %v for the repeated 01:30", got)
	}
}

func TestNext(t *testing.T) {
	loc := time.UTC
	weekly := Spec{Kind: KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00", "14:30"}}
	anchor := at(loc, 2026, 1, 1, 0, 0)
	got, ok := weekly.Next(anchor, at(loc, 2026, 10, 28, 14, 30), loc) // Wed 14:30
	if !ok || !got.Equal(at(loc, 2026, 11, 2, 2, 0)) {
		t.Errorf("weekly next after Wed 14:30 = %v, %v; want Mon 2026-11-02 02:00", got, ok)
	}
	got, ok = weekly.Next(anchor, at(loc, 2026, 10, 28, 2, 0), loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 28, 14, 30)) {
		t.Errorf("weekly next after Wed 02:00 = %v, %v; want Wed 14:30", got, ok)
	}
	// Saved today: the first occurrence is after the save, not before it.
	saved := at(loc, 2026, 10, 28, 10, 0)
	got, ok = weekly.Next(saved, saved, loc)
	if !ok || !got.Equal(at(loc, 2026, 10, 28, 14, 30)) {
		t.Errorf("weekly next after saving = %v, %v; want today 14:30", got, ok)
	}

	every := Spec{Kind: KindEveryDays, EveryDays: 4}
	got, ok = every.Next(saved, saved.Add(-time.Hour), loc)
	if !ok || !got.Equal(saved) {
		t.Errorf("every-days next before the anchor = %v, %v; want the anchor", got, ok)
	}
	got, ok = every.Next(saved, saved, loc)
	if !ok || !got.Equal(at(loc, 2026, 11, 1, 10, 0)) {
		t.Errorf("every-days next after the anchor = %v, %v; want +4 days", got, ok)
	}
	if _, ok := (Spec{}).Next(saved, saved, loc); ok {
		t.Error("an invalid Spec has a next occurrence")
	}
}

func TestInvalidSpecAndInputsHaveNoOccurrences(t *testing.T) {
	loc := time.UTC
	now := at(loc, 2026, 10, 28, 12, 0)
	if _, ok := (Spec{}).Latest(now, time.Time{}, now, loc); ok {
		t.Error("an invalid Spec is due")
	}
	good := Spec{Kind: KindEveryDays, EveryDays: 1}
	if _, ok := good.Latest(time.Time{}, time.Time{}, now, loc); ok {
		t.Error("a Spec with no anchor is due")
	}
	if _, ok := good.Latest(now, time.Time{}, now, nil); ok {
		t.Error("a Spec with no zone is due")
	}
	if _, ok := good.Latest(now.Add(time.Hour), time.Time{}, now, loc); ok {
		t.Error("an every-days Spec is due before its anchor")
	}
}
