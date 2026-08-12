// SPDX-License-Identifier: AGPL-3.0-or-later

// Package transfers is Model transfers orchestration - see
// ARCHITECTURE.md and SCHEMA.md Model transfers / Node model inventory. It
// never accesses the database directly (see CLAUDE.md); the validation in
// this file is pure, and Service in service.go is the thin orchestration
// layer that persists a validated, permitted transfer via internal/db and
// dispatches it over internal/agentconn.
package transfers

import (
	"errors"
	"fmt"
)

// ErrInvalidTransfer is returned when InitiateTransferParams fails
// validation - wrapped with a specific reason, same pattern as
// internal/nodes' ErrInvalidNode.
var ErrInvalidTransfer = errors.New("invalid model transfer")

// ErrDestNodeOffline is returned when the destination node has no live
// agent connection - see internal/agentconn.Registry.Connected. Checked
// before a model_transfers row is even created, so an unreachable node
// never leaves behind a queued transfer nothing will ever pick up.
var ErrDestNodeOffline = errors.New("destination node is not connected")

// InitiateTransferParams is the input to Service.InitiateTransfer. v0.1.0
// only initiates internet-sourced (Hugging Face) downloads - see
// PLANNING.md's Model transfers milestone item ("no peer replication yet")
// - so Service always constructs a db.TransferSourceInternet row; there is
// no source-node field to set here yet.
type InitiateTransferParams struct {
	DestNodeID string
	ModelRef   string
}

// validate checks InitiateTransferParams' own shape - the things knowable
// without a database or the agent registry (DestNodeID's connectivity is
// Service's job, since it needs internal/agentconn).
func (p InitiateTransferParams) validate() error {
	if p.DestNodeID == "" {
		return fmt.Errorf("%w: dest_node_id is required", ErrInvalidTransfer)
	}
	if p.ModelRef == "" {
		return fmt.Errorf("%w: model_ref is required", ErrInvalidTransfer)
	}
	return nil
}
