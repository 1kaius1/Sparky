// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// vllmImage is the default container image vLLM profiles launch -
// vllm/vllm-openai is vLLM's own official OpenAI-compatible server
// image. Confirmed via a real `podman pull`/`podman inspect`
// (PLANNING.md Decisions Log) that its ENTRYPOINT is ["vllm","serve"] -
// which is why agent/connection's buildEngineLaunchArgs does not
// prepend "serve" for the containers backend, only bare-metal. Not
// necessarily the right image on real DGX Spark hardware - a real
// Spark launch (2026-08-19 Decisions Log) used NVIDIA's own NGC vLLM
// container instead, presumably for Grace-Blackwell ARM64 support the
// vanilla DockerHub image may not target; Model profiles' per-profile
// image override (SCHEMA.md, PR #79) is how an operator points a
// Spark-hosted profile at the right image. vllmParams below remains a
// separate question, partially but not fully confirmed - see its own
// doc comment.
const vllmImage = "vllm/vllm-openai:latest"

// vllmAdapter is the Adapter for db.ProfileEngineVLLM.
type vllmAdapter struct{}

// RequiresFullGPUResidency is always true for vLLM - see SCHEMA.md Model
// profiles.
func (vllmAdapter) RequiresFullGPUResidency() bool { return true }

// vllmParams are the vLLM launch parameters Sparky recognizes.
// tensor_parallel_size, gpu_memory_utilization, dtype, and quantization
// remain unconfirmed against a live vLLM install - installing vLLM's
// CUDA/torch dependency chain wasn't practical in this environment - so
// they still reflect well-documented, stable knowledge rather than a
// fresh empirical check. max_model_len and the seven fields below it
// (served_model_name through tool_call_parser) are, however, confirmed
// against two independent real production vLLM launches on real DGX
// Spark hardware (PLANNING.md's 2026-08-17 and 2026-08-19 Decisions Log
// entries) - each appeared verbatim as a non-default arg in vLLM's own
// startup log. Any other key in engine_params is rejected as an error -
// see Adapter.ValidateParams.
type vllmParams struct {
	TensorParallelSize   *int     `json:"tensor_parallel_size,omitempty"`
	GPUMemoryUtilization *float64 `json:"gpu_memory_utilization,omitempty"`
	DType                *string  `json:"dtype,omitempty"`
	Quantization         *string  `json:"quantization,omitempty"`
	MaxModelLen          *int     `json:"max_model_len,omitempty"`
	ServedModelName      *string  `json:"served_model_name,omitempty"`
	KVCacheDType         *string  `json:"kv_cache_dtype,omitempty"`
	MaxNumBatchedTokens  *int     `json:"max_num_batched_tokens,omitempty"`
	MaxNumSeqs           *int     `json:"max_num_seqs,omitempty"`
	EnableChunkedPrefill *bool    `json:"enable_chunked_prefill,omitempty"`
	EnableAutoToolChoice *bool    `json:"enable_auto_tool_choice,omitempty"`
	ToolCallParser       *string  `json:"tool_call_parser,omitempty"`
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
	if p.MaxNumBatchedTokens != nil && *p.MaxNumBatchedTokens < 1 {
		return fmt.Errorf("%w: max_num_batched_tokens must be positive, got %d", ErrInvalidParams, *p.MaxNumBatchedTokens)
	}
	if p.MaxNumSeqs != nil && *p.MaxNumSeqs < 1 {
		return fmt.Errorf("%w: max_num_seqs must be positive, got %d", ErrInvalidParams, *p.MaxNumSeqs)
	}
	// vLLM itself rejects --tool-call-parser given without
	// --enable-auto-tool-choice - catching it here gives the operator an
	// error at profile-save time instead of at launch.
	if p.ToolCallParser != nil && (p.EnableAutoToolChoice == nil || !*p.EnableAutoToolChoice) {
		return fmt.Errorf("%w: tool_call_parser requires enable_auto_tool_choice to be true", ErrInvalidParams)
	}
	return nil
}

// BuildLaunchSpec translates the recognized vLLM keys into vllm serve
// flags - --tensor-parallel-size, --gpu-memory-utilization, --dtype,
// --quantization, --max-model-len, --served-model-name,
// --kv-cache-dtype, --max-num-batched-tokens, --max-num-seqs,
// --enable-chunked-prefill, --enable-auto-tool-choice,
// --tool-call-parser. A key left unset in params is simply omitted,
// letting vLLM fall back to its own default rather than Sparky
// hardcoding one. The last three flags are presence-only (no value) -
// enable_chunked_prefill/enable_auto_tool_choice set to false are
// omitted the same as unset, since vLLM has no --no-enable-...
// counterpart and omission already yields the same disabled behavior.
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
	if p.ServedModelName != nil {
		args = append(args, "--served-model-name", *p.ServedModelName)
	}
	if p.KVCacheDType != nil {
		args = append(args, "--kv-cache-dtype", *p.KVCacheDType)
	}
	if p.MaxNumBatchedTokens != nil {
		args = append(args, "--max-num-batched-tokens", strconv.Itoa(*p.MaxNumBatchedTokens))
	}
	if p.MaxNumSeqs != nil {
		args = append(args, "--max-num-seqs", strconv.Itoa(*p.MaxNumSeqs))
	}
	if p.EnableChunkedPrefill != nil && *p.EnableChunkedPrefill {
		args = append(args, "--enable-chunked-prefill")
	}
	if p.EnableAutoToolChoice != nil && *p.EnableAutoToolChoice {
		args = append(args, "--enable-auto-tool-choice")
	}
	if p.ToolCallParser != nil {
		args = append(args, "--tool-call-parser", *p.ToolCallParser)
	}
	return LaunchSpec{Image: vllmImage, Args: args}, nil
}
