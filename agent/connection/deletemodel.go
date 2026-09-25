// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/modelpath"
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
	dir, err := modelpath.Resolve(c.cfg.ModelStoragePath, modelRef)
	if err != nil {
		return err
	}
	// Deleting is idempotent: files that are already gone (removed by hand,
	// a lost disk, an earlier delete whose answer never arrived) are the
	// outcome the operator asked for, so that is success - otherwise the
	// inventory entry could never be cleared. Only a genuine failure to
	// inspect or remove them is an error.
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("model directory: %w", err)
	}

	wholeRepo := format != "gguf" || quantization == "" || quantization == "UNKNOWN"
	if wholeRepo {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	}

	// Any file whose name contains the quantization - not just "*.gguf": an
	// interrupted peer copy also leaves rsync's hidden temp file
	// (".<name>.gguf.XXXXXX"), which is exactly the data a delete of an
	// incomplete entry has to free.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), quantization) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
		removed++
	}
	if removed == 0 {
		// Nothing matched: this quantization is already gone (the directory
		// may still hold a sibling's files, which are left alone).
		return nil
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
