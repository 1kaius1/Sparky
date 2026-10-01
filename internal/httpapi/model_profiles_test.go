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
		Name: "test-profile", EngineType: "llamacpp",
		TargetNodeID: "node-1", Port: "8000",
		Entry: encodeEntry("test-org/test-model", "", db.ModelFormatSafetensors),
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

// Quantization is no longer a free-typed field - it comes from the
// Inventory-driven picker's Entry value (encodeEntry/decodeEntry,
// internal/httpapi/inventory_transfer.go), alongside ModelRef and Format.
// See TestFieldsFromForm_Entry_* and TestProfileFormValuesFromProfile_Entry_*
// below for that round trip.

func TestFieldsFromForm_Entry_DecodesModelRefQuantizationFormat(t *testing.T) {
	form := validProfileForm()
	form.Entry = encodeEntry("org/repo", "Q4_K_M", db.ModelFormatGGUF)

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.ModelRef != "org/repo" {
		t.Errorf("ModelRef = %q, want %q", fields.ModelRef, "org/repo")
	}
	if fields.Quantization != "Q4_K_M" {
		t.Errorf("Quantization = %q, want %q", fields.Quantization, "Q4_K_M")
	}
	if fields.Format != db.ModelFormatGGUF {
		t.Errorf("Format = %q, want %q", fields.Format, db.ModelFormatGGUF)
	}
}

func TestFieldsFromForm_Entry_WholeRepoQuantizationIsEmptyString(t *testing.T) {
	form := validProfileForm()
	form.Entry = encodeEntry("org/repo", "", db.ModelFormatSafetensors)

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Quantization != "" {
		t.Errorf("Quantization = %q, want empty for a whole-repo entry", fields.Quantization)
	}
}

func TestFieldsFromForm_Entry_Empty(t *testing.T) {
	form := validProfileForm()
	form.Entry = ""

	if _, err := fieldsFromForm(form); err == nil {
		t.Error("fieldsFromForm() succeeded with no model chosen, want an error")
	}
}

func TestFieldsFromForm_Entry_Malformed(t *testing.T) {
	form := validProfileForm()
	form.Entry = "not-a-valid-entry"

	if _, err := fieldsFromForm(form); err == nil {
		t.Error("fieldsFromForm() succeeded with a malformed entry, want an error")
	}
}

func TestProfileFormValuesFromProfile_Entry_RoundTrips(t *testing.T) {
	p := &db.Profile{ModelRef: "org/repo", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF, Port: 8000}

	form := profileFormValuesFromProfile(p)
	modelRef, quant, format, ok := decodeEntry(form.Entry)
	if !ok {
		t.Fatalf("decodeEntry(%q) failed", form.Entry)
	}
	if modelRef != "org/repo" || quant != "Q4_K_M" || format != db.ModelFormatGGUF {
		t.Errorf("decoded entry = (%q, %q, %q), want (%q, %q, %q)", modelRef, quant, format, "org/repo", "Q4_K_M", db.ModelFormatGGUF)
	}
}

func TestProfileFormValuesFromProfile_Entry_WholeRepo(t *testing.T) {
	p := &db.Profile{ModelRef: "org/repo", Quantization: "", Format: db.ModelFormatSafetensors, Port: 8000}

	form := profileFormValuesFromProfile(p)
	modelRef, quant, format, ok := decodeEntry(form.Entry)
	if !ok {
		t.Fatalf("decodeEntry(%q) failed", form.Entry)
	}
	if modelRef != "org/repo" || quant != "" || format != db.ModelFormatSafetensors {
		t.Errorf("decoded entry = (%q, %q, %q), want (%q, %q, %q)", modelRef, quant, format, "org/repo", "", db.ModelFormatSafetensors)
	}
}

func TestFieldsFromForm_Image_Set(t *testing.T) {
	form := validProfileForm()
	form.Image = "nvcr.io/nvidia/vllm:26.06-py3"

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Image == nil || *fields.Image != "nvcr.io/nvidia/vllm:26.06-py3" {
		t.Errorf("Image = %v, want %q", fields.Image, "nvcr.io/nvidia/vllm:26.06-py3")
	}
}

func TestFieldsFromForm_Image_BlankIsNil(t *testing.T) {
	form := validProfileForm()
	form.Image = ""

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Image != nil {
		t.Errorf("Image = %v, want nil for a blank field", *fields.Image)
	}
}

func TestFieldsFromForm_Image_WhitespaceOnlyIsNil(t *testing.T) {
	form := validProfileForm()
	form.Image = "   "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Image != nil {
		t.Errorf("Image = %v, want nil for a whitespace-only field", *fields.Image)
	}
}

func TestFieldsFromForm_Image_Trimmed(t *testing.T) {
	form := validProfileForm()
	form.Image = "  nvcr.io/nvidia/vllm:26.06-py3  "

	fields, err := fieldsFromForm(form)
	if err != nil {
		t.Fatalf("fieldsFromForm() error: %v", err)
	}
	if fields.Image == nil || *fields.Image != "nvcr.io/nvidia/vllm:26.06-py3" {
		t.Errorf("Image = %v, want trimmed %q", fields.Image, "nvcr.io/nvidia/vllm:26.06-py3")
	}
}

func TestProfileFormValuesFromProfile_Image_Set(t *testing.T) {
	image := "nvcr.io/nvidia/vllm:26.06-py3"
	p := &db.Profile{Image: &image, Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.Image != image {
		t.Errorf("Image = %q, want %q", form.Image, image)
	}
}

func TestProfileFormValuesFromProfile_Image_NilIsBlank(t *testing.T) {
	p := &db.Profile{Port: 8000}

	form := profileFormValuesFromProfile(p)
	if form.Image != "" {
		t.Errorf("Image = %q, want empty when not set", form.Image)
	}
}
