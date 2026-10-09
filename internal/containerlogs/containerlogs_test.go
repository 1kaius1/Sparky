// SPDX-License-Identifier: AGPL-3.0-or-later

package containerlogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	node1      = "11111111-1111-1111-1111-111111111111"
	node2      = "22222222-2222-2222-2222-222222222222"
	instanceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	profileID  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

type fakeStore struct {
	mu          sync.Mutex
	created     []db.NewContainerLogArchive
	byUpl       map[string]*db.ContainerLogArchive
	err         error
	latest      map[string]string
	latestAsked []string
	deleted     int64
	cutoffs     []time.Time
	rows        map[string]struct {
		a  *db.ContainerLogArchive
		gz []byte
	}
}

func (f *fakeStore) Create(_ context.Context, in db.NewContainerLogArchive) (*db.ContainerLogArchive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.byUpl == nil {
		f.byUpl = map[string]*db.ContainerLogArchive{}
	}
	if a, ok := f.byUpl[in.UploadID]; ok {
		return a, nil
	}
	f.created = append(f.created, in)
	a := &db.ContainerLogArchive{ID: fmt.Sprintf("row-%d", len(f.created)), UploadID: in.UploadID}
	f.byUpl[in.UploadID] = a
	return a, nil
}

func (f *fakeStore) List(_ context.Context, limit int) ([]*db.ContainerLogArchive, error) {
	return []*db.ContainerLogArchive{{ID: "x"}}, f.err
}

func (f *fakeStore) FindByID(_ context.Context, id string) (*db.ContainerLogArchive, []byte, error) {
	if r, ok := f.rows[id]; ok {
		return r.a, r.gz, nil
	}
	return nil, nil, db.ErrContainerLogArchiveNotFound
}

func (f *fakeStore) LatestByInstanceIDs(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestAsked = append([]string(nil), ids...)
	return f.latest, f.err
}

func (f *fakeStore) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, cutoff)
	return f.deleted, f.err
}

func (f *fakeStore) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

type fakeSettings struct {
	cfg       db.ContainerLogSettings
	getErr    error
	updates   []int
	updateErr error
}

func (f *fakeSettings) Get(context.Context) (*db.ContainerLogSettings, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	c := f.cfg
	return &c, nil
}

func (f *fakeSettings) Update(_ context.Context, months int, _ *string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, months)
	f.cfg.RetentionMonths = months
	return nil
}

type auditCall struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	detail     map[string]any
}

type fakeAudit struct {
	calls []auditCall
	err   error
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, superAdmin bool, action, objectType, _ string, detail map[string]any) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, auditCall{actorID, superAdmin, action, objectType, detail})
	return nil
}

type fakeInstances struct {
	inst *db.RunningInstance
	err  error
}

func (f fakeInstances) FindByID(context.Context, string) (*db.RunningInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.inst == nil {
		return nil, db.ErrRunningInstanceNotFound
	}
	return f.inst, nil
}

type fakeProfiles struct{ p *db.Profile }

func (f fakeProfiles) FindByID(context.Context, string) (*db.Profile, error) {
	if f.p == nil {
		return nil, db.ErrProfileNotFound
	}
	return f.p, nil
}

type sentAck struct {
	nodeID string
	ack    agentproto.ContainerLogAck
}

type fakeDispatch struct {
	acks      chan sentAck
	offline   bool
	fetches   chan agentproto.FetchLogs
	onFetch   func(agentproto.FetchLogs)
	sendCalls atomic.Int32
}

func (f *fakeDispatch) Connected(string) bool { return !f.offline }

func (f *fakeDispatch) Send(_ context.Context, nodeID string, env agentproto.Envelope) error {
	f.sendCalls.Add(1)
	switch env.Type {
	case agentproto.TypeContainerLogAck:
		var ack agentproto.ContainerLogAck
		if err := env.DecodePayload(&ack); err != nil {
			return err
		}
		f.acks <- sentAck{nodeID, ack}
	case agentproto.TypeFetchLogs:
		var req agentproto.FetchLogs
		if err := env.DecodePayload(&req); err != nil {
			return err
		}
		f.fetches <- req
		if f.onFetch != nil {
			go f.onFetch(req)
		}
	}
	return nil
}

