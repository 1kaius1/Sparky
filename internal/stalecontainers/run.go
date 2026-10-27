// SPDX-License-Identifier: AGPL-3.0-or-later

package stalecontainers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
)

// Alert object type: the inbox links every alert of this feature to the
// Container cleanup page (internal/httpapi's alertObjectLink).
const alertObjectType = "stale_containers"

// maxListedInAlert bounds how many containers an alert body names.
const maxListedInAlert = 10

// RunSummary is what one scheduled run found and did. It is stored as JSON on
// the settings row and shown on the Settings page.
type RunSummary struct {
	Mode       string    `json:"mode"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// NodesScanned counts nodes that answered; NodesSkipped those that were
	// offline or did not answer, whose containers are unknown.
	NodesScanned int `json:"nodes_scanned"`
	NodesSkipped int `json:"nodes_skipped"`

	// Stale is how many stale containers the scan found; Recent how many it left
	// for being stopped less than the minimum age; Orphans how many running
	// containers had no instance record.
	Stale   int `json:"stale"`
	Recent  int `json:"recent"`
	Orphans int `json:"orphans"`

	// The rest apply to Prune only. Removed had their log archived and were
	// removed; Gone had already disappeared; Kept had a log that could not be
	// archived and were left in place; Failed were refused or errored.
	Removed int `json:"removed"`
	Gone    int `json:"gone"`
	Kept    int `json:"kept"`
	Failed  int `json:"failed"`
}

// RemoveResult is how removing one container went.
type RemoveResult struct {
	// Outcome is one of agentproto's RemoveOutcome values.
	Outcome       string
	Detail        string
	ContainerName string
	ProfileName   string
}

// Job is the scheduled monitoring, for schedule.Runner. Its schedule, mode and
// last run are read from the settings row at every check, so a change on the
// Settings page applies without a restart.
func (s *Service) Job() schedule.Job {
	return schedule.Job{
		Name: "stale container monitoring",
		Load: func(ctx context.Context) (schedule.State, error) {
			cfg, err := s.settings.Get(ctx)
			if err != nil {
				return schedule.State{}, err
			}
			st := schedule.State{Enabled: cfg.Mode != db.StaleContainerModeNone, Anchor: cfg.ScheduleSetAt}
			if cfg.LastRunAt != nil {
				st.LastRun = *cfg.LastRunAt
			}
			if st.Enabled {
				if err := json.Unmarshal(cfg.Schedule, &st.Spec); err != nil {
					return schedule.State{}, fmt.Errorf("the saved schedule cannot be read: %w", err)
				}
				if err := st.Spec.Validate(); err != nil {
					return schedule.State{}, err
				}
			}
			return st, nil
		},
		Begin: func(ctx context.Context, startedAt time.Time) error { return s.settings.MarkRun(ctx, startedAt) },
		Run:   func(ctx context.Context, _ time.Time) error { return s.runScheduled(ctx) },
	}
}

// runScheduled is one scheduled run: scan every container node, remove what is
// stale if the mode is Prune, and bring the alerts in line with what is left.
func (s *Service) runScheduled(ctx context.Context) error {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get stale container settings: %w", err)
	}
	if cfg.Mode == db.StaleContainerModeNone {
		// Switched off between the check and now.
		return nil
	}
	all, err := s.nodes.List(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	sum := RunSummary{Mode: string(cfg.Mode), StartedAt: s.now()}
	scans := s.scan(ctx, containerNodes(all), time.Duration(cfg.MinAgeHours)*time.Hour)

	gone := make(map[string]bool) // instance ids removed, or found already gone
	if cfg.Mode == db.StaleContainerModePrune {
		s.prune(ctx, scans, cfg.ArchiveLines, &sum, gone)
	}
	s.syncAlerts(ctx, scans, gone, &sum)

	sum.FinishedAt = s.now()
	raw, err := json.Marshal(sum)
	if err != nil {
		return fmt.Errorf("encode run summary: %w", err)
	}
	if err := s.settings.SetLastRunSummary(ctx, raw); err != nil {
		return err
	}
	s.logger.Printf("stalecontainers: %s run: %d stale, %d recent, %d removed, %d kept, %d failed, %d of %d nodes scanned",
		sum.Mode, sum.Stale, sum.Recent, sum.Removed, sum.Kept, sum.Failed, sum.NodesScanned, sum.NodesScanned+sum.NodesSkipped)
	return nil
}

// prune removes every stale container the scan found, one at a time, and says
// in sum and gone what happened. A failure to remove one never stops the rest.
func (s *Service) prune(ctx context.Context, scans []NodeScan, lines int, sum *RunSummary, gone map[string]bool) {
	var removedNames = map[string][]string{} // node name -> names of what was removed
	for i := range scans {
		ns := &scans[i]
		if ns.Status != ScanDone {
			continue
		}
		for _, it := range ns.With(VerdictStale) {
			if ctx.Err() != nil {
				return
			}
			res, err := s.removeItem(ctx, ns, it, lines, "scheduled_prune", nil, false)
			if err != nil {
				s.logger.Printf("stalecontainers: %v", err)
			}
			switch res.Outcome {
			case agentproto.RemoveOutcomeRemoved:
				sum.Removed++
				gone[it.InstanceID] = true
				removedNames[ns.NodeName] = append(removedNames[ns.NodeName], it.ContainerName)
			case agentproto.RemoveOutcomeGone:
				sum.Gone++
				gone[it.InstanceID] = true
			case agentproto.RemoveOutcomeKept:
				sum.Kept++
			default:
				sum.Failed++
			}
		}
	}
	if sum.Removed > 0 {
		s.raiseRemovedSummary(ctx, removedNames, sum.Removed)
	}
}

// RemoveOne removes one stale container on request - the manual cleanup page's
// per-row button. It scans the node afresh and refuses unless the container is
// stale right now by the same rules every mode uses, so the page being stale
// itself cannot get a live container removed. lines is how many trailing lines
// of the log to archive first (0 is all). Requires
// rbac.CanManageStaleContainers. A removal is audited as
// removed_stale_container, naming who asked.
func (s *Service) RemoveOne(ctx context.Context, actor rbac.Actor, nodeID, instanceID string, lines int) (RemoveResult, error) {
	if !rbac.CanManageStaleContainers(actor) {
		return RemoveResult{}, rbac.ErrNotPermitted
	}
	if !uuidPattern.MatchString(nodeID) || !uuidPattern.MatchString(instanceID) {
		return RemoveResult{}, fmt.Errorf("%w: malformed id", ErrInvalidRequest)
	}
	if lines < 0 || lines > MaxArchiveLines {
		return RemoveResult{}, fmt.Errorf("%w: log lines must be between 0 (all) and %d", ErrInvalidRequest, MaxArchiveLines)
	}
	instanceID = strings.ToLower(instanceID)

	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return RemoveResult{}, fmt.Errorf("get stale container settings: %w", err)
	}
	node, err := s.nodes.FindByID(ctx, strings.ToLower(nodeID))
	if err != nil || node == nil {
		return RemoveResult{}, fmt.Errorf("%w: no such node", ErrNodeUnavailable)
	}
	if len(containerNodes([]*db.Node{node})) == 0 {
		return RemoveResult{}, fmt.Errorf("%w: %s does not run containers", ErrNodeUnavailable, node.Name)
	}

	scans := s.scan(ctx, []*db.Node{node}, time.Duration(cfg.MinAgeHours)*time.Hour)
	ns := &scans[0]
	if ns.Status != ScanDone {
		return RemoveResult{}, fmt.Errorf("%w: %s: %s", ErrNodeUnavailable, node.Name, ns.Error)
	}
	var found *Item
	for i := range ns.Items {
		if ns.Items[i].InstanceID == instanceID {
			found = &ns.Items[i]
			break
		}
	}
	if found == nil {
		return RemoveResult{Outcome: agentproto.RemoveOutcomeGone, Detail: "that container is already gone"}, nil
	}
	if found.Verdict != VerdictStale {
		return RemoveResult{}, fmt.Errorf("%w: %s", ErrNotStale, found.Reason)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	res, err := s.removeItem(ctx, ns, *found, lines, "manual", actorID, actor.IsSuperAdmin)

	gone := map[string]bool{}
	if res.Outcome == agentproto.RemoveOutcomeRemoved || res.Outcome == agentproto.RemoveOutcomeGone {
		gone[instanceID] = true
	}
	s.syncAlerts(ctx, scans, gone, &RunSummary{})
	return res, err
}

// removeItem asks the node to remove one stale container, then audits it if it
// went. Removals are serialized across the scheduled run and the manual page.
// The result is returned even when the request itself failed (as a failed
// outcome), so callers tally every case the same way; the error is non-nil only
// for ErrAuditAfterRemoval.
func (s *Service) removeItem(ctx context.Context, ns *NodeScan, it Item, lines int, trigger string, actorID *string, superAdmin bool) (RemoveResult, error) {
	s.removeMu.Lock()
	defer s.removeMu.Unlock()

	out := RemoveResult{ContainerName: it.ContainerName, ProfileName: it.ProfileName}
	res, err := s.requestRemove(ctx, ns.NodeID, it.InstanceID, lines)
	if err != nil {
		out.Outcome, out.Detail = agentproto.RemoveOutcomeFailed, err.Error()
		return out, nil
	}
	out.Outcome, out.Detail = res.Outcome, printable(res.Detail, 1000)

	switch res.Outcome {
	case agentproto.RemoveOutcomeRemoved:
		detail := map[string]any{
			"node_id": ns.NodeID, "node_name": ns.NodeName, "trigger": trigger,
			"container_name": it.ContainerName, "container_id": it.ContainerID, "state": it.State,
			"profile_id": it.ProfileID, "profile_name": it.ProfileName, "lines_archived": lines,
		}
		if it.ExitCode != nil {
			detail["exit_code"] = *it.ExitCode
		}
		if err := s.audit.Record(ctx, actorID, superAdmin, "removed_stale_container", "container", it.InstanceID, detail); err != nil {
			return out, fmt.Errorf("%w: container %s on %s: %v", ErrAuditAfterRemoval, it.ContainerName, ns.NodeName, err)
		}
	case agentproto.RemoveOutcomeKept:
		s.raiseKept(ctx, ns, it, out.Detail)
	}
	return out, nil
}

// audiences is who stale container alerts go to: Admins, who are the only
// people who can act on them.
func audiences() []db.AlertAudience {
	return []db.AlertAudience{{Type: db.AlertAudienceTier, MinTier: db.TierAdmin}}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// keptKeyPrefix and the two keys below tie each alert to the thing it is
// about, so it is raised again rather than duplicated and closed when the
// condition clears.
func keptKeyPrefix(nodeID string) string { return "stale_container_kept:" + nodeID + ":" }
func staleKey(nodeID string) string      { return "stale_containers:" + nodeID }
func orphanKey(nodeID string) string     { return "orphan_containers:" + nodeID }

// raiseKept tells the Admins one container could not have its log archived and
// was therefore left in place - removing it would have lost the log.
func (s *Service) raiseKept(ctx context.Context, ns *NodeScan, it Item, reason string) {
	who := it.ContainerName
	if it.ProfileName != "" {
		who += " (profile " + it.ProfileName + ")"
	}
	body := "The container " + who + " on " + ns.NodeName + " is stale, but its log could not be saved, so it was kept " +
		"rather than lose the log that explains how it ended."
	if reason != "" {
		body += " The agent reported: " + reason + "."
	}
	body += " Open Container cleanup to try again, or inspect the container on the node."
	_, err := s.alerts.Raise(ctx, alerts.RaiseParams{
		Kind: "stale_container_kept", Severity: db.AlertSeverityWarning,
		Title:      "A stale container on " + ns.NodeName + " was kept because its log could not be saved",
		Body:       clipRunes(body, alerts.MaxBodyLen),
		ObjectType: alertObjectType, ObjectID: ns.NodeID,
		DedupeKey: keptKeyPrefix(ns.NodeID) + it.InstanceID, Audiences: audiences(),
	})
	if err != nil {
		s.logger.Printf("stalecontainers: raise kept alert for %s on %s: %v", it.ContainerName, ns.NodeName, err)
	}
}

// raiseRemovedSummary records, as an info alert, what a Prune run removed.
func (s *Service) raiseRemovedSummary(ctx context.Context, byNode map[string][]string, total int) {
	var b strings.Builder
	b.WriteString("Scheduled Prune archived each container's log and then removed these stale containers:\n")
	listed := 0
	for node, names := range byNode {
		for _, name := range names {
			if listed >= maxListedInAlert*2 {
				break
			}
			fmt.Fprintf(&b, "- %s on %s\n", name, node)
			listed++
		}
	}
	if total > listed {
		fmt.Fprintf(&b, "... and %d more.\n", total-listed)
	}
	b.WriteString("Their logs are under Container logs (Developer and above).")
	_, err := s.alerts.Raise(ctx, alerts.RaiseParams{
		Kind: "stale_containers_removed", Severity: db.AlertSeverityInfo,
		Title:     fmt.Sprintf("Stale container cleanup removed %d %s", total, plural(total, "container", "containers")),
		Body:      clipRunes(b.String(), alerts.MaxBodyLen),
		Audiences: audiences(),
	})
	if err != nil {
		s.logger.Printf("stalecontainers: raise cleanup summary alert: %v", err)
	}
}

// syncAlerts brings the alerts in line with a scan. For every node that
// answered: one warning while it has stale containers (updated in place, and
// closed when there are none), one while it has running containers Sparky has
// no record of, and the per-container "kept" alerts are closed for containers
// that are no longer there. A node that did not answer is left alone: its
// containers are unknown, not gone. gone lists instances just removed, which
// no longer count. sum, if given, is tallied.
func (s *Service) syncAlerts(ctx context.Context, scans []NodeScan, gone map[string]bool, sum *RunSummary) {
	for i := range scans {
		ns := &scans[i]
		if ns.Status != ScanDone {
			sum.NodesSkipped++
			continue
		}
		sum.NodesScanned++

		var stale, orphans []Item
		present := make([]string, 0, len(ns.Items))
		for _, it := range ns.Items {
			if gone[it.InstanceID] {
				continue
			}
			present = append(present, keptKeyPrefix(ns.NodeID)+it.InstanceID)
			switch it.Verdict {
			case VerdictStale:
				stale = append(stale, it)
			case VerdictOrphanLive:
				orphans = append(orphans, it)
			case VerdictRecent:
				sum.Recent++
			}
		}
		sum.Stale += len(ns.With(VerdictStale))
		sum.Orphans += len(orphans)

		s.syncCondition(ctx, staleKey(ns.NodeID), len(stale) > 0, func() alerts.RaiseParams {
			return s.staleAlert(ns, stale)
		})
		s.syncCondition(ctx, orphanKey(ns.NodeID), len(orphans) > 0, func() alerts.RaiseParams {
			return s.orphanAlert(ns, orphans)
		})
		if _, err := s.alerts.ResolveConditionsExcept(ctx, keptKeyPrefix(ns.NodeID), present); err != nil {
			s.logger.Printf("stalecontainers: close kept alerts on %s: %v", ns.NodeName, err)
		}
	}
}

// syncCondition raises the alert while the condition holds and closes it when
// it does not.
func (s *Service) syncCondition(ctx context.Context, key string, holds bool, build func() alerts.RaiseParams) {
	if !holds {
		if _, err := s.alerts.ResolveCondition(ctx, key); err != nil {
			s.logger.Printf("stalecontainers: close alert %s: %v", key, err)
		}
		return
	}
	p := build()
	p.DedupeKey = key
	if _, err := s.alerts.Raise(ctx, p); err != nil {
		s.logger.Printf("stalecontainers: raise alert %s: %v", key, err)
	}
}

func (s *Service) staleAlert(ns *NodeScan, stale []Item) alerts.RaiseParams {
	var b strings.Builder
	fmt.Fprintf(&b, "%s holds %d %s for instances that have ended, left behind after the instance stopped or failed to launch. ",
		ns.NodeName, len(stale), plural(len(stale), "container", "containers"))
	b.WriteString("Open Container cleanup to review them and remove the ones you no longer need; each one's log is saved first.\n")
	for i, it := range stale {
		if i >= maxListedInAlert {
			fmt.Fprintf(&b, "... and %d more.\n", len(stale)-maxListedInAlert)
			break
		}
		fmt.Fprintf(&b, "- %s", it.ContainerName)
		if it.ProfileName != "" {
			fmt.Fprintf(&b, " (profile %s)", it.ProfileName)
		}
		fmt.Fprintf(&b, ": %s", it.State)
		if it.ExitCode != nil {
			fmt.Fprintf(&b, ", exit code %d", *it.ExitCode)
		}
		if it.OOMKilled {
			b.WriteString(", out of memory")
		}
		if !it.StoppedAt.IsZero() {
			fmt.Fprintf(&b, ", stopped %s ago", humanDuration(s.now().Sub(it.StoppedAt)))
		}
		b.WriteString("\n")
	}
	return alerts.RaiseParams{
		Kind: "stale_containers", Severity: db.AlertSeverityWarning,
		Title:      fmt.Sprintf("%d stale %s on %s", len(stale), plural(len(stale), "container", "containers"), ns.NodeName),
		Body:       clipRunes(b.String(), alerts.MaxBodyLen),
		ObjectType: alertObjectType, ObjectID: ns.NodeID, Audiences: audiences(),
	}
}

func (s *Service) orphanAlert(ns *NodeScan, orphans []Item) alerts.RaiseParams {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d running %s that Sparky started but has no record of (for example after the database was restored from a backup). ",
		ns.NodeName, len(orphans), plural(len(orphans), "container", "containers"))
	b.WriteString("They are running, so Sparky never removes them. Check them on the node and stop them yourself if they are not needed.\n")
	for i, it := range orphans {
		if i >= maxListedInAlert {
			fmt.Fprintf(&b, "... and %d more.\n", len(orphans)-maxListedInAlert)
			break
		}
		fmt.Fprintf(&b, "- %s (instance %s): %s\n", it.ContainerName, it.InstanceID, it.State)
	}
	return alerts.RaiseParams{
		Kind: "orphan_containers", Severity: db.AlertSeverityWarning,
		Title:      fmt.Sprintf("%d running %s on %s with no matching instance", len(orphans), plural(len(orphans), "container", "containers"), ns.NodeName),
		Body:       clipRunes(b.String(), alerts.MaxBodyLen),
		ObjectType: alertObjectType, ObjectID: ns.NodeID, Audiences: audiences(),
	}
}

// clipRunes shortens s to at most n runes, marking the cut.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
