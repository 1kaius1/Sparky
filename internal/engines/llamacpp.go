// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"fmt"
)

// llamaCPPAdapter is the Adapter for db.ProfileEngineLlamaCPP.
type llamaCPPAdapter struct{}

// RequiresFullGPUResidency is always false for llama.cpp-style engines -
// see SCHEMA.md Model profiles: partial GPU offload is the point.
func (llamaCPPAdapter) RequiresFullGPUResidency() bool { return false }

// llamaCPPParams are the llama.cpp server launch parameters Sparky
// recognizes - confirmed against a real `llama-server --help` (run via
// ghcr.io/ggml-org/llama.cpp:server, the same image
// agent/runtime/containers was verified against), not guessed:
// n_gpu_layers maps to --gpu-layers / --n-gpu-layers / -ngl, ctx_size
// maps to --ctx-size / -c, threads maps to --threads / -t. Any other
// key in engine_params is passed through unvalidated - see
// Adapter.ValidateParams.
type llamaCPPParams struct {
	NGPULayers *int `json:"n_gpu_layers,omitempty"`
	CtxSize    *int `json:"ctx_size,omitempty"`
	Threads    *int `json:"threads,omitempty"`
}

// ValidateParams checks the known llama.cpp keys, when present, for
// sane types and ranges.
func (llamaCPPAdapter) ValidateParams(params json.RawMessage) error {
	var p llamaCPPParams
	if err := unmarshalParamsObject(params, &p); err != nil {
		return err
	}

	if p.NGPULayers != nil && *p.NGPULayers < 0 {
		return fmt.Errorf("%w: n_gpu_layers must not be negative, got %d", ErrInvalidParams, *p.NGPULayers)
	}
	if p.CtxSize != nil && *p.CtxSize < 1 {
		return fmt.Errorf("%w: ctx_size must be positive, got %d", ErrInvalidParams, *p.CtxSize)
	}
	if p.Threads != nil && *p.Threads < 1 {
		return fmt.Errorf("%w: threads must be positive, got %d", ErrInvalidParams, *p.Threads)
	}
	return nil
}
