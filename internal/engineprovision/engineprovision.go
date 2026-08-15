// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engineprovision is Engine transfers orchestration - see
// ARCHITECTURE.md and SCHEMA.md Engine transfers / Node engine inventory.
// It never accesses the database directly (see CLAUDE.md); the validation
// in this file is pure, and Service in service.go is the thin orchestration
// layer that persists a validated, permitted provisioning run via
// internal/db and dispatches it over internal/agentconn - the same shape as
// internal/transfers, kept as a separate package rather than folded into it
// because the RBAC rule, domain object, and destination shape (a versioned
// install directory on the node, not a downloaded file tree) all differ -
// see PLANNING.md's 2026-08-15 Decisions Log entry.
package engineprovision

import (
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrInvalidProvisionRequest is returned when ProvisionEngineParams fails
// validation - wrapped with a specific reason, same pattern as
// internal/transfers' ErrInvalidTransfer.
var ErrInvalidProvisionRequest = errors.New("invalid engine provisioning request")

// ErrDestNodeOffline is returned when the destination node has no live
// agent connection - see internal/agentconn.Registry.Connected. Checked
// before an engine_transfers row is even created, so an unreachable node
// never leaves behind a queued transfer nothing will ever pick up - same
// reasoning as internal/transfers' identically-named error.
var ErrDestNodeOffline = errors.New("destination node is not connected")

// ProvisionEngineParams is the input to Service.ProvisionEngine.
type ProvisionEngineParams struct {
	DestNodeID string
	EngineType db.ProfileEngineType
	Version    string
}

// validate checks ProvisionEngineParams' own shape - the things knowable
// without a database or the agent registry (DestNodeID's connectivity is
// Service's job, since it needs internal/agentconn).
func (p ProvisionEngineParams) validate() error {
	if p.DestNodeID == "" {
		return fmt.Errorf("%w: dest_node_id is required", ErrInvalidProvisionRequest)
	}
	if p.EngineType == "" {
		return fmt.Errorf("%w: engine_type is required", ErrInvalidProvisionRequest)
	}
	if p.Version == "" {
		return fmt.Errorf("%w: version is required", ErrInvalidProvisionRequest)
	}
	return nil
}
