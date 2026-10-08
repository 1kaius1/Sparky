// SPDX-License-Identifier: AGPL-3.0-or-later

// Package modelscan finds model copies sitting in a node's model storage
// directory, so an operator (or the central app, via the scan_models
// command) can see what is on disk regardless of whether the central app
// has an inventory row for it. It is read-only and has no knowledge of the
// central app's inventory - deciding what is "unknown" is the server's job.
//
// Everything here is inferred from file names and sizes. The agent writes
// no marker or metadata files next to a model, and a partly downloaded file
// sits under its final name, so a scan can neither prove a copy is complete
// nor read its true quantization. Callers treat a Candidate as a best guess
// for a human to confirm, never as authoritative.
package modelscan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/agent/modelpath"
)

const (
	// FormatSafetensors and FormatGGUF match the model_format enum values
	// (SCHEMA.md Node model inventory's format).
	FormatSafetensors = "safetensors"
	FormatGGUF        = "gguf"

	// UnknownQuantization is the existing inventory sentinel for "a real
	// quantization exists but could not be determined" (SCHEMA.md Node model
	// inventory). A whole-repo safetensors copy uses "" instead, matching
	// what a download of that repo records.
	UnknownQuantization = "UNKNOWN"

	// MaxCandidates bounds a scan's result so a node pointed at an unrelated
	// large tree cannot produce an unbounded protocol message.
	MaxCandidates = 500

	// maxDepth bounds directory recursion below the storage root. A Hugging
	// Face repo path is two levels (org/name); the extra headroom covers
	// operator-chosen layouts without letting a deep tree run away.
	maxDepth = 6
)

// Candidate is one model copy found on disk.
type Candidate struct {
	// ModelRef is the path relative to the storage root with forward
	// slashes, the same shape as a Hugging Face repo path.
	ModelRef     string `json:"model_ref"`
	Quantization string `json:"quantization"`
	Format       string `json:"format"`
	// SizeBytes is the on-disk size of just this candidate's files.
	SizeBytes int64 `json:"size_bytes"`
	// FileName is the first .gguf file of a gguf candidate (the shard with
	// index 1 for a split model); empty for safetensors.
	FileName string `json:"file_name,omitempty"`
	// PossiblyIncomplete is set when the model directory holds a leftover
	// rsync temp file, which an interrupted peer copy leaves behind.
	PossiblyIncomplete bool `json:"possibly_incomplete,omitempty"`
}

// rsyncTempRe matches the hidden temp name rsync writes while receiving a
// file (".<name>.XXXXXX"). Only weight-file temps count: HF repos
// legitimately contain other dot-files (.gitattributes), which must not
// look like an interrupted copy.
var rsyncTempRe = regexp.MustCompile(`^\.(.+\.(?:gguf|safetensors))\.[A-Za-z0-9]{6}$`)

// shardRe matches a split gguf file ("<base>-00001-of-00003.gguf").
var shardRe = regexp.MustCompile(`^(.+)-(\d{5})-of-(\d{5})\.gguf$`)

// quantRe pulls a quantization label out of a gguf file's base name. The
// matched text keeps the file's own casing, because the download and delete
// paths locate a quantized file by "file name contains the quantization
// string" - a label that did not appear verbatim would never find its file.
var quantRe = regexp.MustCompile(`(?i)(?:^|[-_.])((?:IQ|Q)\d(?:_[A-Z0-9]+)*|BF16|FP16|F16|F32|MXFP4|NVFP4|FP8)(?:$|[-_.])`)

// Scan walks root and returns the model copies it finds, sorted by model
// ref then quantization. truncated is true when MaxCandidates was reached
// and the listing is incomplete. A subdirectory that cannot be read is
// skipped rather than failing the scan; only an unusable root is an error.
func Scan(root string) (candidates []Candidate, truncated bool, err error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, false, fmt.Errorf("model storage %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("model storage %q is not a directory", root)
	}

	s := &scanner{root: root}
	if err := s.walk("", 0); err != nil {
		return nil, false, err
	}
	sort.Slice(s.out, func(i, j int) bool {
		if s.out[i].ModelRef != s.out[j].ModelRef {
			return s.out[i].ModelRef < s.out[j].ModelRef
		}
		if s.out[i].Format != s.out[j].Format {
			return s.out[i].Format < s.out[j].Format
		}
		return s.out[i].Quantization < s.out[j].Quantization
	})
	return s.out, s.truncated, nil
}

type scanner struct {
	root      string
	out       []Candidate
	truncated bool
}

