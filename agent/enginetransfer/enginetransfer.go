// SPDX-License-Identifier: AGPL-3.0-or-later

// Package enginetransfer is sparky-agent's Engine Transfer Executor -
// downloads, checksum-verifies, and installs a maintainer-built compiled
// engine binary release (llama.cpp today) from GitHub Releases into a
// versioned install directory, mirroring agent/transfer's Transfer
// Executor pattern (docs/AGENT.md Service Architecture Notes: "one per
// active transfer... progress is streamed back periodically") - see
// PLANNING.md's 2026-08-15 Decisions Log entry. This package knows nothing
// about the agent protocol or the WebSocket connection; agent/connection is
// what turns its ProgressFunc calls into TypeEngineTransferProgress
// messages.
package enginetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// releaseOwner/releaseRepo identify where Sparky's own maintainer-built
// engine release tarballs are hosted - the main Sparky repo's GitHub
// Releases to start, matching the already-established "raw artifacts now,
// dedicated repo later if warranted" precedent from agent packaging (see
// PLANNING.md's 2026-08-13 Decisions Log entry) - exactly mirroring how
// agent/transfer.New() hardcodes its own defaultBaseURL today. Migrating to
// a dedicated repo later needs only these two constants to change.
const (
	releaseOwner = "1kaius1"
	releaseRepo  = "Sparky"
)

// defaultProgressBytes is how many bytes must be written before a progress
// update is reported again during download - same threshold and reasoning
// as agent/transfer's identically-named constant.
const defaultProgressBytes = 4 * 1024 * 1024 // 4 MiB

// downloadChunkSize is how much is read from a response body at a time.
const downloadChunkSize = 32 * 1024

// Status values a ProgressFunc may report - the same three reachable
// states as agent/transfer's Status* constants, for the same reason (this
// package has no dependency on internal/db).
const (
	StatusTransferring = "transferring"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
)

// Progress is passed to ProgressFunc at each step of a provisioning run.
// This is a struct rather than agent/transfer.ProgressFunc's plain
// positional arguments (the pattern this package otherwise mirrors)
// because a completed run reports two additional pieces of information
// that pattern has no room for: InstallPath and InstalledSizeBytes are
// populated only when Status is StatusCompleted (mirroring how
// ErrorMessage is populated only on StatusFailed) - see
// internal/agentproto.EngineTransferProgress's matching doc comment for
// why InstalledSizeBytes is not simply BytesTotal (the compressed download
// size) reused, the way Model transfers reuses its own BytesTotal.
type Progress struct {
	BytesTransferred   int64
	BytesTotal         int64
	Status             string
	ErrorMessage       string
	InstallPath        string
	InstalledSizeBytes int64
}

// ProgressFunc is called as a provisioning run proceeds: once at the very
// start (StatusTransferring, BytesTransferred 0), periodically as the
// tarball downloads (throttled by Executor's progress-bytes threshold), and
// exactly once more at the end - StatusCompleted with InstallPath/
// InstalledSizeBytes set, or StatusFailed with ErrorMessage set.
type ProgressFunc func(Progress)

// Executor downloads, verifies, and installs a compiled-engine release
// tarball.
type Executor struct {
	baseURL       string
	client        *http.Client
	progressBytes int64
	chunkBytes    int
	run           runner
}

// runner is a fakeable seam for shelling out to `tar` - same pattern as
// agent/provision's identically-named type. Extraction shells out rather
// than using a Go xz decompression library because compress/* has no xz
// support and every bare-metal target already has `tar` - see PLANNING.md's
// 2026-08-15 Decisions Log entry on why this avoids a new Go dependency.
type runner func(ctx context.Context, name string, args ...string) error

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultBaseURL is the real GitHub Releases download endpoint for
// releaseOwner/releaseRepo - a field on Executor rather than inlined
// directly into releaseAssetURL, the same shape as agent/transfer.Executor's
// own baseURL field, so a test can point it at an httptest.Server instead.
var defaultBaseURL = fmt.Sprintf("https://github.com/%s/%s/releases/download", releaseOwner, releaseRepo)

