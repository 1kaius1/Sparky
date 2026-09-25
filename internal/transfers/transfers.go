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

	"github.com/1kaius1/Sparky/internal/db"
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

// ErrNotRetryable is returned by RetryTransfer for a transfer that has not
// failed - only a failed transfer can be retried.
var ErrNotRetryable = errors.New("only a failed transfer can be retried")

// ErrSourceNodeOffline is ErrDestNodeOffline's counterpart for a peer
// transfer's source - both ends must be connected, since the source has to
// authorize the pull before it can start.
var ErrSourceNodeOffline = errors.New("source node is not connected")

// ErrSourceNotPresent is returned when the source node has no present
// inventory entry for the requested model/quantization/format.
var ErrSourceNotPresent = errors.New("source node does not have that model")

// ErrPeerNotReady is returned, wrapped with a specific reason, when a peer
// transfer's prerequisites are missing (a node with no SSH identity
// reported yet, no usable interface, ...).
var ErrPeerNotReady = errors.New("peer transfer prerequisites not met")

// InitiateTransferParams is the input to Service.InitiateTransfer. An
// empty SourceType means db.TransferSourceInternet (a Hugging Face
// download, the original behavior); db.TransferSourcePeerNode pulls a copy
// another node already has, over rsync-over-SSH.
type InitiateTransferParams struct {
	DestNodeID string
	ModelRef   string

	// SourceType selects internet (empty/default) or peer_node. The three
	// fields below apply only to peer_node.
	SourceType   db.TransferSourceType
	SourceNodeID string
	// Format is required for peer_node - with Quantization it identifies
	// exactly which of the source's inventory entries is being copied.
	Format db.ModelFormat
	// SourceInterface optionally overrides which of the source node's
	// reported interfaces is pulled from; empty means the node's default,
	// else "Fastest" - see nodes.Service.ResolveTransferSource.
	SourceInterface string

	// RetryOf is the ID of the failed transfer this one re-runs, recorded in
	// the audit detail only - it has no effect on how the transfer runs.
	RetryOf string

	// Quantization restricts the download to just the one .gguf file
	// matching this value, instead of every file in the repo - empty
	// means "download the whole repo" (today's unchanged behavior,
	// correct for vLLM/Aphrodite and a single-file GGUF repo). See
	// agent/transfer.Executor.Download for the matching logic.
	Quantization string
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
	switch p.SourceType {
	case "", db.TransferSourceInternet:
	case db.TransferSourcePeerNode:
		if p.SourceNodeID == "" {
			return fmt.Errorf("%w: source_node_id is required for a peer transfer", ErrInvalidTransfer)
		}
		if p.SourceNodeID == p.DestNodeID {
			return fmt.Errorf("%w: source and destination must be different nodes", ErrInvalidTransfer)
		}
		if p.Format != db.ModelFormatSafetensors && p.Format != db.ModelFormatGGUF {
			return fmt.Errorf("%w: format must be safetensors or gguf for a peer transfer", ErrInvalidTransfer)
		}
	default:
		return fmt.Errorf("%w: unknown source_type %q", ErrInvalidTransfer, p.SourceType)
	}
	return nil
}
