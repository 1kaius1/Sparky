// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

func TestRegistry_Adapter_VLLM(t *testing.T) {
	r := NewRegistry()
	a, err := r.Adapter(db.ProfileEngineVLLM)
	if err != nil {
		t.Fatalf("Adapter() error: %v", err)
	}
	if !a.RequiresFullGPUResidency() {
		t.Error("vLLM RequiresFullGPUResidency() = false, want true")
	}
}

func TestRegistry_Adapter_LlamaCPP(t *testing.T) {
	r := NewRegistry()
	a, err := r.Adapter(db.ProfileEngineLlamaCPP)
	if err != nil {
		t.Fatalf("Adapter() error: %v", err)
	}
	if a.RequiresFullGPUResidency() {
		t.Error("llama.cpp RequiresFullGPUResidency() = true, want false")
	}
}

func TestRegistry_Adapter_UnknownEngineType(t *testing.T) {
	r := NewRegistry()

	// Aphrodite has no adapter until v0.3.0 - see PLANNING.md's Model
	// profiles phase breakdown.
	_, err := r.Adapter(db.ProfileEngineAphrodite)
	if !errors.Is(err, ErrUnknownEngineType) {
		t.Errorf("Adapter() error = %v, want ErrUnknownEngineType", err)
	}
}

// testParams is a minimal named-field struct for exercising
// unmarshalParamsObject directly, independent of either adapter's own
// params shape.
type testParams struct {
	Known *string `json:"known,omitempty"`
}

func TestUnmarshalParamsObject_UnknownField_Rejected(t *testing.T) {
	var dst testParams
	err := unmarshalParamsObject(json.RawMessage(`{"known":"x","bogus_field":1}`), &dst)
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("unmarshalParamsObject() error = %v, want ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "bogus_field") {
		t.Errorf("unmarshalParamsObject() error = %v, want it to name the unrecognized key %q", err, "bogus_field")
	}
}

func TestUnmarshalParamsObject_KnownFieldsOnly_Succeeds(t *testing.T) {
	var dst testParams
	if err := unmarshalParamsObject(json.RawMessage(`{"known":"x"}`), &dst); err != nil {
		t.Fatalf("unmarshalParamsObject() error: %v", err)
	}
	if dst.Known == nil || *dst.Known != "x" {
		t.Errorf("Known = %v, want %q", dst.Known, "x")
	}
}
