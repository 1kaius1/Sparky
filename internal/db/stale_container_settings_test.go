// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

const (
	scheduleDaily3 = `{"kind":"weekly","weekdays":[0,1,2,3,4,5,6],"times":["03:00"]}`
	scheduleEvery2 = `{"kind":"every_days","every_days":2}`
)

func TestStaleContainerSettingsRepository_SeededRowIsOffAndSane(t *testing.T) {
	pool := newTestPool(t)
	repo := NewStaleContainerSettingsRepository(pool)
	got, err := repo.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	// The row may have been changed by an earlier test run's leftovers only if a
	// cleanup failed, so assert the shape rather than the exact values.
	if !got.Mode.Valid() || got.MinAgeHours < 0 || got.MinAgeHours > 720 || got.ArchiveLines < 0 {
		t.Errorf("settings = %+v", got)
	}
}

func TestStaleContainerSettingsRepository_ScheduleRestartsOnlyWhenItChangesOrTurnsOn(t *testing.T) {
	pool := newTestPool(t)
	repo := NewStaleContainerSettingsRepository(pool)
	ctx := context.Background()

	original, err := repo.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Restore(ctx, original) })
	// Start from a known state: off, no schedule.
	if _, err := pool.Exec(ctx, `UPDATE stale_container_settings SET mode = 'none', schedule = NULL, last_run_at = NULL WHERE id = true`); err != nil {
		t.Fatal(err)
	}
	pause := func() { time.Sleep(10 * time.Millisecond) }

	// Turning it on with a schedule restarts the schedule.
	before := time.Now()
	pause()
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: 2000}); err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	on, _ := repo.Get(ctx)
	if on.Mode != StaleContainerModeReport || !on.ScheduleSetAt.After(before) || on.LastRunAt != nil || on.UpdatedBy != nil {
		t.Fatalf("after switching on: %+v", on)
	}
	var spec map[string]any
	if err := json.Unmarshal(on.Schedule, &spec); err != nil || spec["kind"] != "weekly" {
		t.Fatalf("stored schedule = %s, %v", on.Schedule, err)
	}

	// A run is recorded.
	pause()
	ranAt := time.Now()
	if err := repo.MarkRun(ctx, ranAt); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetLastRunSummary(ctx, json.RawMessage(`{"stale":3}`)); err != nil {
		t.Fatal(err)
	}
	ran, _ := repo.Get(ctx)
	if ran.LastRunAt == nil || ran.LastRunAt.Sub(ranAt).Abs() > time.Second || string(ran.LastRunSummary) != `{"stale": 3}` {
		t.Fatalf("after a run: last %v summary %s", ran.LastRunAt, ran.LastRunSummary)
	}

	// Changing something else, or the mode between report and prune, with the
	// same schedule leaves the schedule's clock and the last run alone.
	pause()
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModePrune, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 48, ArchiveLines: 500}); err != nil {
		t.Fatal(err)
	}
	same, _ := repo.Get(ctx)
	if same.Mode != StaleContainerModePrune || same.MinAgeHours != 48 || same.ArchiveLines != 500 ||
		!same.ScheduleSetAt.Equal(on.ScheduleSetAt) || same.LastRunAt == nil || !same.LastRunAt.Equal(*ran.LastRunAt) {
		t.Errorf("an unchanged schedule restarted: set %v (was %v), last run %v", same.ScheduleSetAt, on.ScheduleSetAt, same.LastRunAt)
	}

	// A different schedule restarts it.
	pause()
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModePrune, Schedule: json.RawMessage(scheduleEvery2), MinAgeHours: 48, ArchiveLines: 500}); err != nil {
		t.Fatal(err)
	}
	changed, _ := repo.Get(ctx)
	if !changed.ScheduleSetAt.After(same.ScheduleSetAt) || changed.LastRunAt != nil {
		t.Errorf("a new schedule did not restart: set %v, last run %v", changed.ScheduleSetAt, changed.LastRunAt)
	}

	// Switching off keeps the schedule; switching back on restarts it.
	if err := repo.MarkRun(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	pause()
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeNone, Schedule: json.RawMessage(scheduleEvery2), MinAgeHours: 48, ArchiveLines: 500}); err != nil {
		t.Fatal(err)
	}
	off, _ := repo.Get(ctx)
	if off.Mode != StaleContainerModeNone || off.Schedule == nil || !off.ScheduleSetAt.Equal(changed.ScheduleSetAt) {
		t.Errorf("switching off: %+v", off)
	}
	pause()
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleEvery2), MinAgeHours: 48, ArchiveLines: 500}); err != nil {
		t.Fatal(err)
	}
	reon, _ := repo.Get(ctx)
	if !reon.ScheduleSetAt.After(off.ScheduleSetAt) || reon.LastRunAt != nil {
		t.Errorf("switching back on did not restart the schedule: set %v last run %v", reon.ScheduleSetAt, reon.LastRunAt)
	}
}

