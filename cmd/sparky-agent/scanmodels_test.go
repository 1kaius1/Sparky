// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedModel(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, "Org", "Name", "model.safetensors")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanModels_Table(t *testing.T) {
	root := t.TempDir()
	seedModel(t, root)
	var out, errOut bytes.Buffer
	code := scanModels([]string{"--path", root}, func(string) string { return "" }, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"Org/Name", "safetensors", "(whole repo)", "2.0 KiB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestScanModels_EnvPathAndJSON(t *testing.T) {
	root := t.TempDir()
	seedModel(t, root)
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == "SPARKY_MODEL_STORAGE_PATH" {
			return root
		}
		return ""
	}
	if code := scanModels([]string{"--json"}, getenv, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got struct {
		StoragePath string `json:"storage_path"`
		Models      []struct {
			ModelRef string `json:"model_ref"`
			Format   string `json:"format"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if got.StoragePath != root || len(got.Models) != 1 || got.Models[0].ModelRef != "Org/Name" {
		t.Errorf("unexpected JSON: %+v", got)
	}
}

func TestScanModels_EmptyAndError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := scanModels([]string{"--path", t.TempDir()}, func(string) string { return "" }, &out, &errOut); code != 0 || !strings.Contains(out.String(), "No models found") {
		t.Errorf("empty dir: code=%d out=%q", code, out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := scanModels([]string{"--path", filepath.Join(t.TempDir(), "missing")}, func(string) string { return "" }, &out, &errOut); code != 1 || errOut.Len() == 0 {
		t.Errorf("missing dir: code=%d err=%q", code, errOut.String())
	}
}
