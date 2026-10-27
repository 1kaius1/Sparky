// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// setMode puts the monitoring in a mode with a schedule, the way the Settings
// page would, with the given archive line count.
func (f *fixture) setMode(t *testing.T, mode db.StaleContainerMode, lines int) {
	t.Helper()
	in := report()
	in.Mode, in.ArchiveLines, in.ConfirmPrune = mode, lines, true
	if err := f.svc.UpdateSettings(context.Background(), admin, in); err != nil {
		t.Fatal(err)
	}
	f.audit.calls = nil // the setup is not what a test is about
}

func (f *fixture) summary(t *testing.T) RunSummary {
	t.Helper()
	var s RunSummary
	if err := json.Unmarshal(f.settings.cfg.LastRunSummary, &s); err != nil {
		t.Fatalf("no readable run summary: %v (%s)", err, f.settings.cfg.LastRunSummary)
	}
	return s
}

func TestRunScheduled_ModeNoneDoesNothing(t *testing.T) {
	f := newFixture(t)
	f.populate()
	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.agents.lists()) != 0 || len(f.alerts.raised) != 0 || f.settings.cfg.LastRunSummary != nil {
		t.Error("a run with the monitoring off did something")
	}
}

func TestRunScheduled_ReportRaisesAlertsAndNeverRemoves(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModeReport, 2000)
	// An alert left from earlier for the offline node must be left alone: its
	// containers are unknown, not gone.
	f.alerts.open[staleKey(nodeB)] = alerts.RaiseParams{Kind: "stale_containers"}

	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.agents.removes(); len(got) != 0 {
		t.Fatalf("Report sent remove commands: %v", got)
	}
	if len(f.audit.calls) != 0 {
		t.Errorf("Report audited %d things; it changes nothing", len(f.audit.calls))
	}

	stale := f.alerts.ofKind("stale_containers")
	if len(stale) != 1 {
		t.Fatalf("%d stale alerts, want one for the node", len(stale))
	}
	p := stale[0]
	if p.DedupeKey != staleKey(nodeA) || p.Severity != db.AlertSeverityWarning || p.ObjectType != "stale_containers" || p.ObjectID != nodeA ||
		len(p.Audiences) != 1 || p.Audiences[0].Type != db.AlertAudienceTier || p.Audiences[0].MinTier != db.TierAdmin {
		t.Errorf("stale alert = %+v", p)
	}
	if p.Title != "2 stale containers on spark-1" || !strings.Contains(p.Body, "sparky-qwen-10000000") || !strings.Contains(p.Body, "Container cleanup") ||
		!strings.Contains(p.Body, "profile qwen-8b") || !strings.Contains(p.Body, "exit code 137") {
		t.Errorf("stale alert text = %q / %q", p.Title, p.Body)
	}
	orphans := f.alerts.ofKind("orphan_containers")
	if len(orphans) != 1 || orphans[0].DedupeKey != orphanKey(nodeA) || !strings.Contains(orphans[0].Body, inst4) || !strings.Contains(orphans[0].Body, "never removes") {
		t.Errorf("orphan alerts = %+v", orphans)
	}
	if !f.alerts.isOpen(staleKey(nodeB)) {
		t.Error("the offline node's alert was closed on no evidence")
	}

	sum := f.summary(t)
	if sum.Mode != "report" || sum.Stale != 2 || sum.Recent != 1 || sum.Orphans != 1 || sum.NodesScanned != 1 || sum.NodesSkipped != 1 || sum.Removed != 0 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestRunScheduled_AlertsCloseWhenTheConditionClears(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModeReport, 2000)
	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.alerts.isOpen(staleKey(nodeA)) || !f.alerts.isOpen(orphanKey(nodeA)) {
		t.Fatal("the alerts were not raised")
	}

	// Someone cleans up by hand and stops the orphan.
	f.agents.holdings[nodeA] = nil
	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.alerts.isOpen(staleKey(nodeA)) || f.alerts.isOpen(orphanKey(nodeA)) {
		t.Error("an alert stayed open after its condition cleared")
	}
}

