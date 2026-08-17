// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHubServer stands in for the real Hugging Face Hub API - see
// PLANNING.md's Decisions Log for the real endpoint shapes this mirrors:
// GET /api/models/{repo} listing siblings (no sizes), and
// GET/HEAD /{repo}/resolve/{revision}/{file} serving content, honoring
// Range unless ignoreRange is set.
type fakeHubServer struct {
	modelRef    string
	files       map[string][]byte
	ignoreRange bool

	mu       sync.Mutex
	getCalls map[string]int
}

func newFakeHubServer(modelRef string, files map[string][]byte) *fakeHubServer {
	return &fakeHubServer{modelRef: modelRef, files: files, getCalls: make(map[string]int)}
}

func (s *fakeHubServer) fileGetCalls(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls[name]
}

func (s *fakeHubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/models/"+s.modelRef {
		type sibling struct {
			RFilename string `json:"rfilename"`
		}
		var siblings []sibling
		for name := range s.files {
			siblings = append(siblings, sibling{RFilename: name})
		}
		sort.Slice(siblings, func(i, j int) bool { return siblings[i].RFilename < siblings[j].RFilename })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"siblings": siblings})
		return
	}

	resolvePrefix := "/" + s.modelRef + "/resolve/main/"
	if strings.HasPrefix(r.URL.Path, resolvePrefix) {
		filename := strings.TrimPrefix(r.URL.Path, resolvePrefix)
		content, ok := s.files[filename]
		if !ok {
			http.NotFound(w, r)
			return
		}

		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			return
		}

		s.mu.Lock()
		s.getCalls[filename]++
		s.mu.Unlock()

		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" && !s.ignoreRange {
			var start int64
			if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-", &start); err == nil && start >= 0 && start < int64(len(content)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
				w.Header().Set("Accept-Ranges", "bytes")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(content[start:])
				return
			}
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}

	http.NotFound(w, r)
}

func newTestExecutor(srv *httptest.Server, progressBytes int64) *Executor {
	return &Executor{baseURL: srv.URL, client: srv.Client(), progressBytes: progressBytes, chunkBytes: downloadChunkSize}
}

type progressCall struct {
	bytesTransferred int64
	bytesTotal       int64
	status           string
	errMsg           string
}

func collectProgress() (ProgressFunc, func() []progressCall) {
	var mu sync.Mutex
	var calls []progressCall
	fn := func(bytesTransferred, bytesTotal int64, status, errMsg string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, progressCall{bytesTransferred, bytesTotal, status, errMsg})
	}
	return fn, func() []progressCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]progressCall(nil), calls...)
	}
}

func TestExecutor_Download_FetchesAllFiles(t *testing.T) {
	files := map[string][]byte{
		"README.md":   []byte("# test model"),
		"config.json": []byte(`{"model_type":"test"}`),
	}
	hub := newFakeHubServer("test-org/test-model", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, calls := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(destDir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s) error: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}

	final := calls()[len(calls())-1]
	if final.status != StatusCompleted {
		t.Errorf("final status = %q, want %q", final.status, StatusCompleted)
	}
	wantTotal := int64(len(files["README.md"]) + len(files["config.json"]))
	if final.bytesTransferred != wantTotal || final.bytesTotal != wantTotal {
		t.Errorf("final progress = %+v, want bytesTransferred=bytesTotal=%d", final, wantTotal)
	}
}

func TestExecutor_Download_ReportsProgressPeriodically(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 1000)
	files := map[string][]byte{"model.bin": content}
	hub := newFakeHubServer("test-org/test-model", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	// A small chunk size and progress threshold relative to the 1000-byte
	// file forces multiple mid-file Read/progress calls instead of just
	// the start/end pair, regardless of how the in-process transport
	// happens to buffer the response.
	progress, calls := collectProgress()
	e := &Executor{baseURL: srv.URL, client: srv.Client(), progressBytes: 100, chunkBytes: 64}

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	got := calls()
	if len(got) < 4 {
		t.Fatalf("got %d progress calls, want at least 4 (start + several mid-transfer + completed) for a 1000-byte file with a 100-byte threshold: %+v", len(got), got)
	}
	if got[0].status != StatusTransferring || got[0].bytesTransferred != 0 {
		t.Errorf("first call = %+v, want status=%q bytesTransferred=0", got[0], StatusTransferring)
	}
	for i := 1; i < len(got)-1; i++ {
		if got[i].status != StatusTransferring {
			t.Errorf("call %d status = %q, want %q", i, got[i].status, StatusTransferring)
		}
		if got[i].bytesTransferred < got[i-1].bytesTransferred {
			t.Errorf("call %d bytesTransferred = %d, want >= previous call's %d (must be monotonically non-decreasing)", i, got[i].bytesTransferred, got[i-1].bytesTransferred)
		}
	}
	last := got[len(got)-1]
	if last.status != StatusCompleted || last.bytesTransferred != int64(len(content)) {
		t.Errorf("last call = %+v, want status=%q bytesTransferred=%d", last, StatusCompleted, len(content))
	}
}

