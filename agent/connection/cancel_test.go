// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/agent/peertransfer"
	"github.com/1kaius1/Sparky/agent/transfer"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// ctxDownloader blocks until its context is cancelled, then behaves like the
// real executor: reports a failure carrying the context error and returns
// it. That is exactly what agent/transfer.Executor.Download does when its
// context is cancelled mid-download.
type ctxDownloader struct{ started chan struct{} }

func (d *ctxDownloader) Download(ctx context.Context, _, _, _ string, progress transfer.ProgressFunc) error {
	progress(0, 100, transfer.StatusTransferring, "")
	close(d.started)
	<-ctx.Done()
	progress(10, 100, transfer.StatusFailed, ctx.Err().Error())
	return ctx.Err()
}

func TestConn_CancelTransfer_StopsADownloadAndReportsCancelled(t *testing.T) {
	start, _ := agentproto.NewEnvelope(agentproto.TypeStartTransfer, "", agentproto.StartTransfer{TransferID: "t-1", ModelRef: "org/m"})
	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &start
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	dl := &ctxDownloader{started: make(chan struct{})}
	conn := New(Config{CentralURL: wsURL(srv), BearerToken: "t", NodeName: "n", ModelStoragePath: t.TempDir()}, &fakeRuntimeBackend{}, dl, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go conn.Run(ctx)

	select {
	case <-dl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the download never started")
	}
	if !conn.cancelTransfer("t-1") {
		t.Fatal("cancelTransfer found no running transfer")
	}

	var last agentproto.TransferProgress
	deadline := time.After(2 * time.Second)
	for last.Status != "cancelled" {
		select {
		case env := <-app.receivedMsgs:
			if env.Type == agentproto.TypeTransferProgress {
				if err := env.DecodePayload(&last); err != nil {
					t.Fatal(err)
				}
				if last.Status == "failed" {
					t.Fatalf("a cancelled transfer must not be reported as failed: %+v", last)
				}
			}
		case <-deadline:
			t.Fatalf("no cancelled report; last progress %+v", last)
		}
	}
	if last.TransferID != "t-1" || last.ErrorMessage != "" || last.BytesTransferred != 10 {
		t.Errorf("last = %+v, want cancelled with the bytes moved so far and no error text", last)
	}
}

func TestConn_CancelRightBehindStartIsNotLost(t *testing.T) {
	// Both messages arrive back to back; the cancel is handled by the read
	// loop immediately after the start, before the transfer goroutine has
	// necessarily run - so the transfer must already be registered.
	c := New(Config{ModelStoragePath: t.TempDir()}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	workCtx, _, done := c.registerTransfer(context.Background(), "t-1")
	defer done()
	if !c.cancelTransfer("t-1") {
		t.Fatal("a registered transfer must be cancellable before its goroutine starts")
	}
	select {
	case <-workCtx.Done():
	default:
		t.Error("cancelling must cancel the transfer's context")
	}
}

func TestConn_CancelUnknownTransferIsHarmless(t *testing.T) {
	c := New(Config{}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	if c.cancelTransfer("never-ran") {
		t.Error("cancelTransfer reported success for a transfer this agent is not running")
	}
	_, _, done := c.registerTransfer(context.Background(), "t-1")
	done()
	if c.cancelTransfer("t-1") {
		t.Error("a finished transfer must no longer be cancellable")
	}
}

func TestCancelAwareProgress(t *testing.T) {
	var got []string
	next := func(_, _ int64, status, errMsg string) { got = append(got, status+":"+errMsg) }

	run := &transferRun{}
	p := cancelAwareProgress(run, next)
	p(1, 2, "failed", "connection reset")
	if got[0] != "failed:connection reset" {
		t.Errorf("a failure that happened before any cancel must stay a failure: %v", got)
	}

	run.cancelled.Store(true)
	p(1, 2, "failed", "context canceled")
	p(1, 2, "transferring", "")
	p(2, 2, "completed", "")
	if got[1] != "cancelled:" || got[2] != "transferring:" || got[3] != "completed:" {
		t.Errorf("after a cancel only the failure is rewritten: %v", got)
	}
}

type blockingPuller struct{ started chan struct{} }

func (p *blockingPuller) Pull(ctx context.Context, _ agentproto.StartPeerTransfer, progress peertransfer.ProgressFunc) error {
	progress(0, 0, "transferring", "")
	close(p.started)
	<-ctx.Done()
	progress(0, 0, "failed", "rsync failed: "+ctx.Err().Error())
	return errors.New("cancelled")
}

func TestConn_CancelTransfer_StopsAPeerPullToo(t *testing.T) {
	start, _ := agentproto.NewEnvelope(agentproto.TypeStartPeerTransfer, "", agentproto.StartPeerTransfer{
		TransferID: "t-2", SourceNodeID: "s", SourceHost: "10.0.0.5", SourceSSHPort: 22, SourceHostPublicKey: peerTestKey, ModelRef: "org/m", Format: "gguf",
	})
	app := newTestCentralApp(true, "")
	app.sendAfterAccept = &start
	app.receivedMsgs = make(chan agentproto.Envelope, 10)
	srv := httptest.NewServer(app)
	defer srv.Close()

	pl := &blockingPuller{started: make(chan struct{})}
	conn := New(Config{CentralURL: wsURL(srv), BearerToken: "t", NodeName: "n", ModelStoragePath: t.TempDir()}, &fakeRuntimeBackend{}, &fakeTransferExecutor{}, &fakeEngineTransferExecutor{}, &fakeTelemetryCollector{}, testLogger())
	conn.puller = pl
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go conn.Run(ctx)

	select {
	case <-pl.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the pull never started")
	}
	conn.cancelTransfer("t-2")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-app.receivedMsgs:
			var p agentproto.TransferProgress
			if env.Type != agentproto.TypeTransferProgress || env.DecodePayload(&p) != nil {
				continue
			}
			if p.Status == "cancelled" && p.TransferID == "t-2" {
				return
			}
			if p.Status == "failed" {
				t.Fatalf("a cancelled pull must not be reported as failed: %+v", p)
			}
		case <-deadline:
			t.Fatal("no cancelled report for the peer pull")
		}
	}
}
