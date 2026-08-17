// SPDX-License-Identifier: AGPL-3.0-or-later

// Package transfer is sparky-agent's Transfer Executor - see
// docs/AGENT.md Service Architecture Notes: "one per active transfer, so
// a long-running download or rsync replication never blocks command
// handling. Progress is streamed back periodically, not just on
// completion." Phase 3 of the Model transfers work (PLANNING.md):
// downloads a Hugging Face model repository over plain net/http, no
// external tool dependency - no Python/huggingface_hub, no git/git-lfs -
// see PLANNING.md's Decisions Log for why. This package knows nothing
// about the agent protocol or the WebSocket connection; agent/connection
// is what turns its ProgressFunc calls into TypeTransferProgress
// messages.
package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const defaultBaseURL = "https://huggingface.co"

// defaultRevision is used for every download - v0.1.0 doesn't support
// pinning a revision (SCHEMA.md's model_ref doc comment: "ideally with a
// pinned revision", not yet enforced or threaded through).
const defaultRevision = "main"

// defaultProgressBytes is how many bytes must be written before a
// progress update is reported again - keeps updates periodic without
// flooding the central app (and the database write behind it) on every
// small read, matching docs/AGENT.md's "streamed periodically, not just
// on completion", not "streamed on every read."
const defaultProgressBytes = 4 * 1024 * 1024 // 4 MiB

// downloadChunkSize is how much is read from a response body at a time.
const downloadChunkSize = 32 * 1024

// Status values a ProgressFunc may report. These are plain strings, not
// internal/db.TransferStatus, deliberately - this package (like
// internal/agentproto, which its wire values must keep matching) has no
// dependency on internal/db; sparky-agent has no database access at all.
// Only the three states an executor can actually reach are defined here -
// StatusQueued and StatusCancelled are meaningful to the central app but
// never produced by this package.
const (
	StatusTransferring = "transferring"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
)

// ProgressFunc is called as a download proceeds: once at the very start
// (StatusTransferring, BytesTransferred 0), periodically as bytes move
// (throttled by Executor's progress-bytes threshold), and exactly once
// more at the end - StatusCompleted, or StatusFailed with ErrorMessage
// set.
type ProgressFunc func(bytesTransferred, bytesTotal int64, status, errMsg string)

// Executor downloads a Hugging Face model repository to local disk.
type Executor struct {
	baseURL       string
	client        *http.Client
	progressBytes int64
	chunkBytes    int
}

// New constructs an Executor against the real Hugging Face Hub.
func New() *Executor {
	return &Executor{
		baseURL:       defaultBaseURL,
		client:        http.DefaultClient,
		progressBytes: defaultProgressBytes,
		chunkBytes:    downloadChunkSize,
	}
}

type repoInfo struct {
	Siblings []struct {
		RFilename string `json:"rfilename"`
	} `json:"siblings"`
}

// Download fetches modelRef's default revision into destDir (created if
// it doesn't exist, along with any subdirectory a filename implies - a
// repo's siblings entries may nest, e.g. "onnx/model.onnx"). When
// quantization is empty, every file in the repo is fetched - correct and
// necessary for a vLLM/full-residency profile (the whole HF
// Transformers-format directory is required anyway) and harmless for a
// single-file GGUF repo, but wasteful for a multi-quantization GGUF repo.
// When quantization is non-empty, only the one file whose name contains
// it is fetched - see selectQuantizedFile for the matching/error rules.
//
// A file already fully present in destDir at the remote's exact size is
// skipped. A partially present file is resumed via a Range request when
// the server honors it. Some Hugging Face-served files advertise
// Accept-Ranges but silently ignore the header - observed for small,
// non-LFS files served directly from huggingface.co, as opposed to large
// LFS files, which redirect to a CDN that does honor Range (confirmed
// against a real repo, not assumed - see PLANNING.md's Decisions Log).
// A 200 response to a Range request is therefore treated as "the server
// is sending the whole file, start over," never appended to blindly - a
// 200 with a Range header in flight would otherwise silently corrupt the
// file by appending the full body after the existing partial one.
//
// No checksum verification is performed - SCHEMA.md's node_model_inventory
// has no field for one yet, so there is nothing to verify against.
func (e *Executor) Download(ctx context.Context, modelRef, quantization, destDir string, progress ProgressFunc) error {
	files, err := e.listFiles(ctx, modelRef)
	if err != nil {
		progress(0, 0, StatusFailed, err.Error())
		return fmt.Errorf("list files for %s: %w", modelRef, err)
	}

	if quantization != "" {
		files, err = selectQuantizedFile(files, quantization)
		if err != nil {
			progress(0, 0, StatusFailed, err.Error())
			return fmt.Errorf("select quantization %q for %s: %w", quantization, modelRef, err)
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		progress(0, 0, StatusFailed, err.Error())
		return fmt.Errorf("create destination directory: %w", err)
	}

	sizes := make([]int64, len(files))
	var bytesTotal int64
	for i, f := range files {
		size, err := e.fileSize(ctx, modelRef, f)
		if err != nil {
			progress(0, 0, StatusFailed, err.Error())
			return fmt.Errorf("determine size of %s: %w", f, err)
		}
		sizes[i] = size
		bytesTotal += size
	}

	var bytesTransferred int64
	progress(bytesTransferred, bytesTotal, StatusTransferring, "")

	for i, f := range files {
		fileTransferredBefore := bytesTransferred
		n, err := e.downloadFile(ctx, modelRef, f, destDir, sizes[i], fileTransferredBefore, bytesTotal, progress)
		bytesTransferred += n
		if err != nil {
			progress(bytesTransferred, bytesTotal, StatusFailed, err.Error())
			return fmt.Errorf("download %s: %w", f, err)
		}
	}

	progress(bytesTransferred, bytesTotal, StatusCompleted, "")
	return nil
}

func (e *Executor) listFiles(ctx context.Context, modelRef string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/models/%s", e.baseURL, modelRef), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d listing %s", resp.StatusCode, modelRef)
	}

	var info repoInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode repo info: %w", err)
	}

	files := make([]string, len(info.Siblings))
	for i, s := range info.Siblings {
		files[i] = s.RFilename
	}
	return files, nil
}

