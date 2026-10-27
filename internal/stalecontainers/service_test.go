// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
)

func weekly() schedule.Spec {
	return schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{1, 3}, Times: []string{"02:00"}}
}

func report() SettingsInput {
	return SettingsInput{Mode: db.StaleContainerModeReport, Schedule: weekly(), HasSchedule: true, MinAgeHours: 24, ArchiveLines: 2000}
}

func TestUpdateSettings_AdminOnly(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.UpdateSettings(context.Background(), developer, report()); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	if _, err := f.svc.Settings(context.Background(), developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer Settings: %v, want ErrNotPermitted", err)
	}
	if f.settings.updates != 0 || len(f.audit.calls) != 0 {
		t.Error("a refused change changed or audited something")
	}
}

func TestUpdateSettings_RefusesWhatIsInvalid(t *testing.T) {
	bad := map[string]func(*SettingsInput){
		"unknown mode":              func(in *SettingsInput) { in.Mode = "delete" },
		"empty mode":                func(in *SettingsInput) { in.Mode = "" },
		"negative minimum age":      func(in *SettingsInput) { in.MinAgeHours = -1 },
		"minimum age over a month":  func(in *SettingsInput) { in.MinAgeHours = MaxMinAgeHours + 1 },
		"negative log lines":        func(in *SettingsInput) { in.ArchiveLines = -1 },
		"log lines over the cap":    func(in *SettingsInput) { in.ArchiveLines = MaxArchiveLines + 1 },
		"report without a schedule": func(in *SettingsInput) { in.Schedule, in.HasSchedule = schedule.Spec{}, false },
		"a schedule with no days":   func(in *SettingsInput) { in.Schedule.Weekdays = nil },
		"a schedule with bad times": func(in *SettingsInput) { in.Schedule.Times = []string{"25:00"} },
		"every 0 days":              func(in *SettingsInput) { in.Schedule = schedule.Spec{Kind: schedule.KindEveryDays} },
		"mode none with a bad schedule submitted": func(in *SettingsInput) {
			in.Mode, in.Schedule = db.StaleContainerModeNone, schedule.Spec{Kind: "cron"}
		},
	}
	for name, mutate := range bad {
		f := newFixture(t)
		in := report()
		mutate(&in)
		if err := f.svc.UpdateSettings(context.Background(), admin, in); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%s: %v, want ErrInvalidSettings", name, err)
		}
		if f.settings.updates != 0 || len(f.audit.calls) != 0 {
			t.Errorf("%s: a refused change changed or audited something", name)
		}
	}
	// The edges of the ranges are accepted.
	f := newFixture(t)
	for _, in := range []SettingsInput{
		{Mode: db.StaleContainerModeNone, MinAgeHours: 0, ArchiveLines: 0},
		{Mode: db.StaleContainerModeNone, MinAgeHours: MaxMinAgeHours, ArchiveLines: MaxArchiveLines},
	} {
		if err := f.svc.UpdateSettings(context.Background(), admin, in); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestUpdateSettings_PruneNeedsConfirmingEachTimeItIsSwitchedOn(t *testing.T) {
	ctx := context.Background()
	prune := func(confirm bool) SettingsInput {
		in := report()
		in.Mode, in.ConfirmPrune = db.StaleContainerModePrune, confirm
		return in
	}

	f := newFixture(t) // starts at none
	if err := f.svc.UpdateSettings(ctx, admin, prune(false)); !errors.Is(err, ErrPruneNotConfirmed) {
		t.Fatalf("none -> prune unconfirmed: %v, want ErrPruneNotConfirmed", err)
	}
	if f.settings.updates != 0 || f.settings.cfg.Mode != db.StaleContainerModeNone {
		t.Error("an unconfirmed Prune was saved")
	}
	if err := f.svc.UpdateSettings(ctx, admin, prune(true)); err != nil {
		t.Fatalf("none -> prune confirmed: %v", err)
	}
	if c := f.audit.calls[len(f.audit.calls)-1]; c.detail["confirmed_prune"] != true || c.detail["mode"] != "prune" || c.detail["previous_mode"] != "none" {
		t.Errorf("audit detail = %v", c.detail)
	}

	// Already Prune: editing it again does not ask.
	edit := prune(false)
	edit.MinAgeHours = 48
	if err := f.svc.UpdateSettings(ctx, admin, edit); err != nil {
		t.Errorf("prune -> prune (an edit): %v", err)
	}
	// Going down to Report never asks, and going back up asks again.
	down := report()
	if err := f.svc.UpdateSettings(ctx, admin, down); err != nil {
		t.Errorf("prune -> report: %v", err)
	}
	if err := f.svc.UpdateSettings(ctx, admin, prune(false)); !errors.Is(err, ErrPruneNotConfirmed) {
		t.Errorf("report -> prune unconfirmed: %v, want ErrPruneNotConfirmed", err)
	}
	// Ticking the box while not turning Prune on records nothing about it.
	rep := report()
	rep.ConfirmPrune = true
	if err := f.svc.UpdateSettings(ctx, admin, rep); err != nil {
		t.Fatal(err)
	}
	if c := f.audit.calls[len(f.audit.calls)-1]; c.detail["confirmed_prune"] != false {
		t.Errorf("a stray confirmation was recorded: %v", c.detail)
	}
}

func TestUpdateSettings_IsAuditedWithWhoAndWhat(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.UpdateSettings(context.Background(), admin, report()); err != nil {
		t.Fatal(err)
	}
	calls := f.audit.actions("updated_stale_container_settings")
	if len(calls) != 1 {
		t.Fatalf("%d audit records, want 1", len(calls))
	}
	c := calls[0]
	if c.actorID == nil || *c.actorID != "admin-1" || c.superAdmin || c.objectType != "stale_container_settings" ||
		c.detail["mode"] != "report" || c.detail["previous_mode"] != "none" || c.detail["min_age_hours"] != 24 || c.detail["archive_lines"] != 2000 || c.detail["schedule"] == nil {
		t.Errorf("audit = %+v", c)
	}
	if f.settings.cfg.UpdatedBy == nil || *f.settings.cfg.UpdatedBy != "admin-1" {
		t.Errorf("UpdatedBy = %v", f.settings.cfg.UpdatedBy)
	}

	f2 := newFixture(t)
	if err := f2.svc.UpdateSettings(context.Background(), breakGlass, report()); err != nil {
		t.Fatal(err)
	}
	if c := f2.audit.calls[0]; c.actorID != nil || !c.superAdmin {
		t.Errorf("break-glass audit = %+v, want a nil actor with the super-admin flag", c)
	}
	if f2.settings.cfg.UpdatedBy != nil {
		t.Error("break-glass UpdatedBy is set")
	}
}

func TestUpdateSettings_AFailedAuditWriteRevertsTheChange(t *testing.T) {
	f := newFixture(t)
	before := f.settings.cfg
	f.audit.err = errors.New("audit down")
	if err := f.svc.UpdateSettings(context.Background(), admin, report()); err == nil {
		t.Fatal("UpdateSettings succeeded without an audit record")
	}
	if !f.settings.restored || f.settings.cfg.Mode != before.Mode || f.settings.cfg.Schedule != nil || !f.settings.cfg.ScheduleSetAt.Equal(before.ScheduleSetAt) {
		t.Errorf("the change was not reverted exactly: %+v", f.settings.cfg)
	}
}

func TestUpdateSettings_SwitchingOffKeepsTheSavedSchedule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.svc.UpdateSettings(ctx, admin, report()); err != nil {
		t.Fatal(err)
	}
	saved := string(f.settings.cfg.Schedule)
	if err := f.svc.UpdateSettings(ctx, admin, SettingsInput{Mode: db.StaleContainerModeNone, MinAgeHours: 24, ArchiveLines: 2000}); err != nil {
		t.Fatal(err)
	}
	if f.settings.cfg.Mode != db.StaleContainerModeNone || string(f.settings.cfg.Schedule) != saved {
		t.Errorf("switching off lost the schedule: %s", f.settings.cfg.Schedule)
	}
}

