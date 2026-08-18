// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestVLLMAdapter_ValidateParams_Valid(t *testing.T) {
	tests := map[string]string{
		"empty object":                          `{}`,
		"all known fields":                      `{"tensor_parallel_size":2,"gpu_memory_utilization":0.9,"dtype":"bfloat16","quantization":"awq","max_model_len":4096}`,
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
		"unknown key":                     `{"unknown_flag":"whatever","tensor_parallel_size":1}`,
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

func TestVLLMAdapter_ValidateParams_UnknownKey_ErrorNamesTheKey(t *testing.T) {
	a := vllmAdapter{}
	err := a.ValidateParams(json.RawMessage(`{"unknown_flag":"whatever"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown_flag") {
		t.Errorf("ValidateParams() error = %v, want it to name the unrecognized key %q", err, "unknown_flag")
	}
}

func TestVLLMAdapter_BuildLaunchSpec(t *testing.T) {
	tests := map[string]struct {
		params string
		want   []string
	}{
		"empty object, no flags": {`{}`, nil},
		"all known fields": {
			`{"tensor_parallel_size":2,"gpu_memory_utilization":0.9,"dtype":"bfloat16","quantization":"awq","max_model_len":4096}`,
			[]string{"--tensor-parallel-size", "2", "--gpu-memory-utilization", "0.9", "--dtype", "bfloat16", "--quantization", "awq", "--max-model-len", "4096"},
		},
	}
	a := vllmAdapter{}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec, err := a.BuildLaunchSpec(json.RawMessage(tt.params))
			if err != nil {
				t.Fatalf("BuildLaunchSpec(%s) error: %v", tt.params, err)
			}
			if spec.Image != vllmImage {
				t.Errorf("Image = %q, want %q", spec.Image, vllmImage)
			}
			if !reflect.DeepEqual(spec.Args, tt.want) {
				t.Errorf("Args = %v, want %v", spec.Args, tt.want)
			}
		})
	}
}

func TestVLLMAdapter_BuildLaunchSpec_InvalidParams(t *testing.T) {
	tests := map[string]string{
		"empty params": ``,
		"unknown key":  `{"unknown_flag":"whatever","tensor_parallel_size":1}`,
	}
	a := vllmAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := a.BuildLaunchSpec(json.RawMessage(params)); !errors.Is(err, ErrInvalidParams) {
				t.Errorf("BuildLaunchSpec(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}
