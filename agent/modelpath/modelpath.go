// SPDX-License-Identifier: AGPL-3.0-or-later

// Package modelpath resolves a wire-supplied model_ref to a directory
// inside the node's model storage, refusing anything that would escape it.
// A model_ref arrives over the network (from the central app, and for
// peer transfers ultimately from another node's inventory) and is used to
// pick a path to delete, serve, or write into - so every such use goes
// through here rather than a bare filepath.Join.
package modelpath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resolve returns the model's directory under root (root/<modelRef>). It
// rejects an empty ref, an absolute ref, and any ref that cleans to root
// itself or to a path outside it (".", "..", "a/../../b"). This is a
// lexical check; callers that follow symlinks (the peer-transfer source)
// must additionally verify the resolved real path.
func Resolve(root, modelRef string) (string, error) {
	if modelRef == "" {
		return "", fmt.Errorf("empty model_ref")
	}
	if filepath.IsAbs(modelRef) || strings.HasPrefix(modelRef, "/") {
		return "", fmt.Errorf("model_ref %q resolves outside model storage", modelRef)
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, filepath.FromSlash(modelRef))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("model_ref %q resolves outside model storage", modelRef)
	}
	return dir, nil
}