func TestUpdateSettings_StoresANormalizedSchedule(t *testing.T) {
	f := newFixture(t)
	in := report()
	in.Schedule = schedule.Spec{Kind: schedule.KindWeekly, Weekdays: []int{5, 1}, Times: []string{"14:30", "02:00"}}
	if err := f.svc.UpdateSettings(context.Background(), admin, in); err != nil {
		t.Fatal(err)
	}
	var got schedule.Spec
	if err := json.Unmarshal(f.settings.cfg.Schedule, &got); err != nil || got.Weekdays[0] != 1 || got.Times[0] != "02:00" {
		t.Errorf("stored %s, want it sorted", f.settings.cfg.Schedule)
	}
}

func TestSettings_NextRun(t *testing.T) {
	f := newFixture(t) // 2026-10-26 12:00 UTC, a Monday
	ctx := context.Background()

	view, err := f.svc.Settings(ctx, admin)
	if err != nil || view.NextRun != nil || view.Spec != nil || view.LastRun != nil || view.Timezone != "UTC" {
		t.Fatalf("off: %+v, %v", view, err)
	}

	// Weekly, Monday and Wednesday at 02:00, saved Monday noon: Monday's 02:00
	// is already past, so the next run is Wednesday.
	if err := f.svc.UpdateSettings(ctx, admin, report()); err != nil {
		t.Fatal(err)
	}
	view, _ = f.svc.Settings(ctx, admin)
	if view.NextRun == nil || !view.NextRun.Equal(time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)) || view.Spec == nil || view.Spec.Kind != schedule.KindWeekly {
		t.Fatalf("weekly: next %v spec %+v", view.NextRun, view.Spec)
	}

	// Every 2 days: due the moment it is saved, then two days after its run.
	in := report()
	in.Schedule = schedule.Spec{Kind: schedule.KindEveryDays, EveryDays: 2}
	if err := f.svc.UpdateSettings(ctx, admin, in); err != nil {
		t.Fatal(err)
	}
	view, _ = f.svc.Settings(ctx, admin)
	if view.NextRun == nil || !view.NextRun.Equal(f.clock) {
		t.Errorf("every 2 days, never run: next %v, want now", view.NextRun)
	}
	if err := f.settings.MarkRun(ctx, f.clock); err != nil {
		t.Fatal(err)
	}
	view, _ = f.svc.Settings(ctx, admin)
	if view.NextRun == nil || !view.NextRun.Equal(f.clock.Add(48*time.Hour)) {
		t.Errorf("every 2 days, just run: next %v, want +48h", view.NextRun)
	}

	// A stored summary is decoded for display; garbage is ignored, not fatal.
	_ = f.settings.SetLastRunSummary(ctx, json.RawMessage(`{"mode":"report","stale":3,"nodes_scanned":2}`))
	view, _ = f.svc.Settings(ctx, admin)
	if view.LastRun == nil || view.LastRun.Stale != 3 || view.LastRun.NodesScanned != 2 {
		t.Errorf("last run = %+v", view.LastRun)
	}
	_ = f.settings.SetLastRunSummary(ctx, json.RawMessage(`not json`))
	if view, err := f.svc.Settings(ctx, admin); err != nil || view.LastRun != nil {
		t.Errorf("a garbled summary: %+v, %v", view, err)
	}
}

