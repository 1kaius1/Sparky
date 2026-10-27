// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// staleCleanupReason is the reason an archived log carries when it was saved by
// stale container cleanup, as opposed to an unload, a failed launch or a
// replaced container.
const staleCleanupReason = "stale_cleanup"

// maxDetailLen bounds the free text an answer carries (a runtime error), so a
// verbose one can never push a reply past the WebSocket message limit.
const maxDetailLen = 1000

// instanceIDPattern is what an instance id looks like on the wire: a uuid, the
// same shape the runtime backend requires of a container it calls Sparky's.
// Checked first so garbage never gets as far as looking for a container.
var instanceIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// runListContainers answers a list_containers command with a description of
// every container this node's runtime holds that Sparky started. It is
// read-only. A runtime that cannot be listed is still answered (Error set) so
// the central app can say why instead of waiting for a reply that never comes.
func (c *Conn) runListContainers(ctx context.Context, conn *websocket.Conn, req agentproto.ListContainers) {
	containers, truncated, err := c.runtime.ListManaged(ctx)
	if err != nil {
		c.logger.Printf("agent connection: list containers %s: %v", req.ScanID, err)
		c.send(ctx, conn, agentproto.TypeListContainersResult, "", agentproto.ListContainersResult{ScanID: req.ScanID, Final: true, Error: clip(err.Error(), maxDetailLen)})
		return
	}
	// A series of parts, because a long list does not fit one message (see
	// agentproto.ListContainersChunkSize). An empty list is one empty part.
	for part, off := 0, 0; ; part++ {
		end := min(off+agentproto.ListContainersChunkSize, len(containers))
		res := agentproto.ListContainersResult{ScanID: req.ScanID, Part: part, Final: end == len(containers)}
		for _, mc := range containers[off:end] {
			res.Containers = append(res.Containers, wireContainer(mc))
		}
		if res.Final {
			res.Truncated = truncated
		}
		c.send(ctx, conn, agentproto.TypeListContainersResult, "", res)
		if res.Final {
			return
		}
		off = end
	}
}

// wireContainer converts a runtime description to its wire form, leaving a
// time the runtime did not know absent rather than the zero time.
func wireContainer(mc runtime.ManagedContainer) agentproto.ManagedContainer {
	out := agentproto.ManagedContainer{
		InstanceID:    mc.InstanceID,
		ProfileID:     mc.ProfileID,
		ContainerID:   wireText(mc.ContainerID, agentproto.MaxContainerIDLen),
		ContainerName: wireText(mc.ContainerName, agentproto.MaxContainerNameLen),
		State:         wireText(mc.State, 32),
		Labelled:      mc.Labelled,
		ExitCode:      mc.ExitCode,
		OOMKilled:     mc.OOMKilled,
	}
	if !mc.CreatedAt.IsZero() {
		t := mc.CreatedAt
		out.CreatedAt = &t
	}
	if !mc.FinishedAt.IsZero() {
		t := mc.FinishedAt
		out.FinishedAt = &t
	}
	return out
}

// wireText prepares a runtime-reported string for a list reply: anything
// outside printable ASCII becomes '?', then it is clipped to n bytes. A real
// container name or id is plain ASCII already; this exists so the size of a
// reply has a hard bound whatever a runtime reports, since JSON escaping can
// grow a control character sixfold (see ListContainersChunkSize).
func wireText(s string, n int) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return clip(string(out), n)
}

// clip shortens s to at most n bytes without splitting a UTF-8 character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// runRemoveContainer handles a remove_container command and always answers it
// with remove_container_result. The central app has already decided the
// container is stale; this re-checks the rules that keep a live instance safe
// and does nothing at all unless they hold (see agentproto.RemoveContainer).
// The log is archived first, and the container is removed only on a confirmed
// store - the same rule every other removal follows.
func (c *Conn) runRemoveContainer(ctx context.Context, conn *websocket.Conn, req agentproto.RemoveContainer) {
	outcome, detail := c.removeStale(ctx, conn, req)
	if outcome != agentproto.RemoveOutcomeRemoved && outcome != agentproto.RemoveOutcomeGone {
		c.logger.Printf("agent connection: remove container for instance %s: %s: %s", req.InstanceID, outcome, detail)
	}
	c.send(ctx, conn, agentproto.TypeRemoveContainerResult, "", agentproto.RemoveContainerResult{
		RemoveID:   req.RemoveID,
		InstanceID: req.InstanceID,
		Outcome:    outcome,
		Detail:     clip(detail, maxDetailLen),
	})
}

func (c *Conn) removeStale(ctx context.Context, conn *websocket.Conn, req agentproto.RemoveContainer) (outcome, detail string) {
	if !instanceIDPattern.MatchString(req.InstanceID) {
		return agentproto.RemoveOutcomeRefused, "not a valid instance id"
	}
	if req.Lines < 0 {
		return agentproto.RemoveOutcomeRefused, "the number of log lines cannot be negative"
	}

	if c.isActiveInstance(req.InstanceID) {
		return agentproto.RemoveOutcomeRefused, "this agent is running that instance"
	}

	listed, _, err := c.runtime.ListManaged(ctx)
	if err != nil {
		return agentproto.RemoveOutcomeFailed, fmt.Sprintf("could not list containers: %v", err)
	}
	var found *runtime.ManagedContainer
	for i := range listed {
		if listed[i].InstanceID == req.InstanceID {
			found = &listed[i]
			break
		}
	}
	if found == nil {
		return agentproto.RemoveOutcomeGone, "no such container"
	}
	switch found.State {
	case "exited":
	case "dead", "created":
		if !found.Labelled {
			return agentproto.RemoveOutcomeRefused, fmt.Sprintf("an unlabelled container is removed only once it has exited, and this one is %s", found.State)
		}
	default:
		return agentproto.RemoveOutcomeRefused, fmt.Sprintf("the container is %s; only stopped containers are removed", found.State)
	}

	switch c.archiveInstanceLines(ctx, conn, req.InstanceID, staleCleanupReason, req.Lines) {
	case archiveStored:
		// Not forced: a container that started after the check above is
		// refused by the runtime, not removed.
		if err := c.runtime.Remove(ctx, req.InstanceID); err != nil {
			return agentproto.RemoveOutcomeFailed, fmt.Sprintf("the log was saved but removing the container failed: %v", err)
		}
		return agentproto.RemoveOutcomeRemoved, ""
	case archiveNothing:
		return agentproto.RemoveOutcomeGone, "the container disappeared before its log could be read"
	case archiveBusy:
		return agentproto.RemoveOutcomeFailed, "another archive of this instance is in progress"
	default:
		return agentproto.RemoveOutcomeKept, "the log could not be archived, so the container was left in place"
	}
}
