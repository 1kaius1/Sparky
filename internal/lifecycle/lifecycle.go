// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lifecycle is the Model Lifecycle Orchestrator - see
// ARCHITECTURE.md Component Breakdown ("Owns \"load\" and \"unload\"...
// Translates the result into commands sent to the relevant agent(s) via
// the agent-communication layer, and writes the outcome to Running
// instances") and CLAUDE.md's repository layout ("lifecycle/ - Load/
// unload orchestration, Green/Blue/Red eligibility, reduced-capacity
// flow"). This package implements single-node load/unload only, per
// PLANNING.md's v0.1.0 "Running instances: single-node load/unload"
// milestone item - Green/Blue/Red launch eligibility, reduced-capacity
// launches, and Running instance nodes (actual multi-node topology) are
// all v0.3.0 clustering scope and do not exist here.
package lifecycle

import (
	"errors"
	"fmt"
)

// ErrInvalidLoad is returned when LoadParams fails validation, or when a
// profile is otherwise not in a loadable state - wrapped with a specific
// reason, same pattern as internal/nodes' ErrInvalidNode.
var ErrInvalidLoad = errors.New("invalid load request")

// ErrAlreadyRunning is returned by LoadInstance when the target profile
// already has a non-terminal running instance (starting, running, or
// stopping) - see db.RunningInstanceRepository.FindActiveByProfileID.
var ErrAlreadyRunning = errors.New("profile already has an active running instance")

// ErrTargetNodeOffline is returned when the node a load or unload would
// target has no live agent connection - see internal/agentconn.Registry.
var ErrTargetNodeOffline = errors.New("target node is not connected")

// ErrInstanceNotRunning is returned by UnloadInstance when the instance
// is not currently in RunningInstanceStatusRunning - v0.1.0 only supports
// unloading a fully running instance, not canceling one still starting or
// stopping a second time.
var ErrInstanceNotRunning = errors.New("running instance is not in a running state")

// LoadParams is the input to Service.LoadInstance.
type LoadParams struct {
	ProfileID string
}

// validate checks LoadParams' own shape - the things knowable without a
// database (ProfileID's existence, and the profile's own loadability, are
// Service's job).
func (p LoadParams) validate() error {
	if p.ProfileID == "" {
		return fmt.Errorf("%w: profile_id is required", ErrInvalidLoad)
	}
	return nil
}