func TestRunScheduled_PruneRemovesOnlyWhatIsStale(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModePrune, 500)

	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Only the two stale ones, oldest first, with the configured line count.
	want := []string{fmt.Sprintf("remove:%s:%s:500", nodeA, inst1), fmt.Sprintf("remove:%s:%s:500", nodeA, inst5)}
	if got := f.agents.removes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("removes = %v, want %v and nothing for the live, recent or orphan containers", got, want)
	}
	if f.agents.maxInFlight != 1 {
		t.Errorf("%d removals ran at once, want one at a time", f.agents.maxInFlight)
	}

	audits := f.audit.actions("removed_stale_container")
	if len(audits) != 2 {
		t.Fatalf("%d audit records, want one per removal", len(audits))
	}
	for i, a := range audits {
		if a.actorID != nil || a.superAdmin {
			t.Errorf("audit %d actor = %v superadmin %v, want the system (no actor, not break-glass)", i, a.actorID, a.superAdmin)
		}
		if a.objectType != "container" || a.detail["trigger"] != "scheduled_prune" || a.detail["node_name"] != "spark-1" || a.detail["lines_archived"] != 500 {
			t.Errorf("audit %d = %+v", i, a)
		}
	}
	if audits[0].objectID != inst1 || audits[0].detail["profile_name"] != "qwen-8b" || audits[0].detail["exit_code"] != 137 {
		t.Errorf("first audit = %+v", audits[0])
	}

	// What was removed no longer counts: no stale alert, an info summary, the
	// orphan still reported.
	if f.alerts.isOpen(staleKey(nodeA)) {
		t.Error("the stale alert stayed open after everything stale was removed")
	}
	if !f.alerts.isOpen(orphanKey(nodeA)) {
		t.Error("the running orphan was not reported")
	}
	removed := f.alerts.ofKind("stale_containers_removed")
	if len(removed) != 1 || removed[0].Severity != db.AlertSeverityInfo || removed[0].Title != "Stale container cleanup removed 2 containers" ||
		removed[0].DedupeKey != "" || !strings.Contains(removed[0].Body, "spark-1") {
		t.Errorf("summary alerts = %+v", removed)
	}
	if sum := f.summary(t); sum.Mode != "prune" || sum.Removed != 2 || sum.Stale != 2 || sum.Kept != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestRunScheduled_PruneKeepsWhatItCouldNotArchive(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModePrune, 2000)
	f.agents.removeOutcome[inst1] = agentproto.RemoveOutcomeKept
	f.agents.removeDetail[inst1] = "the log could not be archived, so the container was left in place"
	f.agents.removeOutcome[inst5] = agentproto.RemoveOutcomeRefused
	f.agents.removeDetail[inst5] = "the container is running; only stopped containers are removed"

	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}

	if audits := f.audit.actions("removed_stale_container"); len(audits) != 0 {
		t.Errorf("%d removals audited though nothing was removed", len(audits))
	}
	kept := f.alerts.ofKind("stale_container_kept")
	if len(kept) != 1 || kept[0].DedupeKey != keptKeyPrefix(nodeA)+inst1 || kept[0].Severity != db.AlertSeverityWarning || kept[0].ObjectID != nodeA ||
		!strings.Contains(kept[0].Body, "log could not be saved") || !strings.Contains(kept[0].Body, "left in place") {
		t.Errorf("kept alerts = %+v", kept)
	}
	// Both are still there, so the node is still reported as holding stale ones.
	stale := f.alerts.ofKind("stale_containers")
	if len(stale) == 0 || stale[len(stale)-1].Title != "2 stale containers on spark-1" {
		t.Errorf("stale alerts = %+v", stale)
	}
	if got := f.alerts.ofKind("stale_containers_removed"); len(got) != 0 {
		t.Errorf("a cleanup summary was raised though nothing was removed: %+v", got)
	}
	if sum := f.summary(t); sum.Removed != 0 || sum.Kept != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v", sum)
	}

	// Once the container is gone, its kept alert closes: the prefix pass is
	// given only the containers still present.
	f.agents.holdings[nodeA] = nil
	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.alerts.isOpen(keptKeyPrefix(nodeA) + inst1) {
		t.Error("the kept alert stayed open after its container disappeared")
	}
}

func TestRunScheduled_PruneCountsWhatIsAlreadyGoneAndASilentAgent(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModePrune, 2000)
	f.agents.removeOutcome[inst1] = agentproto.RemoveOutcomeGone
	f.agents.removeSilent[inst5] = true

	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	sum := f.summary(t)
	if sum.Gone != 1 || sum.Failed != 1 || sum.Removed != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if len(f.audit.actions("removed_stale_container")) != 0 {
		t.Error("something was audited as removed that was not")
	}
	// The one that vanished no longer counts; the one that never answered does.
	if got := f.alerts.ofKind("stale_containers"); len(got) == 0 || got[len(got)-1].Title != "1 stale container on spark-1" {
		t.Errorf("stale alerts = %+v", got)
	}
}

func TestRunScheduled_AFailedScanTouchesNothing(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModePrune, 2000)
	f.instances.err = errors.New("db down")
	if err := f.svc.runScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.agents.removes(); len(got) != 0 {
		t.Errorf("removed %v though the instance lookup failed", got)
	}
	if sum := f.summary(t); sum.NodesScanned != 0 || sum.NodesSkipped != 2 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestJob_RunRunsTheScheduledWork(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModeReport, 2000)
	if err := f.svc.Job().Run(context.Background(), f.clock); err != nil {
		t.Fatal(err)
	}
	if len(f.alerts.ofKind("stale_containers")) != 1 {
		t.Error("the job's Run did not do the scheduled work")
	}
}