type fixture struct {
	svc      *Service
	settings *fakeSettings
	audit    *fakeAudit
	store    *fakeStore
	disp     *fakeDispatch
	now      time.Time
}

func newFixture() *fixture {
	f := &fixture{
		store: &fakeStore{},
		disp:  &fakeDispatch{acks: make(chan sentAck, 20), fetches: make(chan agentproto.FetchLogs, 20)},
		now:   time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}
	f.settings = &fakeSettings{cfg: db.ContainerLogSettings{RetentionMonths: 12}}
	f.audit = &fakeAudit{}
	f.svc = NewService(f.store, f.settings, f.audit,
		fakeInstances{inst: &db.RunningInstance{ID: instanceID, ProfileID: profileID, PrimaryNodeID: node1}},
		fakeProfiles{p: &db.Profile{ID: profileID, Name: "tiny"}},
		f.disp, log.New(io.Discard, "", 0))
	f.svc.now = func() time.Time { return f.now }
	return f
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func chunkEnv(t *testing.T, c agentproto.ContainerLogChunk) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeContainerLogChunk, "", c)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func meta() *agentproto.ContainerLogMeta {
	code := 137
	return &agentproto.ContainerLogMeta{InstanceID: instanceID, ContainerName: "sparky-tiny-1", Reason: "unload", State: "exited", ExitCode: &code, OOMKilled: true, LinesRequested: 2000, LinesKept: 2, SizeBytes: 12}
}

// upload sends data as chunks of at most size bytes under uploadID.
func (f *fixture) upload(t *testing.T, nodeID, uploadID string, data []byte, size int, m *agentproto.ContainerLogMeta) {
	t.Helper()
	sum := sha256.Sum256(data)
	for seq, off := 0, 0; ; seq++ {
		end := min(off+size, len(data))
		c := agentproto.ContainerLogChunk{UploadID: uploadID, Seq: seq, Data: data[off:end]}
		if seq == 0 {
			c.Meta = m
		}
		if end == len(data) {
			c.Final, c.TotalBytes, c.SHA256 = true, int64(len(data)), hex.EncodeToString(sum[:])
		}
		f.svc.HandleChunk(nodeID, chunkEnv(t, c))
		if c.Final {
			return
		}
		off = end
	}
}

func (f *fixture) awaitAck(t *testing.T) sentAck {
	t.Helper()
	select {
	case a := <-f.disp.acks:
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("no ack sent")
		return sentAck{}
	}
}

func (f *fixture) expectNoAck(t *testing.T) {
	t.Helper()
	select {
	case a := <-f.disp.acks:
		t.Fatalf("unexpected ack %+v", a)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleChunk_ReassemblesVerifiesStoresAndAcks(t *testing.T) {
	f := newFixture()
	data := gz(t, "line one\nline two\n")
	f.upload(t, node1, "upload-0001", data, 7, meta())

	a := f.awaitAck(t)
	if a.nodeID != node1 || a.ack.UploadID != "upload-0001" || !a.ack.Stored || a.ack.Error != "" {
		t.Fatalf("ack = %+v", a)
	}
	if f.store.createdCount() != 1 {
		t.Fatalf("stored %d rows, want 1", f.store.createdCount())
	}
	in := f.store.created[0]
	if !bytes.Equal(in.LogGz, data) || in.NodeID != node1 || in.SizeBytes != int64(len("line one\nline two\n")) {
		t.Errorf("stored row = %+v", in)
	}
	if in.InstanceID == nil || *in.InstanceID != instanceID || in.ProfileID == nil || *in.ProfileID != profileID || in.ProfileName != "tiny" {
		t.Errorf("attribution = instance %v profile %v %q, want taken from the instance row", in.InstanceID, in.ProfileID, in.ProfileName)
	}
	if in.Reason != "unload" || in.State != "exited" || in.ExitCode == nil || *in.ExitCode != 137 || !in.OOMKilled || in.LinesRequested == nil || *in.LinesRequested != 2000 {
		t.Errorf("row = %+v", in)
	}
}

// The agent's size claim is not trusted; the stored size is what the gzip
// stream really expands to.
func TestHandleChunk_StoredSizeIsMeasuredNotClaimed(t *testing.T) {
	f := newFixture()
	m := meta()
	m.SizeBytes = 999999
	f.upload(t, node1, "upload-0001", gz(t, "abc"), 100, m)
	f.awaitAck(t)
	if got := f.store.created[0].SizeBytes; got != 3 {
		t.Errorf("SizeBytes = %d, want the measured 3", got)
	}
}

func TestHandleChunk_MultiChunkWithRandomData(t *testing.T) {
	f := newFixture()
	rng := rand.New(rand.NewSource(7))
	var sb strings.Builder
	for sb.Len() < 80_000 {
		fmt.Fprintf(&sb, "%016x\n", rng.Uint64())
	}
	data := gz(t, sb.String())
	f.upload(t, node1, "upload-0002", data, agentproto.ContainerLogChunkSize, meta())
	if a := f.awaitAck(t); !a.ack.Stored {
		t.Fatalf("ack = %+v", a)
	}
	if !bytes.Equal(f.store.created[0].LogGz, data) {
		t.Error("stored body differs from the uploaded stream")
	}
}

func TestHandleChunk_RejectsDamagedUploads(t *testing.T) {
	data := gz(t, "hello\n")
	sum := sha256.Sum256(data)
	for name, final := range map[string]agentproto.ContainerLogChunk{
		"wrong sha":   {Final: true, TotalBytes: int64(len(data)), SHA256: strings.Repeat("0", 64)},
		"wrong total": {Final: true, TotalBytes: int64(len(data)) + 1, SHA256: hex.EncodeToString(sum[:])},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			final.UploadID, final.Seq, final.Data, final.Meta = "upload-0003", 0, data, meta()
			f.svc.HandleChunk(node1, chunkEnv(t, final))
			if a := f.awaitAck(t); a.ack.Stored || a.ack.Error == "" {
				t.Errorf("ack = %+v, want a refusal with a reason", a)
			}
			if f.store.createdCount() != 0 {
				t.Error("a damaged upload was stored")
			}
		})
	}
}

