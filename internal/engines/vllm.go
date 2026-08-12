// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// vllmImage is the container image vLLM profiles launch - vllm/vllm-openai
// is vLLM's own official OpenAI-compatible server image, well-documented
// and stable, but - like vllmParams below - not confirmed against a live
// install in this environment: installing vLLM's CUDA/torch dependency
// chain wasn't practical here. An honest confidence gap, not glossed
// over, same as vllmParams' own doc comment already carries.
const vllmImage = "vllm/vllm-openai:latest"

// vllmAdapter is the Adapter for db.ProfileEngineVLLM.
type vllmAdapter struct{}

// RequiresFullGPUResidency is always true for vLLM - see SCHEMA.md Model
// profiles.
func (vllmAdapter) RequiresFullGPUResidency() bool { return true }

// vllmParams are the vLLM launch parameters Sparky recognizes -
// tensor_parallel_size, gpu_memory_utilization, dtype, quantization, and
// max_model_len are well-established, stable vLLM engine arguments that
// have been part of its OpenAI-compatible server CLI since its earliest
// releases. Unlike llamaCPPParams, these were not confirmed against a
// live vLLM install in this environment - installing vLLM's CUDA/torch
// dependency chain wasn't practical here - so this reflects
// well-documented, stable knowledge rather than a fresh empirical check.
// Any other key in engine_params is passed through unvalidated - see
// Adapter.ValidateParams.
type vllmParams struct {
	TensorParallelSize   *int     `json:"tensor_parallel_size,omitempty"`
	GPUMemoryUtilization *float64 `json:"gpu_memory_utilization,omitempty"`
	DType                *string  `json:"dtype,omitempty"`
	Quantization         *string  `json:"quantization,omitempty"`
	MaxModelLen          *int     `json:"max_model_len,omitempty"`
}

// ValidateParams checks the known vLLM keys, when present, for sane
// types and ranges.
func (vllmAdapter) ValidateParams(params json.RawMessage) error {
	var p vllmParams
	if err := unmarshalParamsObject(params, &p); err != nil {
		return err
	}

	if p.TensorParallelSize != nil && *p.TensorParallelSize < 1 {
		return fmt.Errorf("%w: tensor_parallel_size must be positive, got %d", ErrInvalidParams, *p.TensorParallelSize)
	}
	if p.GPUMemoryUtilization != nil && (*p.GPUMemoryUtilization <= 0 || *p.GPUMemoryUtilization > 1) {
		return fmt.Errorf("%w: gpu_memory_utilization must be in (0, 1], got %v", ErrInvalidParams, *p.GPUMemoryUtilization)
	}
	if p.MaxModelLen != nil && *p.MaxModelLen < 1 {
		return fmt.Errorf("%w: max_model_len must be positive, got %d", ErrInvalidParams, *p.MaxModelLen)
	}
	return nil
}

// BuildLaunchSpec translates the recognized vLLM keys into vllm serve
// flags - --tensor-parallel-size, --gpu-memory-utilization, --dtype,
// --quantization, --max-model-len. A key left unset in params is simply
// omitted, letting vLLM fall back to its own default rather than Sparky
// hardcoding one.
func (vllmAdapter) BuildLaunchSpec(params json.RawMessage) (LaunchSpec, error) {
	var p vllmParams
	if err := unmarshalParamsObject(params, &p); err != nil {
		return LaunchSpec{}, err
	}

	var args []string
	if p.TensorParallelSize != nil {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(*p.TensorParallelSize))
	}
	if p.GPUMemoryUtilization != nil {
		args = append(args, "--gpu-memory-utilization", strconv.FormatFloat(*p.GPUMemoryUtilization, 'f', -1, 64))
	}
	if p.DType != nil {
		args = append(args, "--dtype", *p.DType)
	}
	if p.Quantization != nil {
		args = append(args, "--quantization", *p.Quantization)
	}
	if p.MaxModelLen != nil {
		args = append(args, "--max-model-len", strconv.Itoa(*p.MaxModelLen))
	}
	return LaunchSpec{Image: vllmImage, Args: args}, nil
}