// ---- the manual removal ----

func TestRemoveOne_Refusals(t *testing.T) {
	f := newFixture(t)
	f.populate()
	ctx := context.Background()

	if _, err := f.svc.RemoveOne(ctx, developer, nodeA, inst1, 100); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	for name, args := range map[string][3]any{
		"bad node id":     {"nope", inst1, 100},
		"bad instance id": {nodeA, "nope", 100},
		"injected id":     {nodeA, "x'; DROP TABLE x;--", 100},
		"negative lines":  {nodeA, inst1, -1},
		"too many lines":  {nodeA, inst1, MaxArchiveLines + 1},
	} {
		if _, err := f.svc.RemoveOne(ctx, admin, args[0].(string), args[1].(string), args[2].(int)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.svc.RemoveOne(ctx, admin, "aaaaaaaa-0000-4000-8000-0000000000ff", inst1, 100); !errors.Is(err, ErrNodeUnavailable) {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := f.svc.RemoveOne(ctx, admin, nodeBare, inst1, 100); !errors.Is(err, ErrNodeUnavailable) {
		t.Errorf("bare-metal node: %v", err)
	}
	if _, err := f.svc.RemoveOne(ctx, admin, nodeB, inst1, 100); !errors.Is(err, ErrNodeUnavailable) { // populate() took B offline
		t.Errorf("offline node: %v", err)
	}
	if len(f.agents.removes()) != 0 || len(f.audit.calls) != 0 {
		t.Error("a refused request removed or audited something")
	}
}

// Whatever the page showed, the container must be stale now by the same rules
// every mode uses. Nothing live, recent or unrecorded-and-running is ever sent.
func TestRemoveOne_RefusesAnythingNotStaleNow(t *testing.T) {
	f := newFixture(t)
	f.populate()
	for name, c := range map[string]struct{ id, why string }{
		"too recent":      {inst2, "less than the 24 hours minimum"},
		"live":            {inst3, "running"},
		"running orphan":  {inst4, "no record"},
		"a dead instance": {inst6, "Unload"},
	} {
		_, err := f.svc.RemoveOne(context.Background(), admin, nodeA, c.id, 100)
		if !errors.Is(err, ErrNotStale) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want ErrNotStale mentioning %q", name, err, c.why)
		}
	}
	if got := f.agents.removes(); len(got) != 0 {
		t.Errorf("sent remove commands for containers that are not stale: %v", got)
	}
}

func TestRemoveOne_RemovesAndAuditsWhoAsked(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.setMode(t, db.StaleContainerModeReport, 2000)
	if err := f.svc.runScheduled(context.Background()); err != nil { // raises the stale alert
		t.Fatal(err)
	}

	res, err := f.svc.RemoveOne(context.Background(), admin, nodeA, strings.ToUpper(inst1), 50)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != agentproto.RemoveOutcomeRemoved || res.ContainerName != "sparky-qwen-10000000" || res.ProfileName != "qwen-8b" {
		t.Errorf("result = %+v", res)
	}
	if got := f.agents.removes(); len(got) != 1 || got[0] != fmt.Sprintf("remove:%s:%s:50", nodeA, inst1) {
		t.Errorf("removes = %v", got)
	}
	audits := f.audit.actions("removed_stale_container")
	if len(audits) != 1 || audits[0].actorID == nil || *audits[0].actorID != "admin-1" || audits[0].superAdmin ||
		audits[0].objectID != inst1 || audits[0].detail["trigger"] != "manual" || audits[0].detail["lines_archived"] != 50 {
		t.Errorf("audit = %+v", audits)
	}
	// One stale container is left, so the node's alert is updated, not closed.
	stale := f.alerts.ofKind("stale_containers")
	if got := stale[len(stale)-1]; got.Title != "1 stale container on spark-1" || !f.alerts.isOpen(staleKey(nodeA)) {
		t.Errorf("alert after the first removal = %+v", got)
	}

	// Removing the last one closes it.
	if _, err := f.svc.RemoveOne(context.Background(), breakGlass, nodeA, inst5, 0); err != nil {
		t.Fatal(err)
	}
	if f.alerts.isOpen(staleKey(nodeA)) {
		t.Error("the stale alert stayed open after the last stale container was removed")
	}
	audits = f.audit.actions("removed_stale_container")
	if len(audits) != 2 || audits[1].actorID != nil || !audits[1].superAdmin {
		t.Errorf("break-glass audit = %+v", audits[len(audits)-1])
	}
	if got := f.agents.removes(); got[1] != fmt.Sprintf("remove:%s:%s:0", nodeA, inst5) {
		t.Errorf("lines 0 (all) was not sent as 0: %v", got)
	}
}

func TestRemoveOne_Outcomes(t *testing.T) {
	ctx := context.Background()

	// The agent could not archive the log: kept, with a warning for the Admins.
	f := newFixture(t)
	f.populate()
	f.agents.removeOutcome[inst1] = agentproto.RemoveOutcomeKept
	f.agents.removeDetail[inst1] = "the log could not be archived, so the container was left in place"
	res, err := f.svc.RemoveOne(ctx, admin, nodeA, inst1, 100)
	if err != nil || res.Outcome != agentproto.RemoveOutcomeKept || !strings.Contains(res.Detail, "left in place") {
		t.Errorf("kept: %+v, %v", res, err)
	}
	if len(f.alerts.ofKind("stale_container_kept")) != 1 || len(f.audit.calls) != 0 {
		t.Error("a kept container should raise its alert and audit nothing")
	}

	// The container vanished before the node was asked.
	f = newFixture(t)
	f.populate()
	res, err = f.svc.RemoveOne(ctx, admin, nodeA, "10000000-0000-4000-8000-0000000000aa", 100)
	if err != nil || res.Outcome != agentproto.RemoveOutcomeGone {
		t.Errorf("gone: %+v, %v", res, err)
	}
	if len(f.agents.removes()) != 0 {
		t.Error("asked a node to remove a container it does not hold")
	}

	// The agent never answers.
	f = newFixture(t)
	f.populate()
	f.agents.removeSilent[inst1] = true
	res, err = f.svc.RemoveOne(ctx, admin, nodeA, inst1, 100)
	if err != nil || res.Outcome != agentproto.RemoveOutcomeFailed || !strings.Contains(res.Detail, "did not answer") || len(f.audit.calls) != 0 {
		t.Errorf("silent: %+v, %v, %d audit", res, err, len(f.audit.calls))
	}

	// An answer this server does not understand is a failure, not a success.
	f = newFixture(t)
	f.populate()
	f.agents.removeOutcome[inst1] = "exploded"
	res, _ = f.svc.RemoveOne(ctx, admin, nodeA, inst1, 100)
	if res.Outcome != agentproto.RemoveOutcomeFailed || len(f.audit.calls) != 0 {
		t.Errorf("unknown outcome: %+v", res)
	}

	// Removed, but the audit entry could not be written: reported loudly.
	f = newFixture(t)
	f.populate()
	f.audit.err = errors.New("audit down")
	res, err = f.svc.RemoveOne(ctx, admin, nodeA, inst1, 100)
	if !errors.Is(err, ErrAuditAfterRemoval) || res.Outcome != agentproto.RemoveOutcomeRemoved {
		t.Errorf("audit failure: %+v, %v", res, err)
	}
}

func TestRemoveOne_AnotherNodesAnswerDoesNotCount(t *testing.T) {
	f := newFixture(t)
	f.populate()
	// Node B answers for node A's removal: the answer is ignored and the
	// removal times out rather than being believed.
	f.svc.removeTimeout = 100 * time.Millisecond
	f.agents.removeSilent[inst1] = true
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.svc.mu.Lock()
		var id string
		for k := range f.svc.removeWaits {
			id = k
		}
		f.svc.mu.Unlock()
		env, _ := agentproto.NewEnvelope(agentproto.TypeRemoveContainerResult, "", agentproto.RemoveContainerResult{RemoveID: id, InstanceID: inst1, Outcome: "removed"})
		f.svc.HandleRemoveContainerResult(nodeB, env)
	}()
	res, _ := f.svc.RemoveOne(context.Background(), admin, nodeA, inst1, 100)
	if res.Outcome != agentproto.RemoveOutcomeFailed || len(f.audit.calls) != 0 {
		t.Errorf("a removal was believed on another node's word: %+v", res)
	}
}

func TestRemoveOne_RemovalsNeverOverlap(t *testing.T) {
	f := newFixture(t)
	f.populate()
	f.agents.removeDelay = 30 * time.Millisecond
	f.svc.removeTimeout = 2 * time.Second
	var wg sync.WaitGroup
	for _, id := range []string{inst1, inst5} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := f.svc.RemoveOne(context.Background(), admin, nodeA, id, 10); err != nil {
				t.Errorf("RemoveOne(%s): %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	if len(f.agents.removes()) != 2 || f.agents.maxInFlight != 1 {
		t.Errorf("%d removals sent, %d at once; want 2, one at a time", len(f.agents.removes()), f.agents.maxInFlight)
	}
}
