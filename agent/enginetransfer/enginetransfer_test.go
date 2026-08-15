// SPDX-License-Identifier: AGPL-3.0-or-later

package enginetransfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// buildTarball produces a real tar archive (gzip, not xz - Go's stdlib has
// no xz support, which is exactly why Provision shells out to the system
// `tar` for extraction; tests fake that shell-out with a fakeRunner that
// understands gzip instead, so no real xz tooling is required to run
// `go test`) containing the given files.
func buildTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// gzipRunner is a fake runner standing in for the real `tar -xJf` shell-out
// - it extracts the gzip tarball buildTarball produced instead, since the
// test has no real .xz file to hand it. Confirms Provision invokes
// extraction with the right arguments and wires the result correctly; the
// real `tar -xJf` command line itself is exercised by
// TestRunCommand_RealTarBinary below, against a real system `tar`.
type gzipRunner struct {
	calls []call
}

type call struct {
	name string
	args []string
}

func (g *gzipRunner) run(_ context.Context, name string, args ...string) error {
	g.calls = append(g.calls, call{name, args})
	if len(args) != 4 || args[0] != "-xJf" || args[2] != "-C" {
		return fmt.Errorf("unexpected tar invocation: %s %v", name, args)
	}
	tarballPath, destDir := args[1], args[3]

	raw, err := os.ReadFile(tarballPath)
	if err != nil {
		return fmt.Errorf("read tarball: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		full := filepath.Join(destDir, hdr.Name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		out, err := os.Create(full)
		if err != nil {
			return err
		}
		if _, err := out.ReadFrom(tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
	return nil
}

// fakeReleaseServer stands in for GitHub's release-asset download endpoint
// (a flat /{version}/{asset} path under the executor's baseURL - the real
// URL shape is .../releases/download/{version}/{asset}, and only the path
// suffix after baseURL matters here).
type fakeReleaseServer struct {
	assets map[string][]byte // path -> content, e.g. "/b4610/llamacpp-b4610-amd64.tar.xz"

	mu    sync.Mutex
	calls []string
}

func (s *fakeReleaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.URL.Path)
	s.mu.Unlock()

	content, ok := s.assets[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// buildTestAssets builds a valid {tarball, checksum} asset pair for
// engineType/version on this test binary's own GOARCH (matching what
// assetName would compute), keyed by path under the fake server's baseURL.
func buildTestAssets(t *testing.T, engineType, version string, files map[string]string) map[string][]byte {
	t.Helper()
	tarball := buildTarball(t, files)
	asset := fmt.Sprintf("%s-%s-%s.tar.xz", engineType, version, runtime.GOARCH)
	checksum := sha256Hex(tarball) + "  " + asset + "\n"
	return map[string][]byte{
		"/" + version + "/" + asset:             tarball,
		"/" + version + "/" + asset + ".sha256": []byte(checksum),
	}
}

func newTestExecutor(srv *httptest.Server, run runner) *Executor {
	return &Executor{baseURL: srv.URL, client: srv.Client(), progressBytes: defaultProgressBytes, chunkBytes: downloadChunkSize, run: run}
}

func collectProgress() (ProgressFunc, func() []Progress) {
	var mu sync.Mutex
	var calls []Progress
	fn := func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, p)
	}
	return fn, func() []Progress {
		mu.Lock()
		defer mu.Unlock()
		return append([]Progress(nil), calls...)
	}
}

func TestExecutor_Provision_InstallsAndSwapsLatest(t *testing.T) {
	files := map[string]string{"llama-server": "#!/bin/sh\necho fake binary\n"}
	assets := buildTestAssets(t, "llamacpp", "b4610", files)
	srv := httptest.NewServer(&fakeReleaseServer{assets: assets})
	defer srv.Close()

	installRoot := t.TempDir()
	gz := &gzipRunner{}
	e := newTestExecutor(srv, gz.run)
	progress, calls := collectProgress()

	installPath, size, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress)
	if err != nil {
		t.Fatalf("Provision() error: %v", err)
	}

	wantInstallPath := filepath.Join(installRoot, "llamacpp", "b4610")
	if installPath != wantInstallPath {
		t.Errorf("installPath = %q, want %q", installPath, wantInstallPath)
	}
	if size != int64(len(files["llama-server"])) {
		t.Errorf("size = %d, want %d", size, len(files["llama-server"]))
	}

	got, err := os.ReadFile(filepath.Join(wantInstallPath, "llama-server"))
	if err != nil {
		t.Fatalf("ReadFile(installed binary) error: %v", err)
	}
	if string(got) != files["llama-server"] {
		t.Errorf("installed binary content = %q, want %q", got, files["llama-server"])
	}

	latest := filepath.Join(installRoot, "llamacpp", "latest")
	target, err := os.Readlink(latest)
	if err != nil {
		t.Fatalf("Readlink(latest) error: %v", err)
	}
	if target != "b4610" {
		t.Errorf("latest symlink target = %q, want %q", target, "b4610")
	}

	last := calls()[len(calls())-1]
	if last.Status != StatusCompleted {
		t.Errorf("final progress status = %q, want %q", last.Status, StatusCompleted)
	}
	if last.InstallPath != wantInstallPath {
		t.Errorf("final progress InstallPath = %q, want %q", last.InstallPath, wantInstallPath)
	}
	if last.InstalledSizeBytes != size {
		t.Errorf("final progress InstalledSizeBytes = %d, want %d", last.InstalledSizeBytes, size)
	}

	// The temp download file must not be left behind.
	entries, err := os.ReadDir(filepath.Join(installRoot, "llamacpp"))
	if err != nil {
		t.Fatalf("ReadDir(engineDir) error: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") {
			t.Errorf("leftover temp download file %q was not cleaned up", e.Name())
		}
	}
}

func TestExecutor_Provision_SecondVersionCoexistsAndMovesLatest(t *testing.T) {
	filesA := map[string]string{"llama-server": "version A"}
	filesB := map[string]string{"llama-server": "version B, longer content"}
	assetsA := buildTestAssets(t, "llamacpp", "b4523", filesA)
	assetsB := buildTestAssets(t, "llamacpp", "b4610", filesB)
	merged := map[string][]byte{}
	for k, v := range assetsA {
		merged[k] = v
	}
	for k, v := range assetsB {
		merged[k] = v
	}
	srv := httptest.NewServer(&fakeReleaseServer{assets: merged})
	defer srv.Close()

	installRoot := t.TempDir()
	e := newTestExecutor(srv, (&gzipRunner{}).run)
	progress, _ := collectProgress()

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4523", installRoot, progress); err != nil {
		t.Fatalf("Provision(b4523) error: %v", err)
	}
	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err != nil {
		t.Fatalf("Provision(b4610) error: %v", err)
	}

	for _, version := range []string{"b4523", "b4610"} {
		if _, err := os.Stat(filepath.Join(installRoot, "llamacpp", version, "llama-server")); err != nil {
			t.Errorf("version %s no longer installed after provisioning a second version: %v", version, err)
		}
	}

	target, err := os.Readlink(filepath.Join(installRoot, "llamacpp", "latest"))
	if err != nil {
		t.Fatalf("Readlink(latest) error: %v", err)
	}
	if target != "b4610" {
		t.Errorf("latest symlink target = %q, want %q (most recently provisioned)", target, "b4610")
	}
}

