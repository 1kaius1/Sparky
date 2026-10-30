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
		served string // the profile name passed as the served model name
		want   []string
	}{
		"empty object, no flags":               {`{}`, "", nil},
		"all known fields":                     {`{"n_gpu_layers":20,"ctx_size":4096,"threads":8}`, "", []string{"--gpu-layers", "20", "--ctx-size", "4096", "--threads", "8"}},
		"n_gpu_layers zero (CPU only)":         {`{"n_gpu_layers":0}`, "", []string{"--gpu-layers", "0"}},
		"alias is the profile name":            {`{"ctx_size":4096}`, "my-profile", []string{"--ctx-size", "4096", "--alias", "my-profile"}},
		"alias is passed through as one entry": {`{}`, "Qwen3 8B (Q4)", []string{"--alias", "Qwen3 8B (Q4)"}},
	}
	a := llamaCPPAdapter{}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			spec, err := a.BuildLaunchSpec(json.RawMessage(tt.params), tt.served)
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
			if _, err := a.BuildLaunchSpec(json.RawMessage(params), "my-profile"); !errors.Is(err, ErrInvalidParams) {
				t.Errorf("BuildLaunchSpec(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}

func TestLlamaCPPAdapter_ExtraArgs(t *testing.T) {
	a := llamaCPPAdapter{}
	params := json.RawMessage(`{"ctx_size":8192,"extra_args":["--jinja","--flash-attn","on","--cache-type-k=q8_0"]}`)
	if err := a.ValidateParams(params); err != nil {
		t.Fatalf("ValidateParams() error: %v", err)
	}
	spec, err := a.BuildLaunchSpec(params, "my-profile")
	if err != nil {
		t.Fatalf("BuildLaunchSpec() error: %v", err)
	}
	want := []string{"--ctx-size", "8192", "--alias", "my-profile", "--jinja", "--flash-attn", "on", "--cache-type-k=q8_0"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v (extra_args last, verbatim)", spec.Args, want)
	}
}

func TestLlamaCPPAdapter_ExtraArgs_Rejected(t *testing.T) {
	tests := map[string]string{
		"empty entry": `{"extra_args":[""]}`,
		"model":       `{"extra_args":["--model","/x"]}`,
		"short model": `{"extra_args":["-m","/x"]}`,
		"hf repo":     `{"extra_args":["-hf","org/repo"]}`,
		"port":        `{"extra_args":["--port=9000"]}`,
		"host":        `{"extra_args":["--host","0.0.0.0"]}`,
		"alias":       `{"extra_args":["--alias","x"]}`,
		"short alias": `{"extra_args":["-a","x"]}`,
	}
	a := llamaCPPAdapter{}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if err := a.ValidateParams(json.RawMessage(params)); !errors.Is(err, ErrInvalidParams) {
				t.Errorf("ValidateParams(%s) error = %v, want ErrInvalidParams", params, err)
			}
		})
	}
}
