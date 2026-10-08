// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

func TestScanModels_ListsStorageRoot(t *testing.T) {
	c, root := newDeleteTestConn(t)
	writeFile(t, filepath.Join(root, "org", "m", "model.safetensors"))
	writeFile(t, filepath.Join(root, "org", "g", "g.Q4_K_M.gguf"))

	res, err := c.scanModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Models) != 2 || res.Models[0].ModelRef != "org/g" || res.Models[0].Quantization != "Q4_K_M" || res.Models[1].ModelRef != "org/m" {
		t.Errorf("unexpected models: %+v", res.Models)
	}
}

func TestScanModels_UnconfiguredPathIsAnError(t *testing.T) {
	c := New(Config{}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	if _, err := c.scanModels(); err == nil {
		t.Error("expected an error when ModelStoragePath is empty")
	}
}

func TestConn_ScanModels_RepliesWithScanID(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "org", "m", "model.safetensors"))

	scan, err := agentproto.NewEnvelope(agentproto.TypeScanModels, "", agentproto.ScanModels{ScanID: "scan-1"})
	if err != nil {
		t.Fatal(err)
	}
	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &scan
	app.receivedMsgs = make(chan agentproto.Envelope, 16)
	srv := httptest.NewServer(app)
	defer srv.Close()

	conn := New(Config{CentralURL: wsURL(srv), BearerToken: "t", NodeName: "spark-1", ModelStoragePath: root}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go conn.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-app.receivedMsgs:
			if env.Type != agentproto.TypeScanModelsResult {
				continue
			}
			var got agentproto.ScanModelsResult
			if err := env.DecodePayload(&got); err != nil {
				t.Fatal(err)
			}
			if got.ScanID != "scan-1" || got.Error != "" || len(got.Models) != 1 || got.Models[0].ModelRef != "org/m" {
				t.Errorf("result = %+v", got)
			}
			return
		case <-deadline:
			t.Fatal("no scan_models_result received")
		}
	}
}
