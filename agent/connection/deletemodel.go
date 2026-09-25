// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// removeModel deletes a model copy from local storage, resolving the path
// itself from the same layout runTransfer writes (never a wire-supplied
// path). modelRef arrives over the wire, so it is validated to stay
// strictly inside ModelStoragePath - a value like "../../etc" must never
// reach os.RemoveAll. A safetensors copy, or a GGUF entry whose quantization
// is empty/"UNKNOWN" (whole-repo), removes the model's whole directory. A
// GGUF entry with a concrete quantization removes only the .gguf file(s)
// matching it (same filename-contains matching resolveModelPath uses), and
// the directory too once no .gguf files remain, so sibling quantizations
// of the same repo are left alone.
func (c *Conn) removeModel(modelRef, quantization, format string) error {
	root := filepath.Clean(c.cfg.ModelStoragePath)
	if modelRef == "" {
		return fmt.Errorf("empty model_ref")
	}
	dir := filepath.Join(root, filepath.FromSlash(modelRef))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(modelRef) {
		return fmt.Errorf("model_ref %q resolves outside model storage", modelRef)
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("model directory: %w", err)
	}

	wholeRepo := format != "gguf" || quantization == "" || quantization == "UNKNOWN"
	if wholeRepo {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.gguf"))
	if err != nil {
		return fmt.Errorf("glob .gguf files in %s: %w", dir, err)
	}
	removed := 0
	for _, m := range matches {
		if !strings.Contains(filepath.Base(m), quantization) {
			continue
		}
		if err := os.Remove(m); err != nil {
			return fmt.Errorf("remove %s: %w", m, err)
		}
		removed++
	}
	if removed == 0 {
		return fmt.Errorf("no .gguf file matching quantization %q in %s", quantization, dir)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.gguf")); len(left) == 0 {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return nil
}

// runDeleteModel handles a delete_model command and reports the outcome
// via delete_model_result.
func (c *Conn) runDeleteModel(ctx context.Context, conn *websocket.Conn, del agentproto.DeleteModel) {
	result := agentproto.DeleteModelResult{ModelRef: del.ModelRef, Quantization: del.Quantization, Format: del.Format, Success: true}
	if err := c.removeModel(del.ModelRef, del.Quantization, del.Format); err != nil {
		c.logger.Printf("agent connection: delete model %s: %v", del.ModelRef, err)
		result.Success = false
		result.Reason = err.Error()
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeDeleteModelResult, "", result)
	if err != nil {
		c.logger.Printf("agent connection: build delete_model_result for %s: %v", del.ModelRef, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal delete_model_result for %s: %v", del.ModelRef, err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send delete_model_result for %s: %v", del.ModelRef, err)
	}
}
