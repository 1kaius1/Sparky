// SPDX-License-Identifier: AGPL-3.0-or-later

// Package modelsource estimates how big a model download will be before it
// starts, by asking the source's own public API for per-file sizes. It is
// best-effort by design: any failure degrades to "unknown" and never blocks
// a transfer.
//
// Only Hugging Face is supported, because it is the only source the agent
// can actually download from today (agent/transfer). Estimating a source
// Sparky cannot fetch would only mislead; the provider is detected from the
// model_ref itself so another source slots in here when the agent gains it.
//
// This is the central app making an outbound request whose path is
// influenced by user input, so the host is fixed (never taken from
// model_ref) and model_ref is validated to a strict repo-path shape before
// it is used at all.
package modelsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://huggingface.co"
	requestTimeout = 8 * time.Second
	maxPages       = 10
	maxBodyBytes   = 8 << 20
)

// ErrUnsupported is returned for a model_ref that is not a Hugging Face repo
// path (the only source supported).
var ErrUnsupported = errors.New("size estimation is not supported for this model source")

// ErrUnknown is returned when the source could not tell us a size - a
// network failure, a missing repo, or (with a quantization) no matching
// file.
var ErrUnknown = errors.New("model size unknown")

// repoPattern is a Hugging Face "org/repo" path: exactly one slash, each
// side alphanumeric-led with only [A-Za-z0-9._-] after. Nothing that could
// change the request's host, path structure, or query.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// ValidRepo reports whether ref is a well-formed Hugging Face repo path.
func ValidRepo(ref string) bool { return repoPattern.MatchString(ref) }

// Estimator queries a model source's public API.
type Estimator struct {
	client  *http.Client
	baseURL string
}

// New returns an Estimator for the real Hugging Face API.
func New() *Estimator {
	return &Estimator{client: &http.Client{Timeout: requestTimeout}, baseURL: defaultBaseURL}
}

type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs"`
}

// EstimateSize returns the number of bytes a download of modelRef would
// transfer, from the repo's default branch - what agent/transfer downloads.
// A non-empty quantization restricts the estimate to the .gguf file(s)
// whose name contains it, matching agent/transfer's own selection; with a
// quantization and no matching file the result is ErrUnknown. LFS files are
// sized by their real (LFS) size, not the pointer file's.
func (e *Estimator) EstimateSize(ctx context.Context, modelRef, quantization string) (int64, error) {
	if !ValidRepo(modelRef) {
		return 0, ErrUnsupported
	}
	next := e.baseURL + "/api/models/" + modelRef + "/tree/main?recursive=true"
	baseHost := mustHost(e.baseURL)

	var total int64
	matched := 0
	for page := 0; page < maxPages && next != ""; page++ {
		entries, link, err := e.fetchPage(ctx, next)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrUnknown, err)
		}
		for _, en := range entries {
			if en.Type != "file" {
				continue
			}
			if quantization != "" {
				lower := strings.ToLower(en.Path)
				if !strings.HasSuffix(lower, ".gguf") || !strings.Contains(en.Path, quantization) {
					continue
				}
			}
			size := en.Size
			if en.LFS != nil && en.LFS.Size > 0 {
				size = en.LFS.Size
			}
			total += size
			matched++
		}
		// Only follow a next link that stays on the configured host - the
		// response must not be able to redirect this request elsewhere.
		next = ""
		if link != "" {
			if u, err := url.Parse(link); err == nil && u.Scheme+"://"+u.Host == baseHost {
				next = link
			}
		}
	}
	if quantization != "" && matched == 0 {
		return 0, fmt.Errorf("%w: no .gguf file matches %q", ErrUnknown, quantization)
	}
	return total, nil
}

func (e *Estimator) fetchPage(ctx context.Context, pageURL string) ([]treeEntry, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var entries []treeEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&entries); err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	return entries, nextLink(resp.Header.Get("Link")), nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(header string) string {
	if m := linkNext.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

func mustHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