// selectQuantizedFile narrows files down to the single .gguf file whose
// name contains quantization, e.g. "Q4_K_M" matching
// "llama-2-7b.Q4_K_M.gguf" - llama.cpp's own naming convention. Restricted
// to .gguf files specifically (not just any filename containing the
// substring) so an unrelated file that happens to mention the quant label
// - a README, for instance - can never match. Returns a clear error
// listing the repo's actual .gguf filenames when nothing matches (no live
// repo-check UI exists yet - see PLANNING.md's Decisions Log - so this is
// the fast-failure safety net a bad or misspelled quantization value gets
// instead), or when more than one file matches (ambiguous).
func selectQuantizedFile(files []string, quantization string) ([]string, error) {
	var ggufFiles, matches []string
	for _, f := range files {
		if !strings.HasSuffix(strings.ToLower(f), ".gguf") {
			continue
		}
		ggufFiles = append(ggufFiles, f)
		if strings.Contains(f, quantization) {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no .gguf file matched, available: %s", strings.Join(ggufFiles, ", "))
	case 1:
		return matches, nil
	default:
		return nil, fmt.Errorf("ambiguous, matched: %s", strings.Join(matches, ", "))
	}
}

// fileSize issues a HEAD request against the resolve endpoint to learn a
// file's size before downloading it - confirmed against a real repo to
// return the same Content-Length a GET would (see PLANNING.md's Decisions
// Log), letting Download report an accurate BytesTotal from the first
// progress call rather than only discovering it file by file.
func (e *Executor) fileSize(ctx context.Context, modelRef, rfilename string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, e.resolveURL(modelRef, rfilename), nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// downloadFile fetches one file into destDir, returning the file's total
// size on disk when it returns (regardless of how much of that was
// already present before this call) so Download's running total stays
// accurate even when a file was skipped or resumed.
func (e *Executor) downloadFile(ctx context.Context, modelRef, rfilename, destDir string, remoteSize, transferredBefore, bytesTotal int64, progress ProgressFunc) (int64, error) {
	localPath := filepath.Join(destDir, filepath.FromSlash(rfilename))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return 0, fmt.Errorf("create parent directory: %w", err)
	}

	var offset int64
	if info, statErr := os.Stat(localPath); statErr == nil {
		switch {
		case info.Size() == remoteSize:
			return remoteSize, nil
		case info.Size() < remoteSize:
			offset = info.Size()
		}
		// info.Size() > remoteSize shouldn't happen in practice (nothing
		// else writes to destDir), but if it did, offset stays 0 and the
		// file is simply re-downloaded from scratch below.
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.resolveURL(modelRef, rfilename), nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	var out *os.File
	switch resp.StatusCode {
	case http.StatusPartialContent:
		out, err = os.OpenFile(localPath, os.O_WRONLY|os.O_APPEND, 0o644)
	case http.StatusOK:
		// Either a fresh download (offset == 0) or the server ignored our
		// Range header (offset > 0) - either way, the response body is the
		// whole file, so start over rather than append.
		offset = 0
		out, err = os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	default:
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", localPath, err)
	}
	defer out.Close()

	buf := make([]byte, e.chunkBytes)
	fileWritten := offset
	var sinceLastReport int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fileWritten, fmt.Errorf("write: %w", writeErr)
			}
			fileWritten += int64(n)
			sinceLastReport += int64(n)
			if sinceLastReport >= e.progressBytes {
				progress(transferredBefore+fileWritten, bytesTotal, StatusTransferring, "")
				sinceLastReport = 0
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fileWritten, fmt.Errorf("read: %w", readErr)
		}
	}

	return fileWritten, nil
}

// resolveURL builds a Hugging Face resolve URL, path-escaping each
// filename segment individually - rfilename may contain '/' for a nested
// path (e.g. "onnx/model.onnx"), which must stay as path separators, not
// be escaped into "%2F".
func (e *Executor) resolveURL(modelRef, rfilename string) string {
	segments := strings.Split(rfilename, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", e.baseURL, modelRef, defaultRevision, strings.Join(segments, "/"))
}
