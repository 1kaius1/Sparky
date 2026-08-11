// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"errors"
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
