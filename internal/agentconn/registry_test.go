// SPDX-License-Identifier: AGPL-3.0-or-later

package agentconn

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
)

// registerViaHandshake completes a real handshake against h so nodeID ends
// up registered in registry with a real, live server-side *websocket.Conn
// - Registry.Send has nothing meaningful to test against a conn that was
// never actually accepted through Handler. Returns the client-side
// connection a test reads from to observe what Send wrote.
func registerViaHandshake(t *testing.T, nodeID string) (registry *Registry, client *websocket.Conn) {
	t.Helper()
	node := &db.Node{ID: nodeID, Name: "spark-1"}
	h, _, registry := testHandler(t, &fakeAuthenticator{node: node})
	conn := dialTestServer(t, h)

	writeHello(t, conn, "req-1", "spark-1", "spk_validtoken")
	readHelloAck(t, conn)

	return registry, conn
}

func TestRegistry_Send_WritesToConnection(t *testing.T) {
	registry, client := registerViaHandshake(t, "node-1")

	env, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "req-2",
		agentproto.StartTransfer{TransferID: "xfer-1", ModelRef: "test-org/test-model"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	if err := registry.Send(context.Background(), "node-1", env); err != nil {
		t.Fatalf("Send() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	var got agentproto.Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if got.Type != agentproto.TypeStartTransfer {
		t.Errorf("Type = %q, want %q", got.Type, agentproto.TypeStartTransfer)
	}

	var start agentproto.StartTransfer
	if err := got.DecodePayload(&start); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if start.TransferID != "xfer-1" || start.ModelRef != "test-org/test-model" {
		t.Errorf("StartTransfer = %+v, want TransferID=xfer-1 ModelRef=test-org/test-model", start)
	}
}

func TestRegistry_Send_NotConnected(t *testing.T) {
	registry := NewRegistry()

	env, err := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "req-1",
		agentproto.StartTransfer{TransferID: "xfer-1", ModelRef: "test-org/test-model"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	if err := registry.Send(context.Background(), "no-such-node", env); err != ErrNodeNotConnected {
		t.Errorf("Send() error = %v, want ErrNodeNotConnected", err)
	}
}