func TestJob_LoadReadsTheScheduleFreshAndRefusesABrokenOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job := f.svc.Job()

	st, err := job.Load(ctx)
	if err != nil || st.Enabled {
		t.Fatalf("off: %+v, %v", st, err)
	}
	if err := f.svc.UpdateSettings(ctx, admin, report()); err != nil {
		t.Fatal(err)
	}
	st, err = job.Load(ctx)
	if err != nil || !st.Enabled || st.Spec.Kind != schedule.KindWeekly || !st.Anchor.Equal(f.settings.cfg.ScheduleSetAt) || !st.LastRun.IsZero() {
		t.Fatalf("on: %+v, %v", st, err)
	}
	if err := job.Begin(ctx, f.clock); err != nil {
		t.Fatal(err)
	}
	if st, _ := job.Load(ctx); !st.LastRun.Equal(f.clock) {
		t.Errorf("Begin did not record the run: %v", st.LastRun)
	}

	f.settings.cfg.Schedule = json.RawMessage(`{"kind":"cron"}`)
	if _, err := job.Load(ctx); err == nil {
		t.Error("a broken saved schedule was loaded as if fine")
	}
	f.settings.cfg.Schedule = json.RawMessage(`not json`)
	if _, err := job.Load(ctx); err == nil {
		t.Error("an unreadable saved schedule was loaded as if fine")
	}
	f.settings.getErr = errors.New("db down")
	if _, err := job.Load(ctx); err == nil {
		t.Error("a database error was swallowed")
	}
}
