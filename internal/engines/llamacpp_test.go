// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLlamaCPPAdapter_ValidateParams_Valid(t *testing.T) {
	tests := map[string]string{
		"empty object":                 `{}`,
		"all known fields":             `{"n_gpu_layers":20,"ctx_size":4096,"threads":8}`,
		"n_gpu_layers zero (CPU only)": `{"n_gpu_layers":0}`,
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
		"unknown key":           `{"rope_freq_base":10000,"threads":4}`,
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

func TestLlamaCPPAdapter_ValidateParams_UnknownKey_ErrorNamesTheKey(t *testing.T) {
	a := llamaCPPAdapter{}
	err := a.ValidateParams(json.RawMessage(`{"rope_freq_base":10000}`))
	if err == nil || !strings.Contains(err.Error(), "rope_freq_base") {
		t.Errorf("ValidateParams() error = %v, want it to name the unrecognized key %q", err, "rope_freq_base")
	}
}

func TestLlamaCPPAdapter_BuildLaunchSpec(t *testing.T) {
	tests := map[string]struct {
		params string
		want   []string
	}{
		"empty object, no flags":       {`{}`, nil},
		"all known fields":             {`{"n_gpu_layers":20,"ctx_size":4096,"threads":8}`, []string{"--gpu-layers", "20", "--ctx-size", "4096", "--threads", "8"}},
		"n_gpu_layers zero (CPU only)": {`{"n_gpu_layers":0}`, []string{"--gpu-layers", "0"}},
	}
	a := llamaCPPAdapter{}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec, err := a.BuildLaunchSpec(json.RawMessage(tt.params))
			if err != nil {
				t.Fatalf("BuildLaunchSpec(%s) error: %v", tt.params, err)
			}
			if spec.Image != llamaCPPImage {
				t.Errorf("Image = %q, want %q", spec.Image, llamaCPPImage)
			}
			if !reflect.DeepEqual(spec.Args, tt.want) {
				t.Errorf("Args = %v, want %v", spec.Args, tt.want)
			}
		})
	}
}

func TestLlamaCPPAdapter_BuildLaunchSpec_InvalidParams(t *testing.T) {
	tests := map[string]string{
		"empty params": ``,
		"unknown key":  `{"rope_freq_base":10000,"threads":4}`,
	}
	a := llamaCPPAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := a.BuildLaunchSpec(json.RawMessage(params)); !errors.Is(err, ErrInvalidParams) {
				t.Errorf("BuildLaunchSpec(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}