func TestExecutor_Download_ResumesPartialFile(t *testing.T) {
	content := bytes.Repeat([]byte("abcdefgh"), 50) // 400 bytes
	files := map[string][]byte{"model.bin": content}
	hub := newFakeHubServer("test-org/test-model", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	// Pre-write the first half, as if a previous attempt got this far.
	partial := content[:200]
	if err := os.WriteFile(filepath.Join(destDir, "model.bin"), partial, 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "model.bin"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("resumed file content mismatch: got %d bytes, want %d bytes matching the full remote content", len(got), len(content))
	}
}

func TestExecutor_Download_ServerIgnoresRange_RestartsFromScratch(t *testing.T) {
	content := bytes.Repeat([]byte("abcdefgh"), 50) // 400 bytes
	files := map[string][]byte{"model.bin": content}
	hub := newFakeHubServer("test-org/test-model", files)
	hub.ignoreRange = true
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	// Pre-write garbage that does NOT match a prefix of content - if the
	// executor blindly appended a 200 response to this, the result would
	// be corrupt (garbage followed by the full file).
	garbage := bytes.Repeat([]byte("Z"), 200)
	if err := os.WriteFile(filepath.Join(destDir, "model.bin"), garbage, 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "model.bin"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content after a Range-ignoring server response = %d bytes, want the full %d-byte remote content with no leftover garbage prefix", len(got), len(content))
	}
}

func TestExecutor_Download_AlreadyComplete_Skipped(t *testing.T) {
	content := []byte("already have this one")
	files := map[string][]byte{"model.bin": content}
	hub := newFakeHubServer("test-org/test-model", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(destDir, "model.bin"), content, 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	if got := hub.fileGetCalls("model.bin"); got != 0 {
		t.Errorf("GET was called %d times for an already-complete file, want 0 (HEAD-only sizing, no re-download)", got)
	}
}

func TestExecutor_Download_ListFilesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	destDir := t.TempDir()
	progress, calls := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err == nil {
		t.Fatal("Download() succeeded despite a 500 from the listing endpoint, want an error")
	}

	got := calls()
	if len(got) != 1 || got[0].status != StatusFailed {
		t.Errorf("progress calls = %+v, want exactly one StatusFailed call", got)
	}
}

func TestExecutor_Download_FileDownloadError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/test-org/test-model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"siblings":[{"rfilename":"model.bin"}]}`))
	})
	mux.HandleFunc("/test-org/test-model/resolve/main/model.bin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "gone", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	destDir := t.TempDir()
	progress, calls := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err == nil {
		t.Fatal("Download() succeeded despite a 500 downloading the file, want an error")
	}

	got := calls()
	last := got[len(got)-1]
	if last.status != StatusFailed {
		t.Errorf("last progress call status = %q, want %q", last.status, StatusFailed)
	}
}

func TestExecutor_Download_ContextAlreadyCanceled(t *testing.T) {
	hub := newFakeHubServer("test-org/test-model", map[string][]byte{"f.txt": []byte("x")})
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := e.Download(ctx, "test-org/test-model", "", destDir, progress); err == nil {
		t.Fatal("Download() succeeded with an already-canceled context, want an error")
	}
}

func TestExecutor_Download_NestedFilePath(t *testing.T) {
	content := []byte("onnx content")
	files := map[string][]byte{"onnx/model.onnx": content}
	hub := newFakeHubServer("test-org/test-model", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/test-model", "", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "onnx", "model.onnx"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("nested file content = %q, want %q", got, content)
	}
}

func TestExecutor_Download_QuantizationDownloadsOnlyMatchingFile(t *testing.T) {
	files := map[string][]byte{
		"README.md":              []byte("# test model"),
		"llama-2-7b.Q4_K_M.gguf": []byte("q4 content"),
		"llama-2-7b.Q5_K_M.gguf": []byte("q5 content"),
	}
	hub := newFakeHubServer("test-org/multi-quant", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, calls := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	if err := e.Download(context.Background(), "test-org/multi-quant", "Q4_K_M", destDir, progress); err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "llama-2-7b.Q4_K_M.gguf"))
	if err != nil {
		t.Fatalf("ReadFile(Q4_K_M) error: %v", err)
	}
	if !bytes.Equal(got, files["llama-2-7b.Q4_K_M.gguf"]) {
		t.Errorf("Q4_K_M content = %q, want %q", got, files["llama-2-7b.Q4_K_M.gguf"])
	}

	for _, unwanted := range []string{"llama-2-7b.Q5_K_M.gguf", "README.md"} {
		if _, err := os.Stat(filepath.Join(destDir, unwanted)); !os.IsNotExist(err) {
			t.Errorf("%s was downloaded, want only the matching quantization fetched", unwanted)
		}
	}

	final := calls()[len(calls())-1]
	wantTotal := int64(len(files["llama-2-7b.Q4_K_M.gguf"]))
	if final.status != StatusCompleted || final.bytesTotal != wantTotal {
		t.Errorf("final progress = %+v, want status=%q bytesTotal=%d", final, StatusCompleted, wantTotal)
	}
}

func TestExecutor_Download_QuantizationNoMatch_FailsFastWithAvailableFiles(t *testing.T) {
	files := map[string][]byte{
		"llama-2-7b.Q4_K_M.gguf": []byte("q4 content"),
		"llama-2-7b.Q5_K_M.gguf": []byte("q5 content"),
	}
	hub := newFakeHubServer("test-org/multi-quant", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, calls := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	err := e.Download(context.Background(), "test-org/multi-quant", "Q8_0", destDir, progress)
	if err == nil {
		t.Fatal("Download() succeeded for a quantization matching no file, want an error")
	}
	if !strings.Contains(err.Error(), "Q4_K_M") || !strings.Contains(err.Error(), "Q5_K_M") {
		t.Errorf("error = %v, want it to list the repo's actual .gguf filenames", err)
	}

	// Fails before ever creating destDir/downloading anything.
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Errorf("destDir has %d entries, want 0 - a no-match failure should download nothing", len(entries))
	}
	if len(calls()) != 1 || calls()[0].status != StatusFailed {
		t.Errorf("progress calls = %+v, want exactly one StatusFailed call", calls())
	}
}

func TestExecutor_Download_QuantizationAmbiguousMatch_Fails(t *testing.T) {
	files := map[string][]byte{
		"llama-2-7b.Q4_K_M.gguf":    []byte("q4 content"),
		"llama-2-7b.Q4_K_M-v2.gguf": []byte("q4 v2 content"),
	}
	hub := newFakeHubServer("test-org/ambiguous-quant", files)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	destDir := t.TempDir()
	progress, _ := collectProgress()
	e := newTestExecutor(srv, defaultProgressBytes)

	err := e.Download(context.Background(), "test-org/ambiguous-quant", "Q4_K_M", destDir, progress)
	if err == nil {
		t.Fatal("Download() succeeded for an ambiguous quantization match, want an error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v, want it to mention the match is ambiguous", err)
	}
}

func TestSelectQuantizedFile(t *testing.T) {
	files := []string{"README.md", "config.json", "model.Q4_K_M.gguf", "model.Q5_K_M.gguf"}

	t.Run("exactly one match", func(t *testing.T) {
		got, err := selectQuantizedFile(files, "Q4_K_M")
		if err != nil {
			t.Fatalf("selectQuantizedFile() error: %v", err)
		}
		if len(got) != 1 || got[0] != "model.Q4_K_M.gguf" {
			t.Errorf("selectQuantizedFile() = %v, want [model.Q4_K_M.gguf]", got)
		}
	})

	t.Run("no match lists available .gguf files only, not every file", func(t *testing.T) {
		_, err := selectQuantizedFile(files, "Q8_0")
		if err == nil {
			t.Fatal("selectQuantizedFile() succeeded, want an error")
		}
		if strings.Contains(err.Error(), "README.md") || strings.Contains(err.Error(), "config.json") {
			t.Errorf("error = %v, want it to list only .gguf filenames, not every repo file", err)
		}
		if !strings.Contains(err.Error(), "model.Q4_K_M.gguf") || !strings.Contains(err.Error(), "model.Q5_K_M.gguf") {
			t.Errorf("error = %v, want it to list both available .gguf files", err)
		}
	})

	t.Run("substring match against a non-gguf file never matches", func(t *testing.T) {
		filesWithReadmeMention := []string{"README.md mentioning Q4_K_M", "model.Q4_K_M.gguf"}
		got, err := selectQuantizedFile(filesWithReadmeMention, "Q4_K_M")
		if err != nil {
			t.Fatalf("selectQuantizedFile() error: %v", err)
		}
		if len(got) != 1 || got[0] != "model.Q4_K_M.gguf" {
			t.Errorf("selectQuantizedFile() = %v, want only the .gguf file, not the non-.gguf one containing the same substring", got)
		}
	})
}