func TestHandleChunk_RejectsNonGzipAndGzipBombs(t *testing.T) {
	f := newFixture()
	notGzip := []byte("this is not gzip")
	f.upload(t, node1, "upload-0004", notGzip, 100, meta())
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("non-gzip data was accepted")
	}

	// A stream that expands past the cap: highly compressible zeros.
	f.svc.maxUncompressed = 1 << 20
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(make([]byte, 4<<20))
	zw.Close()
	f.upload(t, node1, "upload-0005", buf.Bytes(), agentproto.ContainerLogChunkSize, meta())
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("a gzip bomb was accepted")
	}
	if f.store.createdCount() != 0 {
		t.Error("something was stored")
	}
}

func TestHandleChunk_RejectsOutOfOrderAndUnknownUploads(t *testing.T) {
	f := newFixture()
	data := gz(t, "x")

	// Seq 1 with no Seq 0 first.
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0006", Seq: 1, Data: data, Final: true}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("an unknown upload was accepted")
	}

	// Skipped sequence number.
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0007", Seq: 0, Data: data[:1], Meta: meta()}))
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0007", Seq: 2, Data: data[1:], Final: true}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("an out-of-order chunk was accepted")
	}

	// A restarted upload under the same id is dropped, not merged.
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0008", Seq: 0, Data: data[:1], Meta: meta()}))
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0008", Seq: 0, Data: data[:1], Meta: meta()}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("a restarted upload was accepted")
	}
	if f.store.createdCount() != 0 {
		t.Error("something was stored")
	}
}

func TestHandleChunk_FirstChunkNeedsMeta(t *testing.T) {
	f := newFixture()
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-0009", Seq: 0, Data: []byte{1}}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("a first chunk with no description was accepted")
	}
}

func TestHandleChunk_CapsSize(t *testing.T) {
	f := newFixture()
	f.svc.maxCompressed = 3 * agentproto.ContainerLogChunkSize
	full := make([]byte, agentproto.ContainerLogChunkSize)
	for seq := 0; seq < 4; seq++ {
		c := agentproto.ContainerLogChunk{UploadID: "upload-0010", Seq: seq, Data: full}
		if seq == 0 {
			c.Meta = meta()
		}
		f.svc.HandleChunk(node1, chunkEnv(t, c))
		if seq < 3 {
			f.expectNoAck(t)
		}
	}
	if a := f.awaitAck(t); a.ack.Stored || !strings.Contains(a.ack.Error, "too large") {
		t.Errorf("ack = %+v, want a too-large refusal on the chunk that passes the cap", a)
	}
	if f.store.createdCount() != 0 {
		t.Error("an oversize upload was stored")
	}
}

