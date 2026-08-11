// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestLlamaCPPAdapter_ValidateParams_Valid(t *testing.T) {
	tests := map[string]string{
		"empty object":                 `{}`,
		"all known fields":             `{"n_gpu_layers":20,"ctx_size":4096,"threads":8}`,
		"n_gpu_layers zero (CPU only)": `{"n_gpu_layers":0}`,
		"unknown fields pass through":  `{"rope_freq_base":10000,"threads":4}`,
	}
	a := llamaCPPAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if err := a.ValidateParams(json.RawMessage(params)); err != nil {
				t.Errorf("ValidateParams(%s) error: %v", params, err)
			}
		})
	}
}

func TestLlamaCPPAdapter_ValidateParams_Invalid(t *testing.T) {
	tests := map[string]string{
		"empty params":          ``,
		"not an object":         `"a string"`,
		"malformed JSON":        `{"n_gpu_layers":`,
		"n_gpu_layers negative": `{"n_gpu_layers":-1}`,
		"ctx_size zero":         `{"ctx_size":0}`,
		"threads zero":          `{"threads":0}`,
		"threads wrong type":    `{"threads":"eight"}`,
	}
	a := llamaCPPAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			err := a.ValidateParams(json.RawMessage(params))
			if !errors.Is(err, ErrInvalidParams) {
				t.Errorf("ValidateParams(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}
