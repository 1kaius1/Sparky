// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

// Note: this package has no existing test coverage for the Model profiles
// create/edit form's HTTP handlers (handleCreateProfile/handleUpdateProfile
// themselves) - a pre-existing gap, not introduced here. These tests cover
// the two pure functions the engine_version field touches directly:
// fieldsFromForm (submitted form -> profiles.Fields) and
// profileFormValuesFromProfile (a persisted profile -> form values, for
// the edit form's prefill).

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