func TestHandleChunk_CapsConcurrentUploadsPerNode(t *testing.T) {
	f := newFixture()
	for i := 0; i < maxUploadsPerNode; i++ {
		f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: fmt.Sprintf("upload-c%03d", i), Seq: 0, Data: []byte{1}, Meta: meta()}))
	}
	f.expectNoAck(t)
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-c999", Seq: 0, Data: []byte{1}, Meta: meta()}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("the fifth concurrent upload was accepted")
	}
	// Another node is unaffected.
	f.svc.HandleChunk(node2, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-d000", Seq: 0, Data: []byte{1}, Meta: meta()}))
	f.expectNoAck(t)
}

func TestHandleChunk_IdleUploadsExpire(t *testing.T) {
	f := newFixture()
	for i := 0; i < maxUploadsPerNode; i++ {
		f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: fmt.Sprintf("upload-e%03d", i), Seq: 0, Data: []byte{1}, Meta: meta()}))
	}
	f.now = f.now.Add(uploadIdleTimeout + time.Second)
	// The old ones are swept, so a new upload fits again.
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-e999", Seq: 0, Data: []byte{1}, Meta: meta()}))
	f.expectNoAck(t)
	// And a late chunk of an expired upload is refused.
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "upload-e000", Seq: 1, Data: []byte{1}, Final: true}))
	if a := f.awaitAck(t); a.ack.Stored {
		t.Error("a chunk of an expired upload was accepted")
	}
}

// A node can only attribute a log to its own instances.
func TestHandleChunk_DoesNotLinkAnotherNodesInstance(t *testing.T) {
	f := newFixture()
	f.upload(t, node2, "upload-0011", gz(t, "x\n"), 100, meta()) // the instance belongs to node1
	if a := f.awaitAck(t); !a.ack.Stored {
		t.Fatalf("ack = %+v", a)
	}
	in := f.store.created[0]
	if in.NodeID != node2 || in.InstanceID != nil || in.ProfileID != nil || in.ProfileName != "" {
		t.Errorf("row = %+v, want stored under node2 with no instance or profile link", in)
	}
}

func TestHandleChunk_UnknownInstanceIsStoredUnlinked(t *testing.T) {
	f := newFixture()
	f.svc.instances = fakeInstances{}
	f.upload(t, node1, "upload-0012", gz(t, "x\n"), 100, meta())
	if a := f.awaitAck(t); !a.ack.Stored {
		t.Fatalf("ack = %+v", a)
	}
	if in := f.store.created[0]; in.InstanceID != nil || in.ProfileID != nil {
		t.Errorf("row = %+v", in)
	}
}

func TestHandleChunk_SanitizesAgentSuppliedFields(t *testing.T) {
	f := newFixture()
	m := meta()
	m.InstanceID = "not-a-uuid"
	m.Reason = "Drop Table;"
	m.ContainerName = strings.Repeat("n", 500)
	m.LinesKept = -5
	f.upload(t, node1, "upload-0013", gz(t, "x\n"), 100, m)
	f.awaitAck(t)
	in := f.store.created[0]
	if in.InstanceID != nil || in.Reason != "unknown" || len(in.ContainerName) != 128 || in.LinesKept != 0 {
		t.Errorf("row = %+v", in)
	}
}

func TestHandleChunk_StoreFailureAnswersNotStored(t *testing.T) {
	f := newFixture()
	f.store.err = errors.New("pq: connection refused")
	f.upload(t, node1, "upload-0014", gz(t, "x\n"), 100, meta())
	a := f.awaitAck(t)
	if a.ack.Stored || strings.Contains(a.ack.Error, "pq") || strings.Contains(a.ack.Error, "refused") {
		t.Errorf("ack = %+v: must be a refusal without the raw database error", a)
	}
}

func TestHandleChunk_RetryOfAStoredUploadStoresOnce(t *testing.T) {
	f := newFixture()
	data := gz(t, "x\n")
	f.upload(t, node1, "upload-0015", data, 100, meta())
	f.awaitAck(t)
	f.upload(t, node1, "upload-0015", data, 100, meta())
	if a := f.awaitAck(t); !a.ack.Stored {
		t.Errorf("retry ack = %+v, want Stored", a)
	}
	if f.store.createdCount() != 1 {
		t.Errorf("stored %d rows, want 1", f.store.createdCount())
	}
}

