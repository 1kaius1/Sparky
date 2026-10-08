// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"encoding/json"
	"errors"
	"strings"
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
		Format:       db.ModelFormatSafetensors,
		Port:         8000,
	}
}

func TestFields_Validate_Valid(t *testing.T) {
	memory := 8.0
	tests := map[string]Fields{
		"minimal":              validFields(),
		"slash and colon name": func() Fields { f := validFields(); f.Name = "team/qwen2.5-coder:7b"; return f }(),
		"name at 64 chars":     func() Fields { f := validFields(); f.Name = strings.Repeat("a", 64); return f }(),
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
		"name with spaces":     func() Fields { f := validFields(); f.Name = "my model"; return f }(),
		"name with comma":      func() Fields { f := validFields(); f.Name = "a,b"; return f }(),
		"name leading dash":    func() Fields { f := validFields(); f.Name = "-fast"; return f }(),
		"name too long":        func() Fields { f := validFields(); f.Name = strings.Repeat("a", 65); return f }(),
		"name untrimmed":       func() Fields { f := validFields(); f.Name = " tiny-model"; return f }(),
		"empty model_ref":      func() Fields { f := validFields(); f.ModelRef = ""; return f }(),
		"empty target_node_id": func() Fields { f := validFields(); f.TargetNodeID = ""; return f }(),
		"zero port":            func() Fields { f := validFields(); f.Port = 0; return f }(),
		"negative port":        func() Fields { f := validFields(); f.Port = -1; return f }(),
		"port too high":        func() Fields { f := validFields(); f.Port = 65536; return f }(),
		"empty format":         func() Fields { f := validFields(); f.Format = ""; return f }(),
		"unknown format":       func() Fields { f := validFields(); f.Format = db.ModelFormat("bogus"); return f }(),
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