// New constructs an Executor against the real GitHub Releases endpoint.
func New() *Executor {
	return &Executor{
		baseURL:       defaultBaseURL,
		client:        http.DefaultClient,
		progressBytes: defaultProgressBytes,
		chunkBytes:    downloadChunkSize,
		run:           runCommand,
	}
}

// assetName builds the release asset filename for engineType/version on
// this agent's own architecture - $ENGINE-$VERSION-$ARCH.tar.xz, per the
// maintainer-controlled convention recorded in PLANNING.md's 2026-08-15
// Decisions Log entry. There is no arch parameter - only the agent knows
// (and needs to know) its own runtime.GOARCH; the central app never picks
// an asset on the agent's behalf.
func assetName(engineType, version string) string {
	return fmt.Sprintf("%s-%s-%s.tar.xz", engineType, version, runtime.GOARCH)
}

func (e *Executor) releaseAssetURL(version, asset string) string {
	return fmt.Sprintf("%s/%s/%s", e.baseURL, version, asset)
}

// Provision downloads engineType's version release tarball, verifies it
// against its sibling .sha256 checksum file, and installs it into
// installRoot/<engineType>/<version>/ - creating installRoot/<engineType>
// if it doesn't exist. On success, atomically repoints
// installRoot/<engineType>/latest at the newly-installed version and
// returns the absolute install path and its on-disk size. A prior install
// of the same version is replaced; a different version's own directory is
// left untouched - multiple versions are meant to coexist side by side, see
// PLANNING.md's 2026-08-15 Decisions Log entry on the deferred per-profile
// engine_version pinning follow-up this layout is designed to support.
func (e *Executor) Provision(ctx context.Context, engineType, version, installRoot string, progress ProgressFunc) (installPath string, installedSizeBytes int64, err error) {
	asset := assetName(engineType, version)
	engineDir := filepath.Join(installRoot, engineType)

	fail := func(bytesTransferred, bytesTotal int64, cause error) (string, int64, error) {
		progress(Progress{BytesTransferred: bytesTransferred, BytesTotal: bytesTotal, Status: StatusFailed, ErrorMessage: cause.Error()})
		return "", 0, cause
	}

	if err := os.MkdirAll(engineDir, 0o755); err != nil {
		return fail(0, 0, fmt.Errorf("create engine install directory %s: %w", engineDir, err))
	}

	wantHash, err := e.fetchChecksum(ctx, e.releaseAssetURL(version, asset+".sha256"))
	if err != nil {
		return fail(0, 0, fmt.Errorf("fetch checksum for %s: %w", asset, err))
	}

	tarballPath, bytesTotal, err := e.downloadTarball(ctx, engineDir, e.releaseAssetURL(version, asset), progress)
	if err != nil {
		return fail(0, bytesTotal, fmt.Errorf("download %s: %w", asset, err))
	}
	defer os.Remove(tarballPath)

	gotHash, err := sha256File(tarballPath)
	if err != nil {
		return fail(bytesTotal, bytesTotal, fmt.Errorf("checksum %s: %w", asset, err))
	}
	if !strings.EqualFold(gotHash, wantHash) {
		return fail(bytesTotal, bytesTotal, fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, gotHash, wantHash))
	}

	extractDir, err := os.MkdirTemp(engineDir, ".extract-*")
	if err != nil {
		return fail(bytesTotal, bytesTotal, fmt.Errorf("create extraction directory: %w", err))
	}
	if err := e.run(ctx, "tar", "-xJf", tarballPath, "-C", extractDir); err != nil {
		os.RemoveAll(extractDir)
		return fail(bytesTotal, bytesTotal, fmt.Errorf("extract %s: %w", asset, err))
	}

	versionDir := filepath.Join(engineDir, version)
	if err := os.RemoveAll(versionDir); err != nil {
		os.RemoveAll(extractDir)
		return fail(bytesTotal, bytesTotal, fmt.Errorf("remove existing install at %s: %w", versionDir, err))
	}
	if err := os.Rename(extractDir, versionDir); err != nil {
		os.RemoveAll(extractDir)
		return fail(bytesTotal, bytesTotal, fmt.Errorf("install %s to %s: %w", asset, versionDir, err))
	}

	size, err := dirSize(versionDir)
	if err != nil {
		return fail(bytesTotal, bytesTotal, fmt.Errorf("measure installed size of %s: %w", versionDir, err))
	}

	if err := swapLatestSymlink(engineDir, version); err != nil {
		return fail(bytesTotal, bytesTotal, fmt.Errorf("update latest symlink for %s: %w", engineType, err))
	}

	progress(Progress{
		BytesTransferred: bytesTotal, BytesTotal: bytesTotal, Status: StatusCompleted,
		InstallPath: versionDir, InstalledSizeBytes: size,
	})
	return versionDir, size, nil
}