func TestHandleChunk_IgnoresMalformedAndForeignMessages(t *testing.T) {
	f := newFixture()
	f.svc.HandleChunk(node1, agentproto.Envelope{Type: agentproto.TypeContainerLogChunk, Payload: []byte(`{"bogus":1}`)})
	f.svc.HandleChunk(node1, chunkEnv(t, agentproto.ContainerLogChunk{UploadID: "x", Seq: 0, Meta: meta()})) // invalid id
	f.svc.HandleChunk(node1, agentproto.Envelope{Type: agentproto.TypeHeartbeat})
	f.expectNoAck(t)
}

func archiveRow(id string) (*db.ContainerLogArchive, []byte) {
	return &db.ContainerLogArchive{ID: id, ContainerName: "c"}, nil
}

func TestRead_ReturnsTheTextForAPermittedActor(t *testing.T) {
	f := newFixture()
	id := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	a, _ := archiveRow(id)
	f.store.rows = map[string]struct {
		a  *db.ContainerLogArchive
		gz []byte
	}{id: {a, gz(t, "hello\nworld\n")}}

	got, text, err := f.svc.Read(context.Background(), rbac.Actor{Tier: db.TierDeveloper}, id)
	if err != nil || got.ID != id || text != "hello\nworld\n" {
		t.Fatalf("got %+v %q %v", got, text, err)
	}
}

func TestRead_RefusesALogThatExpandsPastTheCap(t *testing.T) {
	f := newFixture()
	f.svc.maxUncompressed = 1 << 10
	id := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	a, _ := archiveRow(id)
	f.store.rows = map[string]struct {
		a  *db.ContainerLogArchive
		gz []byte
	}{id: {a, gz(t, strings.Repeat("x", 4096))}}
	if _, _, err := f.svc.Read(context.Background(), rbac.Actor{Tier: db.TierDeveloper}, id); err == nil {
		t.Error("Read returned a log that expands past the cap")
	}
}

func TestRead_RefusesReadOnlyAndHandlesMissingAndMalformedIDs(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if _, _, err := f.svc.Read(ctx, rbac.Actor{Tier: db.TierReadOnly}, instanceID); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("read-only err = %v, want ErrNotPermitted", err)
	}
	if _, err := f.svc.List(ctx, rbac.Actor{Tier: db.TierReadOnly}, 10); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("read-only List err = %v, want ErrNotPermitted", err)
	}
	dev := rbac.Actor{Tier: db.TierDeveloper}
	if _, _, err := f.svc.Read(ctx, dev, instanceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing err = %v, want ErrNotFound", err)
	}
	if _, _, err := f.svc.Read(ctx, dev, "not-a-uuid'; DROP TABLE x;--"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id err = %v, want ErrNotFound without touching the store", err)
	}
	if _, err := f.svc.List(ctx, dev, 10); err != nil {
		t.Errorf("developer List err = %v", err)
	}
}

func TestClip(t *testing.T) {
	if got := clip("abcdef", 4); got != "abcd" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abé", 3); got != "ab" { // é is two bytes, cut in half at 3
		t.Errorf("clip = %q, want the cut character dropped", got)
	}
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip = %q", got)
	}
}

func TestLatestForInstances_ChecksTheTierAndSkipsInvalidIDs(t *testing.T) {
	f := newFixture()
	f.store.latest = map[string]string{instanceID: "archive-1"}
	ctx := context.Background()

	if _, err := f.svc.LatestForInstances(ctx, rbac.Actor{Tier: db.TierReadOnly}, []string{instanceID}); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("read-only err = %v, want ErrNotPermitted", err)
	}
	got, err := f.svc.LatestForInstances(ctx, rbac.Actor{Tier: db.TierDeveloper}, []string{instanceID, "not-a-uuid", "x'; DROP TABLE y;--"})
	if err != nil || got[instanceID] != "archive-1" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(f.store.latestAsked) != 1 || f.store.latestAsked[0] != instanceID {
		t.Errorf("store was asked about %v, want only the valid uuid", f.store.latestAsked)
	}
}
