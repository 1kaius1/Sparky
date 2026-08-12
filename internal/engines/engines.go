// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engines is the pluggable engine adapter registry - see
// CLAUDE.md's repository layout ("engines/ - Pluggable adapters: vllm,
// aphrodite, llamacpp") and ARCHITECTURE.md's Extension and Integration
// Points ("adding another engine is implementing the adapter interface,
// not a schema change"). An adapter validates a profile's engine_params
// (SCHEMA.md Model profiles), reports whether its engine requires full
// GPU residency, and (as of the Running instances work) translates
// engine_params into an image and command-line arguments - see
// Adapter.BuildLaunchSpec. It does not resolve a model's local path or
// know anything about ports/volumes/GPU passthrough - that's
// agent-local, filesystem- and runtime-specific knowledge the central app
// does not have; see internal/lifecycle (the Model Lifecycle
// Orchestrator, ARCHITECTURE.md Component Breakdown) and
// agent/connection's load_instance dispatch, which combine this
// package's output with what only the agent knows.
package engines

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrInvalidParams is returned when engine_params fails an adapter's
// validation - wrapped with a specific reason, so callers can render it
// directly, same pattern as internal/nodes' ErrInvalidNode.
var ErrInvalidParams = errors.New("invalid engine params")

// ErrUnknownEngineType is returned by Registry.Adapter for a
// db.ProfileEngineType with no registered adapter - db.ProfileEngineAphrodite
// today, since it has no adapter until v0.3.0.
var ErrUnknownEngineType = errors.New("unknown engine type")

// LaunchSpec is an engine adapter's translation of engine_params into how
// to actually run the engine - the image to run and the command-line
// arguments derived from the recognized subset of engine_params. It
// deliberately excludes anything agent-local: no --model path (only the
// agent knows its own SPARKY_MODEL_STORAGE_PATH layout - see
// agent/connection's load_instance dispatch, which resolves ModelRef to
// a local path itself) and no --port/--host binding (Service fills that
// in from the profile's own Port field, the same value for every engine
// type, so there is nothing engine-specific about it).
type LaunchSpec struct {
	// Image is the container image to run this engine from.
	Image string

	// Args are additional command-line flags derived from the recognized
	// subset of engine_params - e.g. llama.cpp's --ctx-size, vLLM's
	// --tensor-parallel-size. Does not include --model or --port/--host;
	// see LaunchSpec's doc comment.
	Args []string
}

// Adapter validates a profile's engine-specific launch parameters,
// reports a fixed capability of its engine, and translates engine_params
// into a LaunchSpec.
type Adapter interface {
	// RequiresFullGPUResidency reports whether this engine type requires
	// the whole model to fit in GPU memory - see SCHEMA.md Model
	// profiles' requires_full_gpu_residency. A property of the engine
	// itself, not derived from a given profile's params.
	RequiresFullGPUResidency() bool

	// ValidateParams checks that params is well-formed for this engine.
	// Unknown keys are not rejected - engine_params is deliberately
	// opaque to the database and only partially meaningful to Sparky
	// itself (SCHEMA.md Model profiles); this checks the handful of keys
	// Sparky recognizes, when present, and otherwise passes the rest
	// through. Returns an error wrapping ErrInvalidParams if not.
	ValidateParams(params json.RawMessage) error

	// BuildLaunchSpec translates params into a LaunchSpec. Callers are
	// expected to have already called ValidateParams successfully -
	// BuildLaunchSpec does not re-validate ranges/types, only re-parses
	// the same recognized keys ValidateParams already checked.
	BuildLaunchSpec(params json.RawMessage) (LaunchSpec, error)
}

// Registry maps a db.ProfileEngineType to its Adapter.
type Registry struct {
	adapters map[db.ProfileEngineType]Adapter
}

// NewRegistry constructs a Registry with the adapters this package
// ships - vLLM and llama.cpp. db.ProfileEngineAphrodite has none yet.
func NewRegistry() *Registry {
	return &Registry{
		adapters: map[db.ProfileEngineType]Adapter{
			db.ProfileEngineVLLM:     vllmAdapter{},
			db.ProfileEngineLlamaCPP: llamaCPPAdapter{},
		},
	}
}

// Adapter returns the adapter for engineType, or an error wrapping
// ErrUnknownEngineType if none is registered.
func (r *Registry) Adapter(engineType db.ProfileEngineType) (Adapter, error) {
	a, ok := r.adapters[engineType]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEngineType, engineType)
	}
	return a, nil
}

// unmarshalParamsObject decodes params into dst - shared by both
// adapters. dst is always a pointer to a struct of named, known fields
// (never a map), so encoding/json itself rejects anything that isn't a
// JSON object (an array, a bare string, null, etc. all fail to
// unmarshal into a struct) and silently ignores any key dst doesn't
// declare - engine_params is documented as opaque beyond the handful of
// fields Sparky recognizes, so an unrecognized key is not an error.
func unmarshalParamsObject(params json.RawMessage, dst any) error {
	if len(params) == 0 {
		return fmt.Errorf("%w: engine_params must not be empty", ErrInvalidParams)
	}
	if err := json.Unmarshal(params, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidParams, err)
	}
	return nil
}
