// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

// These tests cover the two pure functions the engine_version field touches
// directly: fieldsFromForm (submitted form -> profiles.Fields) and
// profileFormValuesFromProfile (a persisted profile -> form values, for
// the edit form's prefill). The handlers themselves
// (handleCreateProfile/handleUpdateProfile/handleNewProfileForm/
// handleEditProfileForm) have their own coverage in dashboard_test.go,
// alongside every other Dashboard UI page's handler tests.

func validProfileForm() profileFormValues {
	return profileFormValues{
		Name: "test-profile", ModelRef: "test-org/test-model", EngineType: "llamacpp",
		TargetNodeID: "node-1", Port: "8000",
	}
}

func TestFieldsFromForm_EngineVersion_Set(t *testing.T) {
	form := validProfileForm()
	form.EngineVersion = "b4610"

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.EngineVersion == nil || *fields.EngineVersion != "b4610" {
		t.Errorf("EngineVersion = %v, want %q", fields.EngineVersion, "b4610")
	}
}

func TestFieldsFromForm_EngineVersion_BlankIsNil(t *testing.T) {
	form := validProfileForm()
	form.EngineVersion = ""

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.EngineVersion != nil {
		t.Errorf("EngineVersion = %v, want nil for a blank field", *fields.EngineVersion)
	}
}

func TestFieldsFromForm_EngineVersion_WhitespaceOnlyIsNil(t *testing.T) {
	form := validProfileForm()
	form.EngineVersion = "   "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.EngineVersion != nil {
		t.Errorf("EngineVersion = %v, want nil for a whitespace-only field", *fields.EngineVersion)
	}
}

func TestFieldsFromForm_EngineVersion_Trimmed(t *testing.T) {
	form := validProfileForm()
	form.EngineVersion = "  b4610  "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.EngineVersion == nil || *fields.EngineVersion != "b4610" {
		t.Errorf("EngineVersion = %v, want trimmed %q", fields.EngineVersion, "b4610")
	}
}

func TestProfileFormValuesFromProfile_EngineVersion_Set(t *testing.T) {
	version := "b4610"
	p := &db.Profile{EngineVersion: &version, Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.EngineVersion != version {
		t.Errorf("EngineVersion = %q, want %q", form.EngineVersion, version)
	}
}

func TestProfileFormValuesFromProfile_EngineVersion_NilIsBlank(t *testing.T) {
	p := &db.Profile{Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.EngineVersion != "" {
		t.Errorf("EngineVersion = %q, want empty for an unpinned profile", form.EngineVersion)
	}
}

func TestFieldsFromForm_Quantization_Set(t *testing.T) {
	form := validProfileForm()
	form.Quantization = "Q4_K_M"

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Quantization == nil || *fields.Quantization != "Q4_K_M" {
		t.Errorf("Quantization = %v, want %q", fields.Quantization, "Q4_K_M")
	}
}

func TestFieldsFromForm_Quantization_BlankIsNil(t *testing.T) {
	form := validProfileForm()
	form.Quantization = ""

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Quantization != nil {
		t.Errorf("Quantization = %v, want nil for a blank field", *fields.Quantization)
	}
}

func TestFieldsFromForm_Quantization_WhitespaceOnlyIsNil(t *testing.T) {
	form := validProfileForm()
	form.Quantization = "   "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Quantization != nil {
		t.Errorf("Quantization = %v, want nil for a whitespace-only field", *fields.Quantization)
	}
}

func TestFieldsFromForm_Quantization_Trimmed(t *testing.T) {
	form := validProfileForm()
	form.Quantization = "  Q4_K_M  "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Quantization == nil || *fields.Quantization != "Q4_K_M" {
		t.Errorf("Quantization = %v, want trimmed %q", fields.Quantization, "Q4_K_M")
	}
}

func TestProfileFormValuesFromProfile_Quantization_Set(t *testing.T) {
	quant := "Q4_K_M"
	p := &db.Profile{Quantization: &quant, Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.Quantization != quant {
		t.Errorf("Quantization = %q, want %q", form.Quantization, quant)
	}
}

func TestProfileFormValuesFromProfile_Quantization_NilIsBlank(t *testing.T) {
	p := &db.Profile{Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.Quantization != "" {
		t.Errorf("Quantization = %q, want empty when not set", form.Quantization)
	}
}
