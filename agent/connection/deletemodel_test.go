// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"os"
	"path/filepath"
	"testing"
)

func newDeleteTestConn(t *testing.T) (*Conn, string) {
	t.Helper()
	root := t.TempDir()
	return New(Config{ModelStoragePath: root}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger()), root
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRemoveModel_SafetensorsRemovesWholeDir(t *testing.T) {
	c, root := newDeleteTestConn(t)
	writeFile(t, filepath.Join(root, "org", "m", "model-00001.safetensors"))
	if err := c.removeModel("org/m", "FP16", "safetensors"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "org", "m")) {
		t.Error("model dir should be gone")
	}
	if !exists(filepath.Join(root, "org")) {
		t.Error("only the model dir should be removed, not its parent")
	}
}

func TestRemoveModel_GGUFQuantizationLeavesSiblings(t *testing.T) {
	c, root := newDeleteTestConn(t)
	dir := filepath.Join(root, "org", "m")
	writeFile(t, filepath.Join(dir, "m-Q4_K_M.gguf"))
	writeFile(t, filepath.Join(dir, "m-Q8_0.gguf"))
	if err := c.removeModel("org/m", "Q4_K_M", "gguf"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "m-Q4_K_M.gguf")) || !exists(filepath.Join(dir, "m-Q8_0.gguf")) {
		t.Error("only the Q4_K_M file should be removed")
	}
	if err := c.removeModel("org/m", "Q8_0", "gguf"); err != nil {
		t.Fatal(err)
	}
	if exists(dir) {
		t.Error("dir should be removed once no .gguf files remain")
	}
}

func TestRemoveModel_GGUFUnknownQuantizationRemovesWholeDir(t *testing.T) {
	c, root := newDeleteTestConn(t)
	writeFile(t, filepath.Join(root, "org", "m", "m.gguf"))
	if err := c.removeModel("org/m", "UNKNOWN", "gguf"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "org", "m")) {
		t.Error("dir should be gone")
	}
}

func TestRemoveModel_GGUFNoMatchErrors(t *testing.T) {
	c, root := newDeleteTestConn(t)
	writeFile(t, filepath.Join(root, "org", "m", "m-Q8_0.gguf"))
	if err := c.removeModel("org/m", "Q4_K_M", "gguf"); err == nil {
		t.Error("want an error when no file matches")
	}
}

func TestRemoveModel_RejectsPathEscape(t *testing.T) {
	c, root := newDeleteTestConn(t)
	outside := filepath.Join(filepath.Dir(root), "outside-"+filepath.Base(root))
	writeFile(t, filepath.Join(outside, "keep.txt"))
	t.Cleanup(func() { os.RemoveAll(outside) })

	for _, ref := range []string{"", ".", "..", "../" + filepath.Base(outside), "org/../../" + filepath.Base(outside), "/etc"} {
		if err := c.removeModel(ref, "", "safetensors"); err == nil {
			t.Errorf("removeModel(%q) succeeded, want an error", ref)
		}
	}
	if !exists(filepath.Join(outside, "keep.txt")) || !exists(root) {
		t.Error("nothing outside model storage may be touched, and the root itself must survive")
	}
}

func TestRemoveModel_MissingDirErrors(t *testing.T) {
	c, _ := newDeleteTestConn(t)
	if err := c.removeModel("org/none", "", "safetensors"); err == nil {
		t.Error("want an error for a missing directory")
	}
}
