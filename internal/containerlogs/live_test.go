// SPDX-License-Identifier: AGPL-3.0-or-later

package containerlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var developer = rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}

// replyWith makes the fake dispatcher play the agent: when a fetch_logs request
// goes out, it sends the given log back as a chunk series from fromNode.
func (f *fixture) replyWith(t *testing.T, fromNode, text string) {
	t.Helper()
	f.disp.onFetch = func(req agentproto.FetchLogs) {
		data := gz(t, text)
		sum := sha256.Sum256(data)
		m := meta()
		m.State, m.OOMKilled = "running", false
		for seq, off := 0, 0; ; seq++ {
			end := min(off+9, len(data)) // tiny chunks, to exercise reassembly
			c := agentproto.ContainerLogChunk{UploadID: req.FetchID, FetchID: req.FetchID, Seq: seq, Data: data[off:end]}
			if seq == 0 {
				c.Meta = m
			}
			if end == len(data) {
				c.Final, c.TotalBytes, c.SHA256 = true, int64(len(data)), hex.EncodeToString(sum[:])
			}
			f.svc.HandleChunk(fromNode, chunkEnv(t, c))
			if c.Final {
				return
			}
			off = end
		}
	}
}

func TestLive_ReturnsTheNodesAnswerAndStoresNothing(t *testing.T) {
	f := newFixture()
	f.replyWith(t, node1, "live one\nlive two\n")

	got, err := f.svc.Live(context.Background(), developer, instanceID, 100)
	if err != nil {
		t.Fatalf("Live() error: %v", err)
	}
	if got.Text != "live one\nlive two\n" || got.ProfileName != "tiny" || got.InstanceID != instanceID || got.Meta.State != "running" {
		t.Errorf("got %+v", got)
	}
	req := <-f.disp.fetches
	if req.InstanceID != instanceID || req.Lines != 100 || len(req.FetchID) != 32 {
		t.Errorf("request = %+v", req)
	}
	if f.store.createdCount() != 0 {
		t.Error("a live read stored an archive")
	}
	select {
	case a := <-f.disp.acks:
		t.Errorf("a live read sent an archive ack: %+v", a)
	default:
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.live) != 0 || len(f.svc.uploads) != 0 {
		t.Errorf("state left behind: %d live, %d uploads", len(f.svc.live), len(f.svc.uploads))
	}
}

func TestLive_ClampsLines(t *testing.T) {
	for in, want := range map[int]int{0: DefaultLiveLines, -3: DefaultLiveLines, 50: 50, 999999: MaxLiveLines} {
		f := newFixture()
		f.replyWith(t, node1, "x\n")
		if _, err := f.svc.Live(context.Background(), developer, instanceID, in); err != nil {
			t.Fatal(err)
		}
		if got := (<-f.disp.fetches).Lines; got != want {
			t.Errorf("lines %d: asked the node for %d, want %d", in, got, want)
		}
	}
}

func TestLive_TheNodesOwnReasonIsReturned(t *testing.T) {
	f := newFixture()
	f.disp.onFetch = func(req agentproto.FetchLogs) {
		f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: req.FetchID, FetchID: req.FetchID, Final: true, Error: "this instance has no container or process on the node any more"}))
	}
	_, err := f.svc.Live(context.Background(), developer, instanceID, 100)
	var ae *AgentError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "no container") {
		t.Errorf("err = %v, want an AgentError with the node's reason", err)
	}
}

func TestLive_NoAnswerTimesOutAndCleansUp(t *testing.T) {
	f := newFixture()
	f.svc.liveTimeout = 80 * time.Millisecond
	_, err := f.svc.Live(context.Background(), developer, instanceID, 100)
	if !errors.Is(err, ErrAgentSilent) {
		t.Errorf("err = %v, want ErrAgentSilent", err)
	}
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.live) != 0 {
		t.Error("a timed-out request was left registered")
	}
}

func TestLive_OfflineNodeIsReportedWithoutAsking(t *testing.T) {
	f := newFixture()
	f.disp.offline = true
	if _, err := f.svc.Live(context.Background(), developer, instanceID, 100); !errors.Is(err, ErrNodeOffline) {
		t.Errorf("err = %v, want ErrNodeOffline", err)
	}
	if f.disp.sendCalls.Load() != 0 {
		t.Error("a request was sent to an offline node")
	}
}

func TestLive_UnknownInstanceAndBadIDsAndTiers(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	f.svc.instances = fakeInstances{}
	if _, err := f.svc.Live(ctx, developer, instanceID, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown instance: %v", err)
	}
	if _, err := f.svc.Live(ctx, developer, "x'; DROP TABLE y;--", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id: %v", err)
	}
	if _, err := f.svc.Live(ctx, rbac.Actor{Tier: db.TierReadOnly}, instanceID, 10); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("read-only: %v", err)
	}
	if f.disp.sendCalls.Load() != 0 {
		t.Error("something was sent for a request that should have been refused")
	}
}

func TestLive_LimitsRequestsInFlight(t *testing.T) {
	f := newFixture()
	f.svc.liveTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 3)
	for i := 0; i < maxLiveFetchesPerNode; i++ {
		go func() { _, err := f.svc.Live(ctx, developer, instanceID, 10); done <- err }()
	}
	for i := 0; i < maxLiveFetchesPerNode; i++ {
		<-f.disp.fetches
	}
	if _, err := f.svc.Live(context.Background(), developer, instanceID, 10); !errors.Is(err, ErrBusy) {
		t.Errorf("third request on the node: %v, want ErrBusy", err)
	}
	cancel()
	for i := 0; i < maxLiveFetchesPerNode; i++ {
		<-done
	}
}

// Only the node the request went to may answer it.
func TestLive_AnAnswerFromAnotherNodeIsIgnored(t *testing.T) {
	f := newFixture()
	f.svc.liveTimeout = 150 * time.Millisecond
	f.replyWith(t, node2, "from the wrong node\n") // the instance is on node1
	_, err := f.svc.Live(context.Background(), developer, instanceID, 10)
	if !errors.Is(err, ErrAgentSilent) {
		t.Errorf("err = %v, want ErrAgentSilent: another node's reply must not be accepted", err)
	}
}

func TestLive_ADamagedReplyIsReportedNotShown(t *testing.T) {
	f := newFixture()
	f.disp.onFetch = func(req agentproto.FetchLogs) {
		f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{
			UploadID: req.FetchID, FetchID: req.FetchID, Seq: 0, Final: true, Data: gz(t, "x"), Meta: meta(),
			TotalBytes: 3, SHA256: strings.Repeat("0", 64),
		}))
	}
	_, err := f.svc.Live(context.Background(), developer, instanceID, 10)
	var ae *AgentError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "damaged") {
		t.Errorf("err = %v, want a damaged-reply AgentError", err)
	}
}

// A chunk that claims to answer a request nobody is waiting for is dropped,
// and in particular never becomes an archive.
func TestHandleChunk_AnUnsolicitedLiveReplyIsIgnored(t *testing.T) {
	f := newFixture()
	data := gz(t, "x\n")
	sum := sha256.Sum256(data)
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{
		UploadID: "fetch-unsolicited", FetchID: "fetch-unsolicited", Seq: 0, Final: true, Data: data, Meta: meta(),
		TotalBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	}))
	f.expectNoAck(t)
	time.Sleep(50 * time.Millisecond)
	if f.store.createdCount() != 0 {
		t.Error("an unsolicited live reply was stored as an archive")
	}
}
