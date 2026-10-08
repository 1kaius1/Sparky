// SPDX-License-Identifier: AGPL-3.0-or-later

package modelscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates root/rel with size bytes, making parent directories.
func writeFile(t *testing.T, root, rel string, size int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(cs []Candidate, ref, quant, format string) *Candidate {
	for i := range cs {
		if cs[i].ModelRef == ref && cs[i].Quantization == quant && cs[i].Format == format {
			return &cs[i]
		}
	}
	return nil
}

func TestScan_SafetensorsWholeRepo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Org/Name/config.json", 10)
	writeFile(t, root, "Org/Name/model-00001-of-00002.safetensors", 100)
	writeFile(t, root, "Org/Name/model-00002-of-00002.safetensors", 200)
	writeFile(t, root, "Org/Name/onnx/extra.bin", 5) // belongs to this model, not a second one
	writeFile(t, root, "Org/Name/.gitattributes", 1)

	got, truncated, err := Scan(root)
	if err != nil || truncated {
		t.Fatalf("Scan: err=%v truncated=%v", err, truncated)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := find(got, "Org/Name", "", FormatSafetensors)
	if c == nil {
		t.Fatalf("whole-repo safetensors candidate missing: %+v", got)
	}
	if c.SizeBytes != 10+100+200+5+1 {
		t.Errorf("SizeBytes = %d, want 316", c.SizeBytes)
	}
	if c.PossiblyIncomplete {
		t.Errorf("PossiblyIncomplete set without an rsync temp file")
	}
}

func TestScan_SingleSegmentRef(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "single/model.safetensors", 7)
	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if find(got, "single", "", FormatSafetensors) == nil {
		t.Fatalf("single-segment ref not found: %+v", got)
	}
}

func TestScan_GGUFQuantizations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Org/Gguf/llama-2-7b.Q4_K_M.gguf", 40)
	writeFile(t, root, "Org/Gguf/llama-2-7b.Q8_0.gguf", 80)
	writeFile(t, root, "Org/Gguf/llama-2-7b-IQ4_XS.gguf", 30)
	writeFile(t, root, "Org/Gguf/mystery.gguf", 9)
	writeFile(t, root, "Org/Gguf/README.md", 3)

	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		quant string
		size  int64
		file  string
	}{
		{"Q4_K_M", 40, "llama-2-7b.Q4_K_M.gguf"},
		{"Q8_0", 80, "llama-2-7b.Q8_0.gguf"},
		{"IQ4_XS", 30, "llama-2-7b-IQ4_XS.gguf"},
		{UnknownQuantization, 9, "mystery.gguf"},
	} {
		c := find(got, "Org/Gguf", tc.quant, FormatGGUF)
		if c == nil {
			t.Errorf("missing %s candidate: %+v", tc.quant, got)
			continue
		}
		if c.SizeBytes != tc.size || c.FileName != tc.file {
			t.Errorf("%s: size=%d file=%q, want %d %q", tc.quant, c.SizeBytes, c.FileName, tc.size, tc.file)
		}
		// The label must appear verbatim in the file name so the
		// contains-match used by download/delete can find the file.
		if tc.quant != UnknownQuantization && !strings.Contains(c.FileName, c.Quantization) {
			t.Errorf("%s not contained in %q", c.Quantization, c.FileName)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d candidates, want 4", len(got))
	}
}

func TestScan_ShardedGGUFIsOneCandidate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Org/Big/big-Q4_K_M-00002-of-00002.gguf", 60)
	writeFile(t, root, "Org/Big/big-Q4_K_M-00001-of-00002.gguf", 50)

	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Quantization != "Q4_K_M" || c.SizeBytes != 110 || c.FileName != "big-Q4_K_M-00001-of-00002.gguf" {
		t.Errorf("unexpected shard candidate: %+v", c)
	}
}

func TestScan_MixedFormatsInOneDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Org/Mixed/model.safetensors", 100)
	writeFile(t, root, "Org/Mixed/mixed.Q4_K_M.gguf", 40)

	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	st := find(got, "Org/Mixed", "", FormatSafetensors)
	gg := find(got, "Org/Mixed", "Q4_K_M", FormatGGUF)
	if st == nil || gg == nil {
		t.Fatalf("expected both formats: %+v", got)
	}
	if st.SizeBytes != 100 {
		t.Errorf("safetensors size = %d, want 100 (gguf excluded)", st.SizeBytes)
	}
}

func TestScan_RsyncTempFileFlagsIncomplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Org/Partial/model.safetensors", 10)
	writeFile(t, root, "Org/Partial/.model-2.safetensors.aB3dE9", 500)

	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	c := find(got, "Org/Partial", "", FormatSafetensors)
	if c == nil || !c.PossiblyIncomplete {
		t.Fatalf("expected PossiblyIncomplete candidate: %+v", got)
	}
	if c.SizeBytes != 10 {
		t.Errorf("SizeBytes = %d, want 10 (temp file excluded)", c.SizeBytes)
	}
}

func TestScan_SkipsHiddenSymlinksAndEmpty(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "Evil/Model/model.safetensors", 1)
	if err := os.Symlink(filepath.Join(outside, "Evil"), filepath.Join(root, "Evil")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	writeFile(t, root, ".hidden/Model/model.safetensors", 1)
	if err := os.MkdirAll(filepath.Join(root, "Org", "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Org/ConfigOnly/config.json", 1)
	// A weight file that is itself a symlink out of the root is ignored.
	writeFile(t, outside, "real.safetensors", 1)
	if err := os.MkdirAll(filepath.Join(root, "Org", "LinkedWeights"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "real.safetensors"), filepath.Join(root, "Org", "LinkedWeights", "m.safetensors")); err != nil {
		t.Fatal(err)
	}

	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no candidates, got %+v", got)
	}
}

func TestScan_RootIsNeverAModel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "stray.safetensors", 1)
	got, _, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stray file at the root became a candidate: %+v", got)
	}
}

func TestScan_Truncates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxCandidates+5; i++ {
		writeFile(t, root, fmt.Sprintf("Org/m%04d/model.safetensors", i), 1)
	}
	got, truncated, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) != MaxCandidates {
		t.Fatalf("truncated=%v len=%d, want true/%d", truncated, len(got), MaxCandidates)
	}
}

func TestScan_BadRoot(t *testing.T) {
	if _, _, err := Scan(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error for a missing root")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Scan(f); err == nil {
		t.Error("expected an error for a root that is a file")
	}
}

func TestQuantizationFromName(t *testing.T) {
	for name, want := range map[string]string{
		"llama-2-7b.Q4_K_M":  "Q4_K_M",
		"model-q8_0":         "q8_0",
		"x-IQ3_XXS":          "IQ3_XXS",
		"model-BF16":         "BF16",
		"model-F16":          "F16",
		"Qwen3-8B":           UnknownQuantization,
		"plainname":          UnknownQuantization,
		"Meta-Llama-3-8B-Q6": "Q6",
	} {
		if got := quantizationFromName(name); got != want {
			t.Errorf("quantizationFromName(%q) = %q, want %q", name, got, want)
		}
	}
}
