// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestVLLMAdapter_ValidateParams_Valid(t *testing.T) {
	tests := map[string]string{
		"empty object":                          `{}`,
		"all known fields":                      `{"tensor_parallel_size":2,"gpu_memory_utilization":0.9,"dtype":"bfloat16","quantization":"awq","max_model_len":4096}`,
		"unknown fields pass through":           `{"unknown_flag":"whatever","tensor_parallel_size":1}`,
		"gpu_memory_utilization at upper bound": `{"gpu_memory_utilization":1}`,
	}
	a := vllmAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if err := a.ValidateParams(json.RawMessage(params)); err != nil {
				t.Errorf("ValidateParams(%s) error: %v", params, err)
			}
		})
	}
}

func TestVLLMAdapter_ValidateParams_Invalid(t *testing.T) {
	tests := map[string]string{
		"empty params":                    ``,
		"not an object":                   `[1,2,3]`,
		"malformed JSON":                  `{`,
		"tensor_parallel_size zero":       `{"tensor_parallel_size":0}`,
		"tensor_parallel_size negative":   `{"tensor_parallel_size":-1}`,
		"gpu_memory_utilization zero":     `{"gpu_memory_utilization":0}`,
		"gpu_memory_utilization too high": `{"gpu_memory_utilization":1.5}`,
		"max_model_len zero":              `{"max_model_len":0}`,
		"tensor_parallel_size wrong type": `{"tensor_parallel_size":"four"}`,
	}
	a := vllmAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			err := a.ValidateParams(json.RawMessage(params))
			if !errors.Is(err, ErrInvalidParams) {
				t.Errorf("ValidateParams(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}