func TestExecutor_Provision_ChecksumMismatch_Fails(t *testing.T) {
	files := map[string]string{"llama-server": "content"}
	assets := buildTestAssets(t, "llamacpp", "b4610", files)
	asset := fmt.Sprintf("llamacpp-b4610-%s.tar.xz", runtime.GOARCH)
	assets["/b4610/"+asset+".sha256"] = []byte(strings.Repeat("0", 64) + "  " + asset + "\n")
	srv := httptest.NewServer(&fakeReleaseServer{assets: assets})
	defer srv.Close()

	installRoot := t.TempDir()
	e := newTestExecutor(srv, (&gzipRunner{}).run)
	progress, calls := collectProgress()

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err == nil {
		t.Fatal("Provision() succeeded despite a checksum mismatch, want an error")
	}

	last := calls()[len(calls())-1]
	if last.Status != StatusFailed {
		t.Errorf("final progress status = %q, want %q", last.Status, StatusFailed)
	}
	if !strings.Contains(last.ErrorMessage, "checksum mismatch") {
		t.Errorf("ErrorMessage = %q, want it to mention a checksum mismatch", last.ErrorMessage)
	}

	if _, err := os.Stat(filepath.Join(installRoot, "llamacpp", "b4610")); err == nil {
		t.Error("version directory was created despite a checksum mismatch, want nothing installed")
	}
}

