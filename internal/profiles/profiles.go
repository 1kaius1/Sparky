// SPDX-License-Identifier: AGPL-3.0-or-later

// Package profiles is Model profile CRUD - see ARCHITECTURE.md Model
// Profile Management and SCHEMA.md Model profiles. It never accesses
// the database directly (see CLAUDE.md); the validation in this file is
// pure, and Service in service.go is the thin orchestration layer that
// persists a validated, permitted profile via internal/db, after
// checking engine_params through internal/engines and confirming the
// target node exists via internal/nodes.
package profiles

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrInvalidProfile is returned when profile params fail validation -
// wrapped with a specific reason, so callers can render it directly,
// same pattern as internal/nodes' ErrInvalidNode.
var ErrInvalidProfile = errors.New("invalid model profile")

// Fields are the mutable fields shared by CreateParams and UpdateParams.
// RequiresFullGPUResidency is deliberately absent - see SCHEMA.md Model
// profiles: it is a fixed capability of the selected engine type, not
// caller-supplied input, so Service derives it from the engine adapter
// rather than trusting a value that could disagree with EngineType.
type Fields struct {
	Name             string
	ModelRef         string
	EngineType       db.ProfileEngineType
	EngineParams     json.RawMessage
	RequiredMemoryGB *float64

	// EngineVersion optionally pins this profile's launch to a specific
	// installed engine binary version (see SCHEMA.md Node engine
	// inventory) instead of whatever the target node's `latest` symlink
	// currently points to - nil/empty means unpinned, today's unchanged
	// behavior. Deliberately not validated against node_engine_inventory
	// here or in Service - a bad pin fails clearly at launch time
	// instead, the same "attempt and report failure" philosophy
	// RequiredMemoryGB's own SCHEMA.md doc comment already states, and
	// avoids coupling a profile save to inventory state that can change
	// out from under it anyway.
	EngineVersion *string

	TargetNodeID string
	Port         int
}

// validate checks Fields' own shape - the things knowable without a
// database or an engine adapter (EngineParams' engine-specific validity
// and TargetNodeID's existence are Service's job, since they need
// internal/engines and internal/nodes respectively).
func (f Fields) validate() error {
	if f.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidProfile)
	}
	if f.ModelRef == "" {
		return fmt.Errorf("%w: model_ref is required", ErrInvalidProfile)
	}
	if f.TargetNodeID == "" {
		return fmt.Errorf("%w: target_node_id is required", ErrInvalidProfile)
	}
	if f.Port <= 0 || f.Port > 65535 {
		return fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidProfile)
	}
	if f.RequiredMemoryGB != nil && *f.RequiredMemoryGB <= 0 {
		return fmt.Errorf("%w: required_memory_gb must be positive", ErrInvalidProfile)
	}
	return nil
}

// CreateParams is the input to Service.CreateProfile.
type CreateParams struct {
	Fields
}

// UpdateParams is the input to Service.UpdateProfile - the same mutable
// fields as CreateParams, plus which profile to update. Topology is not
// updatable in v0.1.0 - see migrations/000007_create_model_profiles.up.sql.
type UpdateParams struct {
	ID string
	Fields
}
