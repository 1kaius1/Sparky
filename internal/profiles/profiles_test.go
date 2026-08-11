// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

func validFields() Fields {
	return Fields{
		Name:         "tiny-model",
		ModelRef:     "Qwen/Qwen2.5-0.5B-Instruct",
		EngineType:   db.ProfileEngineVLLM,
		EngineParams: json.RawMessage(`{}`),
		TargetNodeID: "node-1",
		Port:         8000,
	}
}

func TestFields_Validate_Valid(t *testing.T) {
	memory := 8.0
	tests := map[string]Fields{
		"minimal":              validFields(),
		"with required_memory": func() Fields { f := validFields(); f.RequiredMemoryGB = &memory; return f }(),
		"port at lower bound":  func() Fields { f := validFields(); f.Port = 1; return f }(),
		"port at upper bound":  func() Fields { f := validFields(); f.Port = 65535; return f }(),
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.validate(); err != nil {
				t.Errorf("validate() error = %v, want nil", err)
			}
		})
	}
}

func TestFields_Validate_Invalid(t *testing.T) {
	negativeMemory := -1.0
	zeroMemory := 0.0

	tests := map[string]Fields{
		"empty name":           func() Fields { f := validFields(); f.Name = ""; return f }(),
		"empty model_ref":      func() Fields { f := validFields(); f.ModelRef = ""; return f }(),
		"empty target_node_id": func() Fields { f := validFields(); f.TargetNodeID = ""; return f }(),
		"zero port":            func() Fields { f := validFields(); f.Port = 0; return f }(),
		"negative port":        func() Fields { f := validFields(); f.Port = -1; return f }(),
		"port too high":        func() Fields { f := validFields(); f.Port = 65536; return f }(),
		"negative required_memory_gb": func() Fields {
			f := validFields()
			f.RequiredMemoryGB = &negativeMemory
			return f
		}(),
		"zero required_memory_gb": func() Fields {
			f := validFields()
			f.RequiredMemoryGB = &zeroMemory
			return f
		}(),
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			err := f.validate()
			if !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("validate() error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}
