// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/netinfo"
	"github.com/1kaius1/Sparky/agent/peertransfer"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

const peerTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

type fakePuller struct {
	mu   sync.Mutex
	reqs []agentproto.StartPeerTransfer
	err  error
}

func (f *fakePuller) Pull(_ context.Context, req agentproto.StartPeerTransfer, progress peertransfer.ProgressFunc) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	progress(0, 0, "transferring", "")
	progress(50, 50, "completed", "")
	return f.err
}

// runWithMessage connects a Conn to a test central app that pushes one
// envelope, and returns the messages the agent sends back.
func runWithMessage(t *testing.T, msg agentproto.Envelope, setup func(*Conn)) (*Conn, chan agentproto.Envelope, func()) {
	t.Helper()
	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &msg
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	conn := New(Config{CentralURL: wsURL(srv), BearerToken: "t", NodeName: "n", ModelStoragePath: t.TempDir()}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.listInterfaces = func() ([]netinfo.Interface, error) { return nil, nil }
	if setup != nil {
		setup(conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	go conn.Run(ctx)
	return conn, app.receivedMsgs, func() { cancel(); srv.Close() }
}

func awaitType(t *testing.T, ch chan agentproto.Envelope, want agentproto.MessageType) agentproto.Envelope {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-ch:
			if env.Type == want {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s message", want)
		}
	}
}

func TestConn_AuthorizePeerPull_AcceptsAndWritesGrant(t *testing.T) {
	root, grants := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "org", "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	msg, _ := agentproto.NewEnvelope(agentproto.TypeAuthorizePeerPull, "", agentproto.AuthorizePeerPull{
		TransferID: "t-1", DestPublicKey: peerTestKey, DestIPAddress: "10.0.0.9", ModelRef: "org/m", Format: "safetensors", Quantization: "FP16",
	})
	_, got, stop := runWithMessage(t, msg, func(c *Conn) { c.authorizer = peertransfer.NewAuthorizer(grants, t.TempDir(), root, time.Hour) })
	defer stop()

	var res agentproto.PeerAuthorizeResult
	if err := awaitType(t, got, agentproto.TypePeerAuthorizeResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.TransferID != "t-1" {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(grants, "t-1.json")); err != nil {
		t.Errorf("grant not written: %v", err)
	}
}

func TestConn_AuthorizePeerPull_RefusalIsReportedNotSilent(t *testing.T) {
	msg, _ := agentproto.NewEnvelope(agentproto.TypeAuthorizePeerPull, "", agentproto.AuthorizePeerPull{
		TransferID: "t-1", DestPublicKey: `command="x" ` + peerTestKey, DestIPAddress: "10.0.0.9", ModelRef: "org/m", Format: "gguf",
	})
	grants := t.TempDir()
	_, got, stop := runWithMessage(t, msg, func(c *Conn) { c.authorizer = peertransfer.NewAuthorizer(grants, t.TempDir(), t.TempDir(), time.Hour) })
	defer stop()

	var res agentproto.PeerAuthorizeResult
	if err := awaitType(t, got, agentproto.TypePeerAuthorizeResult).DecodePayload(&res); err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.Reason == "" {
		t.Errorf("result = %+v, want a rejection with a reason", res)
	}
	if entries, _ := os.ReadDir(grants); len(entries) != 0 {
		t.Errorf("rejected authorization left %v", entries)
	}
}

func TestConn_RevokePeerPull_RemovesGrant(t *testing.T) {
	root, grants := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "org", "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	auth := peertransfer.NewAuthorizer(grants, t.TempDir(), root, time.Hour)
	if err := auth.Authorize(agentproto.AuthorizePeerPull{TransferID: "t-1", DestPublicKey: peerTestKey, DestIPAddress: "10.0.0.9", ModelRef: "org/m", Format: "safetensors"}); err != nil {
		t.Fatal(err)
	}
	msg, _ := agentproto.NewEnvelope(agentproto.TypeRevokePeerPull, "", agentproto.RevokePeerPull{TransferID: "t-1"})
	_, _, stop := runWithMessage(t, msg, func(c *Conn) { c.authorizer = auth })
	defer stop()

	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(grants, "t-1.json")); errors.Is(err, os.ErrNotExist) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("grant still present after revoke_peer_pull")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestConn_StartPeerTransfer_PullsAndReportsProgress(t *testing.T) {
	msg, _ := agentproto.NewEnvelope(agentproto.TypeStartPeerTransfer, "", agentproto.StartPeerTransfer{
		TransferID: "t-1", SourceNodeID: "src", SourceHost: "10.0.1.5", SourceSSHPort: 22, SourceHostPublicKey: peerTestKey, ModelRef: "org/m", Format: "gguf",
	})
	puller := &fakePuller{}
	_, got, stop := runWithMessage(t, msg, func(c *Conn) { c.puller = puller })
	defer stop()

	var last agentproto.TransferProgress
	for i := 0; i < 2; i++ {
		if err := awaitType(t, got, agentproto.TypeTransferProgress).DecodePayload(&last); err != nil {
			t.Fatal(err)
		}
	}
	if last.TransferID != "t-1" || last.Status != "completed" || last.BytesTotal != 50 {
		t.Errorf("last progress = %+v", last)
	}
	puller.mu.Lock()
	defer puller.mu.Unlock()
	if len(puller.reqs) != 1 || puller.reqs[0].SourceHost != "10.0.1.5" {
		t.Errorf("pull requests = %+v", puller.reqs)
	}
}

func TestConn_CheckPeerConnectivity(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		port      int
		dialErr   error
		reachable bool
	}{
		{"reachable", "10.0.1.5", 22, nil, true},
		{"refused", "10.0.1.5", 22, errors.New("connection refused"), false},
		{"not an ip", "example.com", 22, nil, false},
		{"bad port", "10.0.1.5", 0, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, _ := agentproto.NewEnvelope(agentproto.TypeCheckPeerConnectivity, "", agentproto.CheckPeerConnectivity{CheckID: "c-1", SourceHost: tt.host, SourceSSHPort: tt.port})
			var dialed string
			_, got, stop := runWithMessage(t, msg, func(c *Conn) {
				c.dial = func(_, addr string, _ time.Duration) (net.Conn, error) {
					dialed = addr
					if tt.dialErr != nil {
						return nil, tt.dialErr
					}
					a, b := net.Pipe()
					b.Close()
					return a, nil
				}
			})
			defer stop()

			var res agentproto.ConnectivityCheckResult
			if err := awaitType(t, got, agentproto.TypeConnectivityCheckResult).DecodePayload(&res); err != nil {
				t.Fatal(err)
			}
			if res.CheckID != "c-1" || res.Reachable != tt.reachable {
				t.Errorf("result = %+v, want reachable=%v", res, tt.reachable)
			}
			if !tt.reachable && res.Reason == "" {
				t.Error("an unreachable result must say why")
			}
			if tt.reachable && dialed != "10.0.1.5:22" {
				t.Errorf("dialed %q", dialed)
			}
			if tt.host == "example.com" && dialed != "" {
				t.Error("a non-IP host must never be dialed (no DNS resolution of wire values)")
			}
		})
	}
}