// fetchChecksum downloads and parses a sha256sum-style checksum file
// (a single hex hash, optionally followed by whitespace and a filename -
// the standard `sha256sum` output format), returning just the hash.
func (e *Executor) fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read checksum body: %w", err)
	}

	hash, _, _ := strings.Cut(strings.TrimSpace(string(body)), " ")
	hash = strings.TrimSpace(hash)
	if len(hash) != 64 {
		return "", fmt.Errorf("malformed checksum file at %s: %q", url, string(body))
	}
	return hash, nil
}

// downloadTarball fetches the release tarball into a fresh, uniquely-named
// temporary file under engineDir, reporting throttled progress as it goes.
// Unlike agent/transfer's downloadFile, there is no Range-based resume
// support across separate Provision calls - a deliberate simplification:
// engine release tarballs are far smaller than the multi-gigabyte model
// repositories agent/transfer was built for, so interruption-safety across
// retries matters far less here, and a fresh temp file per attempt avoids
// any risk of two concurrent provisioning runs colliding on the same path.
func (e *Executor) downloadTarball(ctx context.Context, engineDir, url string, progress ProgressFunc) (path string, bytesTotal int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}
	bytesTotal = resp.ContentLength

	out, err := os.CreateTemp(engineDir, ".download-*.tar.xz")
	if err != nil {
		return "", bytesTotal, fmt.Errorf("create temp file: %w", err)
	}
	defer out.Close()

	progress(Progress{BytesTransferred: 0, BytesTotal: bytesTotal, Status: StatusTransferring})

	buf := make([]byte, e.chunkBytes)
	var written, sinceLastReport int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				os.Remove(out.Name())
				return "", bytesTotal, fmt.Errorf("write: %w", writeErr)
			}
			written += int64(n)
			sinceLastReport += int64(n)
			if sinceLastReport >= e.progressBytes {
				progress(Progress{BytesTransferred: written, BytesTotal: bytesTotal, Status: StatusTransferring})
				sinceLastReport = 0
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			os.Remove(out.Name())
			return "", bytesTotal, fmt.Errorf("read: %w", readErr)
		}
	}

	if bytesTotal <= 0 {
		bytesTotal = written
	}
	return out.Name(), bytesTotal, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dirSize sums the size of every regular file under root - the installed
// on-disk footprint reported back as InstalledSizeBytes.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// swapLatestSymlink atomically repoints engineDir/latest at version - a
// temporary symlink is created and then renamed over the real path, since a
// bare remove-then-create is not atomic and would leave a brief window
// with no latest symlink at all. The link target is relative (just
// version, not an absolute path) so the whole engines directory tree
// remains portable if SPARKY_ENGINE_INSTALL_PATH is ever moved.
func swapLatestSymlink(engineDir, version string) error {
	tmp, err := os.MkdirTemp(engineDir, ".latest-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.RemoveAll(tmp); err != nil { // MkdirTemp only to get a unique, non-colliding name
		return fmt.Errorf("reserve temp symlink path: %w", err)
	}
	if err := os.Symlink(version, tmp); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(engineDir, "latest")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("swap latest symlink: %w", err)
	}
	return nil
}