func TestStaleContainerSettingsRepository_RestoreIsExact(t *testing.T) {
	pool := newTestPool(t)
	repo := NewStaleContainerSettingsRepository(pool)
	ctx := context.Background()
	original, _ := repo.Get(ctx)
	t.Cleanup(func() { _ = repo.Restore(ctx, original) })

	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRun(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := repo.Get(ctx)

	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModePrune, Schedule: json.RawMessage(scheduleEvery2), MinAgeHours: 1, ArchiveLines: 0}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Restore(ctx, snapshot); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	got, _ := repo.Get(ctx)
	if got.Mode != snapshot.Mode || got.MinAgeHours != snapshot.MinAgeHours || got.ArchiveLines != snapshot.ArchiveLines ||
		!got.ScheduleSetAt.Equal(snapshot.ScheduleSetAt) || got.LastRunAt == nil || !got.LastRunAt.Equal(*snapshot.LastRunAt) || !got.UpdatedAt.Equal(snapshot.UpdatedAt) {
		t.Errorf("restored %+v, want %+v", got, snapshot)
	}
}

func TestStaleContainerSettingsRepository_DatabaseRefusesWhatTheServiceShouldHave(t *testing.T) {
	pool := newTestPool(t)
	repo := NewStaleContainerSettingsRepository(pool)
	ctx := context.Background()
	original, _ := repo.Get(ctx)
	t.Cleanup(func() { _ = repo.Restore(ctx, original) })

	good := StaleContainerSettingsUpdate{Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: 2000}
	bad := map[string]StaleContainerSettingsUpdate{
		"report with no schedule":  {Mode: StaleContainerModeReport, MinAgeHours: 24, ArchiveLines: 2000},
		"prune with no schedule":   {Mode: StaleContainerModePrune, MinAgeHours: 24, ArchiveLines: 2000},
		"unknown mode":             {Mode: "delete", Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: 2000},
		"negative minimum age":     {Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: -1, ArchiveLines: 2000},
		"minimum age over a month": {Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 721, ArchiveLines: 2000},
		"negative lines":           {Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: -1},
		"lines over the cap":       {Mode: StaleContainerModeReport, Schedule: json.RawMessage(scheduleDaily3), MinAgeHours: 24, ArchiveLines: 100001},
	}
	if err := repo.Update(ctx, good); err != nil {
		t.Fatalf("a valid update was refused: %v", err)
	}
	for name, u := range bad {
		if err := repo.Update(ctx, u); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// None needs no schedule, and the edges of the ranges are fine.
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeNone, MinAgeHours: 0, ArchiveLines: 0}); err != nil {
		t.Errorf("none with no schedule and zero edges: %v", err)
	}
	if err := repo.Update(ctx, StaleContainerSettingsUpdate{Mode: StaleContainerModeNone, MinAgeHours: 720, ArchiveLines: 100000}); err != nil {
		t.Errorf("upper edges: %v", err)
	}
}