func TestExecutor_Provision_MissingChecksumFile_Fails(t *testing.T) {
	files := map[string]string{"llama-server": "content"}
	assets := buildTestAssets(t, "llamacpp", "b4610", files)
	asset := fmt.Sprintf("llamacpp-b4610-%s.tar.xz", runtime.GOARCH)
	delete(assets, "/b4610/"+asset+".sha256")
	srv := httptest.NewServer(&fakeReleaseServer{assets: assets})
	defer srv.Close()

	installRoot := t.TempDir()
	e := newTestExecutor(srv, (&gzipRunner{}).run)
	progress, calls := collectProgress()

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err == nil {
		t.Fatal("Provision() succeeded despite a missing checksum file, want an error")
	}
	if got := calls(); len(got) == 0 || got[len(got)-1].Status != StatusFailed {
		t.Errorf("progress calls = %+v, want a final StatusFailed call", got)
	}
}

func TestExecutor_Provision_MissingTarball_Fails(t *testing.T) {
	files := map[string]string{"llama-server": "content"}
	assets := buildTestAssets(t, "llamacpp", "b4610", files)
	asset := fmt.Sprintf("llamacpp-b4610-%s.tar.xz", runtime.GOARCH)
	delete(assets, "/b4610/"+asset)
	srv := httptest.NewServer(&fakeReleaseServer{assets: assets})
	defer srv.Close()

	installRoot := t.TempDir()
	e := newTestExecutor(srv, (&gzipRunner{}).run)
	progress, _ := collectProgress()

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err == nil {
		t.Fatal("Provision() succeeded despite a missing tarball asset, want an error")
	}
}

func TestExecutor_Provision_ExtractionFails(t *testing.T) {
	files := map[string]string{"llama-server": "content"}
	assets := buildTestAssets(t, "llamacpp", "b4610", files)
	srv := httptest.NewServer(&fakeReleaseServer{assets: assets})
	defer srv.Close()

	installRoot := t.TempDir()
	failingRun := func(context.Context, string, ...string) error { return errors.New("tar: not a valid archive") }
	e := newTestExecutor(srv, failingRun)
	progress, calls := collectProgress()

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err == nil {
		t.Fatal("Provision() succeeded despite an extraction failure, want an error")
	}

	last := calls()[len(calls())-1]
	if last.Status != StatusFailed {
		t.Errorf("final progress status = %q, want %q", last.Status, StatusFailed)
	}
}

func TestExecutor_Provision_ReprovisioningSameVersionReplaces(t *testing.T) {
	filesOld := map[string]string{"llama-server": "old content"}
	filesNew := map[string]string{"llama-server": "new content, this replaces the old install"}

	installRoot := t.TempDir()

	assetsOld := buildTestAssets(t, "llamacpp", "b4610", filesOld)
	srvOld := httptest.NewServer(&fakeReleaseServer{assets: assetsOld})
	e := newTestExecutor(srvOld, (&gzipRunner{}).run)
	progress, _ := collectProgress()
	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err != nil {
		t.Fatalf("first Provision() error: %v", err)
	}
	srvOld.Close()

	assetsNew := buildTestAssets(t, "llamacpp", "b4610", filesNew)
	srvNew := httptest.NewServer(&fakeReleaseServer{assets: assetsNew})
	defer srvNew.Close()
	e.baseURL = srvNew.URL

	if _, _, err := e.Provision(context.Background(), "llamacpp", "b4610", installRoot, progress); err != nil {
		t.Fatalf("second Provision() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(installRoot, "llamacpp", "b4610", "llama-server"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if string(got) != filesNew["llama-server"] {
		t.Errorf("content after re-provisioning = %q, want the new content %q (not stale old content)", got, filesNew["llama-server"])
	}
}

func TestRunCommand_RealTarBinary(t *testing.T) {
	if _, err := os.Stat("/usr/bin/tar"); err != nil {
		t.Skip("no system tar binary available")
	}
	// Sanity-checks runCommand itself (the real, non-faked shell-out) can
	// invoke tar and surface a failure's output - separate from the
	// gzipRunner-faked Provision tests above, which never exercise this
	// function.
	if err := runCommand(context.Background(), "tar", "--this-flag-does-not-exist"); err == nil {
		t.Fatal("runCommand() succeeded for an invalid tar invocation, want an error")
	}
}