func (s *scanner) full() bool {
	if len(s.out) >= MaxCandidates {
		s.truncated = true
		return true
	}
	return false
}

// walk visits the directory at rel (relative to the root, "" for the root
// itself). A directory that directly holds weight files is a model
// directory and is not descended into: its subfolders (onnx/, tokenizer
// assets) belong to that model, not to another one. Symlinks are never
// followed - os.DirEntry.IsDir is false for a symlink to a directory - so a
// link cannot lead the scan, or a later import, outside the storage root.
func (s *scanner) walk(rel string, depth int) error {
	entries, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(rel)))
	if err != nil {
		if rel == "" {
			return fmt.Errorf("read model storage %q: %w", s.root, err)
		}
		return nil
	}

	// The root itself is never a model: an empty model_ref is not valid.
	if rel != "" && hasWeights(entries) {
		s.addModelDir(rel, entries)
		return nil
	}
	if depth >= maxDepth {
		return nil
	}
	for _, e := range entries {
		if s.full() {
			return nil
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		child := e.Name()
		if rel != "" {
			child = rel + "/" + e.Name()
		}
		if err := s.walk(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func hasWeights(entries []os.DirEntry) bool {
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".gguf") || strings.HasSuffix(e.Name(), ".safetensors") {
			return true
		}
	}
	return false
}

// addModelDir turns one model directory into its candidates: one
// whole-repo safetensors candidate, and one candidate per gguf file (or per
// set of shards).
func (s *scanner) addModelDir(rel string, entries []os.DirEntry) {
	if !validRef(s.root, rel) {
		return
	}

	var (
		hasSafetensors bool
		incomplete     bool
		ggufSizes      = map[string]int64{} // grouping key -> total bytes
		ggufFirst      = map[string]string{}
		ggufNames      []string
	)
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if rsyncTempRe.MatchString(name) {
			incomplete = true
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".safetensors"):
			hasSafetensors = true
		case strings.HasSuffix(name, ".gguf"):
			info, err := e.Info()
			if err != nil {
				continue
			}
			key := strings.TrimSuffix(name, ".gguf")
			shard := shardRe.FindStringSubmatch(name)
			if shard != nil {
				key = shard[1]
			}
			if _, seen := ggufSizes[key]; !seen {
				ggufNames = append(ggufNames, key)
				ggufFirst[key] = name
			}
			ggufSizes[key] += info.Size()
			// Prefer shard 1 as the representative file name.
			if shard != nil && shard[2] == "00001" {
				ggufFirst[key] = name
			}
		}
	}

	if hasSafetensors {
		if !s.full() {
			s.out = append(s.out, Candidate{
				ModelRef:           rel,
				Quantization:       "",
				Format:             FormatSafetensors,
				SizeBytes:          s.wholeRepoSize(rel),
				PossiblyIncomplete: incomplete,
			})
		}
	}
	sort.Strings(ggufNames)
	for _, key := range ggufNames {
		if s.full() {
			return
		}
		s.out = append(s.out, Candidate{
			ModelRef:           rel,
			Quantization:       quantizationFromName(key),
			Format:             FormatGGUF,
			SizeBytes:          ggufSizes[key],
			FileName:           ggufFirst[key],
			PossiblyIncomplete: incomplete,
		})
	}
}

// wholeRepoSize totals the model directory's regular files, excluding gguf
// files (those are separate candidates) and rsync temp files. Unreadable
// entries are skipped; a size that is slightly low is better than failing
// the whole scan.
func (s *scanner) wholeRepoSize(rel string) int64 {
	var total int64
	dir := filepath.Join(s.root, filepath.FromSlash(rel))
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".gguf") || rsyncTempRe.MatchString(name) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// quantizationFromName returns the quantization label embedded in a gguf
// base name, or UnknownQuantization when none is recognizable.
func quantizationFromName(base string) string {
	if m := quantRe.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return UnknownQuantization
}

// validRef reports whether rel is a model_ref the rest of the system can
// carry: it must round-trip through modelpath.Resolve (the same check
// delete and peer transfer apply) and contain only printable UTF-8, since
// it is later stored, displayed, and used to build paths.
func validRef(root, rel string) bool {
	if !utf8.ValidString(rel) {
		return false
	}
	for _, r := range rel {
		if unicode.IsControl(r) {
			return false
		}
	}
	dir, err := modelpath.Resolve(root, rel)
	if err != nil {
		return false
	}
	return dir == filepath.Join(root, filepath.FromSlash(rel))
}
