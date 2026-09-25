// SPDX-License-Identifier: AGPL-3.0-or-later

package modelsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestEstimator(t *testing.T, h http.Handler) *Estimator {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Estimator{client: srv.Client(), baseURL: srv.URL}
}

func TestEstimateSize_SumsFilesAndPrefersLFSSize(t *testing.T) {
	var gotPath string
	e := newTestEstimator(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		fmt.Fprint(w, `[
		 {"type":"directory","path":"sub"},
		 {"type":"file","path":"config.json","size":100},
		 {"type":"file","path":"model.safetensors","size":134,"lfs":{"size":5000}},
		 {"type":"file","path":"sub/tok.json","size":50}]`)
	}))
	got, err := e.EstimateSize(context.Background(), "org/model", "")
	if err != nil || got != 5150 {
		t.Errorf("EstimateSize = %d, %v; want 5150 (100 + LFS 5000 + 50, directory ignored)", got, err)
	}
	if gotPath != "/api/models/org/model/tree/main?recursive=true" {
		t.Errorf("request = %q", gotPath)
	}
}

func TestEstimateSize_QuantizationCountsOnlyMatchingGGUF(t *testing.T) {
	e := newTestEstimator(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"type":"file","path":"m-Q4_K_M.gguf","size":1,"lfs":{"size":4000}},
		 {"type":"file","path":"m-Q8_0.gguf","size":1,"lfs":{"size":8000}},
		 {"type":"file","path":"README.md-Q4_K_M","size":9},
		 {"type":"file","path":"m-Q4_K_M-extra.bin","size":7}]`)
	}))
	got, err := e.EstimateSize(context.Background(), "org/model", "Q4_K_M")
	if err != nil || got != 4000 {
		t.Errorf("EstimateSize = %d, %v; want 4000 (only the matching .gguf)", got, err)
	}
	if _, err := e.EstimateSize(context.Background(), "org/model", "Q2_K"); !errors.Is(err, ErrUnknown) {
		t.Errorf("no match error = %v, want ErrUnknown", err)
	}
}

func TestEstimateSize_FollowsSameHostPagination(t *testing.T) {
	var calls atomic.Int32
	var srvURL string
	e := newTestEstimator(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Link", `<`+srvURL+`/api/models/org/model/tree/main?recursive=true&cursor=abc>; rel="next"`)
			fmt.Fprint(w, `[{"type":"file","path":"a","size":10}]`)
			return
		}
		fmt.Fprint(w, `[{"type":"file","path":"b","size":32}]`)
	}))
	srvURL = e.baseURL
	got, err := e.EstimateSize(context.Background(), "org/model", "")
	if err != nil || got != 42 || calls.Load() != 2 {
		t.Errorf("EstimateSize = %d, %v after %d calls; want 42 over 2 pages", got, err, calls.Load())
	}
}

func TestEstimateSize_NeverFollowsALinkToAnotherHost(t *testing.T) {
	var evilHit atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHit.Store(true)
		fmt.Fprint(w, `[]`)
	}))
	defer evil.Close()
	e := newTestEstimator(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+evil.URL+`/steal>; rel="next"`)
		fmt.Fprint(w, `[{"type":"file","path":"a","size":10}]`)
	}))
	got, err := e.EstimateSize(context.Background(), "org/model", "")
	if err != nil || got != 10 {
		t.Errorf("EstimateSize = %d, %v", got, err)
	}
	if evilHit.Load() {
		t.Error("the estimator followed a pagination link to a different host")
	}
}

func TestEstimateSize_Failures(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"404":      func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"500":      func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) },
		"not json": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") },
	} {
		e := newTestEstimator(t, h)
		if _, err := e.EstimateSize(context.Background(), "org/model", ""); !errors.Is(err, ErrUnknown) {
			t.Errorf("%s: error = %v, want ErrUnknown", name, err)
		}
	}
}

func TestEstimateSize_RejectsUnsupportedOrHostileRefsWithoutAnyRequest(t *testing.T) {
	var hits atomic.Int32
	e := newTestEstimator(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	for _, ref := range []string{
		"", "single", "a/b/c", "../etc/passwd", "org/../x", "org/model?x=1", "org/model#f", "org/mo del",
		"https://civitai.com/models/1", "//evil.example.com/x", "org/model/", "/org/model", "org\\model",
		"org/model\nHost: evil", "-org/model", strings.Repeat("a", 200) + "/m",
	} {
		if _, err := e.EstimateSize(context.Background(), ref, ""); !errors.Is(err, ErrUnsupported) {
			t.Errorf("EstimateSize(%q) error = %v, want ErrUnsupported", ref, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d request(s) were made for refs that must be rejected up front", hits.Load())
	}
}

func TestValidRepo(t *testing.T) {
	for _, ok := range []string{"meta-llama/Llama-3-8B", "RedHatAI/Qwen2-0.5B-Instruct-FP8", "TheBloke/x_y.z-GGUF"} {
		if !ValidRepo(ok) {
			t.Errorf("ValidRepo(%q) = false", ok)
		}
	}
}
