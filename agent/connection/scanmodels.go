// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/modelscan"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// scanModels lists the model copies under ModelStoragePath. The path is
// always the agent's own configured storage root - the scan_models command
// carries no location, so a compromised or buggy central app cannot point
// the agent at an arbitrary directory.
func (c *Conn) scanModels() (agentproto.ScanModelsResult, error) {
	root := c.cfg.ModelStoragePath
	if root == "" {
		return agentproto.ScanModelsResult{}, errors.New("model storage path is not configured on this node (SPARKY_MODEL_STORAGE_PATH)")
	}
	cands, truncated, err := modelscan.Scan(root)
	if err != nil {
		return agentproto.ScanModelsResult{}, err
	}
	models := make([]agentproto.ScannedModel, 0, len(cands))
	for _, m := range cands {
		models = append(models, agentproto.ScannedModel{
			ModelRef:           m.ModelRef,
			Quantization:       m.Quantization,
			Format:             m.Format,
			SizeBytes:          m.SizeBytes,
			FileName:           m.FileName,
			PossiblyIncomplete: m.PossiblyIncomplete,
		})
	}
	return agentproto.ScanModelsResult{Models: models, Truncated: truncated}, nil
}

// runScanModels handles a scan_models command and reports the outcome via
// scan_models_result. A failed scan is still answered (Error set) so the
// central app can show why instead of waiting for a result that never comes.
func (c *Conn) runScanModels(ctx context.Context, conn *websocket.Conn, req agentproto.ScanModels) {
	result, err := c.scanModels()
	if err != nil {
		c.logger.Printf("agent connection: scan models %s: %v", req.ScanID, err)
		result = agentproto.ScanModelsResult{Error: err.Error()}
	}
	result.ScanID = req.ScanID

	env, err := agentproto.NewEnvelope(agentproto.TypeScanModelsResult, "", result)
	if err != nil {
		c.logger.Printf("agent connection: build scan_models_result for %s: %v", req.ScanID, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal scan_models_result for %s: %v", req.ScanID, err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send scan_models_result for %s: %v", req.ScanID, err)
	}
}
