// SPDX-License-Identifier: AGPL-3.0-or-later

package inventory

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	scanNode1 = "11111111-1111-1111-1111-111111111111"
	scanNode2 = "22222222-2222-2222-2222-222222222222"
)

var superActor = rbac.Actor{IsSuperAdmin: true}

type scanFixture struct {
	svc      *Service
	store    *fakeInventoryStore
	dispatch *fakeDispatcher
	audit    *fakeAudit
	clockAt  time.Time
}

func newScanFixture() *scanFixture {
	f := &scanFixture{
		store:    &fakeInventoryStore{getErr: db.ErrNodeModelInventoryNotFound},
		dispatch: &fakeDispatcher{connected: true},
		audit:    &fakeAudit{},
		clockAt:  time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
	}
	f.svc = NewService(f.store, &fakeProfileStore{}, &fakeOverrideStore{granted: true}, f.dispatch, f.audit, log.New(&bytes.Buffer{}, "", 0))
	f.svc.now = func() time.Time { return f.clockAt }
	return f
}

func scanResultEnv(t *testing.T, res agentproto.ScanModelsResult) agentproto.Envelope {
	t.Helper()
	env, err := agentproto.NewEnvelope(agentproto.TypeScanModelsResult, "", res)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func stModel(ref string) agentproto.ScannedModel {
	return agentproto.ScannedModel{ModelRef: ref, Format: "safetensors", SizeBytes: 300}
}

func ggufModel(ref, quant, file string) agentproto.ScannedModel {
	return agentproto.ScannedModel{ModelRef: ref, Quantization: quant, Format: "gguf", SizeBytes: 40, FileName: file}
}

// scanAndReply starts a scan of node 1 and feeds back models as its result.
func (f *scanFixture) scanAndReply(t *testing.T, models ...agentproto.ScannedModel) string {
	t.Helper()
	id, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	if err != nil {
		t.Fatalf("StartScan: %v", err)
	}
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Models: models}))
	return id
}

func (f *scanFixture) view(t *testing.T, id string) NodeScanView {
	t.Helper()
	v, err := f.svc.ScanResult(adminActor, id)
	if err != nil {
		t.Fatalf("ScanResult: %v", err)
	}
	return v.Nodes[0]
}

func TestStartScan_RequiresAdmin(t *testing.T) {
	f := newScanFixture()
	for _, a := range []rbac.Actor{
		{Tier: db.TierPowerDev}, // even with the manage_model_store grant the fixture gives
		{Tier: db.TierDeveloper},
		{Tier: db.TierReadOnly},
	} {
		if _, err := f.svc.StartScan(context.Background(), a, []string{scanNode1}); !errors.Is(err, rbac.ErrNotPermitted) {
			t.Errorf("tier %s: err = %v, want ErrNotPermitted", a.Tier, err)
		}
	}
	if len(f.dispatch.sent) != 0 {
		t.Errorf("a refused scan must not dispatch anything: %d sent", len(f.dispatch.sent))
	}
	if _, err := f.svc.StartScan(context.Background(), superActor, []string{scanNode1}); err != nil {
		t.Errorf("SuperAdmin: %v", err)
	}
}

func TestStartScan_DispatchesToEachConnectedNode(t *testing.T) {
	f := newScanFixture()
	id, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1, strings.ToUpper(scanNode2), scanNode1})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.dispatch.sent) != 2 {
		t.Fatalf("sent %d, want 2 (duplicate node collapsed)", len(f.dispatch.sent))
	}
	var req agentproto.ScanModels
	if err := f.dispatch.sent[0].DecodePayload(&req); err != nil || req.ScanID != id || f.dispatch.sent[0].Type != agentproto.TypeScanModels {
		t.Errorf("envelope = %+v (%v), want scan_models for %s", f.dispatch.sent[0], err, id)
	}
	v, _ := f.svc.ScanResult(adminActor, id)
	if v.Complete || len(v.Nodes) != 2 || v.Nodes[1].NodeID != scanNode2 {
		t.Errorf("view = %+v, want 2 pending nodes in request order", v)
	}
}

func TestStartScan_Validation(t *testing.T) {
	f := newScanFixture()
	if _, err := f.svc.StartScan(context.Background(), adminActor, nil); !errors.Is(err, ErrNoScanNodes) {
		t.Errorf("no nodes: err = %v", err)
	}
	if _, err := f.svc.StartScan(context.Background(), adminActor, []string{"not-a-uuid"}); !errors.Is(err, ErrInvalidScanNode) {
		t.Errorf("bad id: err = %v", err)
	}
}

func TestStartScan_OfflineNode(t *testing.T) {
	f := newScanFixture()
	f.dispatch.connected = false
	id, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.view(t, id); n.Status != ScanOffline || len(f.dispatch.sent) != 0 {
		t.Errorf("node = %+v, sent=%d; want offline and nothing sent", n, len(f.dispatch.sent))
	}
	if v, _ := f.svc.ScanResult(adminActor, id); !v.Complete {
		t.Error("a scan with only offline nodes is complete")
	}
}

func TestStartScan_SendFailureMarksNodeFailed(t *testing.T) {
	f := newScanFixture()
	f.dispatch.err = errors.New("boom")
	id, _ := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	if n := f.view(t, id); n.Status != ScanFailed {
		t.Errorf("status = %s, want failed", n.Status)
	}
}

func TestStartScan_CapsLiveScansAndExpires(t *testing.T) {
	f := newScanFixture()
	for i := 0; i < maxLiveScans; i++ {
		if _, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1}); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if _, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1}); !errors.Is(err, ErrTooManyScans) {
		t.Fatalf("err = %v, want ErrTooManyScans", err)
	}
	f.clockAt = f.clockAt.Add(scanTTL + time.Second)
	if _, err := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1}); err != nil {
		t.Errorf("after TTL: %v", err)
	}
}

func TestScanResult_ExpiresAndRequiresAdmin(t *testing.T) {
	f := newScanFixture()
	id, _ := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	if _, err := f.svc.ScanResult(rbac.Actor{Tier: db.TierDeveloper}, id); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: err = %v", err)
	}
	if _, err := f.svc.ScanResult(adminActor, "nope"); !errors.Is(err, ErrScanNotFound) {
		t.Errorf("unknown id: err = %v", err)
	}
	f.clockAt = f.clockAt.Add(scanTTL + time.Second)
	if _, err := f.svc.ScanResult(adminActor, id); !errors.Is(err, ErrScanNotFound) {
		t.Errorf("expired: err = %v", err)
	}
}

func TestScanResult_PendingTimesOut(t *testing.T) {
	f := newScanFixture()
	id, _ := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	f.clockAt = f.clockAt.Add(scanReplyTimeout + time.Second)
	v, _ := f.svc.ScanResult(adminActor, id)
	if !v.Complete || v.Nodes[0].Status != ScanFailed {
		t.Errorf("view = %+v, want a failed, complete scan", v)
	}
	// A late reply is still accepted until the scan expires.
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Models: []agentproto.ScannedModel{stModel("org/late")}}))
	if n := f.view(t, id); n.Status != ScanDone || len(n.Candidates) != 1 {
		t.Errorf("late reply: %+v", n)
	}
}

func TestHandleScanModelsResult_DiffAgainstInventory(t *testing.T) {
	f := newScanFixture()
	removed := entry(scanNode1, "org/removed", "", db.ModelFormatSafetensors, 1)
	removed.Status = db.InventoryStatusRemoved
	incomplete := entry(scanNode1, "org/partial", "", db.ModelFormatSafetensors, 1)
	incomplete.Status = db.InventoryStatusIncomplete
	f.store.listResult = []*db.NodeModelInventory{
		entry(scanNode1, "org/known", "", db.ModelFormatSafetensors, 1),
		entry(scanNode2, "org/new", "", db.ModelFormatSafetensors, 1), // another node's row does not make it known here
		removed, incomplete,
	}
	id := f.scanAndReply(t, stModel("org/known"), stModel("org/new"), stModel("org/removed"), stModel("org/partial"))

	n := f.view(t, id)
	if n.Status != ScanDone || n.KnownCount != 2 {
		t.Fatalf("node = %+v, want done with 2 known (present + incomplete)", n)
	}
	if len(n.Candidates) != 2 || n.Candidates[0].ModelRef != "org/new" || n.Candidates[0].PreviouslyRemoved ||
		n.Candidates[1].ModelRef != "org/removed" || !n.Candidates[1].PreviouslyRemoved {
		t.Errorf("candidates = %+v, want org/new and a revivable org/removed", n.Candidates)
	}
}

func TestHandleScanModelsResult_IgnoresUnexpected(t *testing.T) {
	f := newScanFixture()
	id, _ := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})

	// Wrong node, unknown scan, wrong type: all ignored.
	f.svc.HandleScanModelsResult(scanNode2, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Models: []agentproto.ScannedModel{stModel("org/evil")}}))
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: "other"}))
	f.svc.HandleScanModelsResult(scanNode1, agentproto.Envelope{Type: agentproto.TypeDeleteModelResult})
	if n := f.view(t, id); n.Status != ScanPending {
		t.Fatalf("status = %s, want still pending", n.Status)
	}

	// First valid reply wins; a second one is ignored.
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Models: []agentproto.ScannedModel{stModel("org/first")}}))
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Models: []agentproto.ScannedModel{stModel("org/second")}}))
	if n := f.view(t, id); len(n.Candidates) != 1 || n.Candidates[0].ModelRef != "org/first" {
		t.Errorf("candidates = %+v, want only org/first", n.Candidates)
	}
}

func TestHandleScanModelsResult_NodeError(t *testing.T) {
	f := newScanFixture()
	id, _ := f.svc.StartScan(context.Background(), adminActor, []string{scanNode1})
	f.svc.HandleScanModelsResult(scanNode1, scanResultEnv(t, agentproto.ScanModelsResult{ScanID: id, Error: "storage unreadable"}))
	if n := f.view(t, id); n.Status != ScanFailed || n.Error != "storage unreadable" {
		t.Errorf("node = %+v", n)
	}
}

func TestHandleScanModelsResult_DropsInvalidModels(t *testing.T) {
	f := newScanFixture()
	bad := []agentproto.ScannedModel{
		stModel("/abs/path"),
		stModel("org/../../etc"),
		stModel("org//double"),
		stModel("org/ctl\nchar"),
		stModel("org\\back"),
		stModel(""),
		{ModelRef: "org/neg", Format: "safetensors", SizeBytes: -1},
		{ModelRef: "org/fmt", Format: "pickle"},
		{ModelRef: "org/stq", Format: "safetensors", Quantization: "Q4"},
		{ModelRef: "org/nofile", Format: "gguf", Quantization: "Q4_K_M"},
		ggufModel("org/slash", "Q4_K_M", "sub/x.Q4_K_M.gguf"),
		ggufModel("org/badq", "Q4 K", "x.gguf"),
		ggufModel("org/noq", "", "x.gguf"),
	}
	id := f.scanAndReply(t, append(bad, stModel("org/good"))...)
	n := f.view(t, id)
	if len(n.Candidates) != 1 || n.Candidates[0].ModelRef != "org/good" {
		t.Errorf("candidates = %+v, want only org/good", n.Candidates)
	}
}

func TestHandleScanModelsResult_CapsModels(t *testing.T) {
	f := newScanFixture()
	var many []agentproto.ScannedModel
	for i := 0; i < maxScanModels+10; i++ {
		many = append(many, stModel("org/m"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26))))
	}
	id := f.scanAndReply(t, many...)
	if n := f.view(t, id); len(n.Candidates) > maxScanModels {
		t.Errorf("%d candidates, want at most %d", len(n.Candidates), maxScanModels)
	}
}

func TestScanCandidates_BlockedWhenDirectoryShared(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t,
		ggufModel("org/mix", "UNKNOWN", "mystery.gguf"),
		ggufModel("org/mix", "Q4_K_M", "m.Q4_K_M.gguf"),
		ggufModel("org/solo", "UNKNOWN", "solo.gguf"),
	)
	n := f.view(t, id)
	by := map[string]ScanCandidate{}
	for _, c := range n.Candidates {
		by[c.ModelRef+"/"+c.Quantization] = c
	}
	if by["org/mix/UNKNOWN"].BlockedReason == "" {
		t.Error("an UNKNOWN gguf sharing a directory must be blocked")
	}
	if by["org/mix/Q4_K_M"].BlockedReason != "" {
		t.Errorf("a uniquely matching quantization is importable: %q", by["org/mix/Q4_K_M"].BlockedReason)
	}
	if by["org/solo/UNKNOWN"].BlockedReason != "" {
		t.Errorf("an UNKNOWN gguf alone in its directory is importable: %q", by["org/solo/UNKNOWN"].BlockedReason)
	}
}

func TestImport_RequiresAdmin(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t, stModel("org/new"))
	item := ImportItem{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatSafetensors}
	if _, _, err := f.svc.Import(context.Background(), rbac.Actor{Tier: db.TierPowerDev}, id, []ImportItem{item}); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("err = %v, want ErrNotPermitted", err)
	}
	if len(f.store.upserts) != 0 || len(f.audit.actions) != 0 {
		t.Error("a refused import must write and audit nothing")
	}
}

func TestImport_RecordsPresentWithNoTransferAndAudits(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t, stModel("org/new"), ggufModel("org/g", "Q4_K_M", "g.Q4_K_M.gguf"))

	n, fails, err := f.svc.Import(context.Background(), adminActor, id, []ImportItem{
		{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatSafetensors},
		{NodeID: scanNode1, ModelRef: "org/g", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF},
	})
	if err != nil || n != 2 || len(fails) != 0 {
		t.Fatalf("Import = %d, %v, %v", n, fails, err)
	}
	if len(f.store.upserts) != 2 {
		t.Fatalf("upserts = %+v", f.store.upserts)
	}
	u := f.store.upserts[0]
	if u.nodeID != scanNode1 || u.status != db.InventoryStatusPresent || u.sizeBytes != 300 || u.placedVia != "" {
		t.Errorf("upsert = %+v, want present, scanned size, no placing transfer", u)
	}
	if v := f.view(t, id); len(v.Candidates) != 0 || v.KnownCount != 2 {
		t.Errorf("after import: candidates=%+v known=%d, want none left and 2 known", v.Candidates, v.KnownCount)
	}
	if len(f.audit.actions) != 2 || f.audit.actions[0] != "imported_model" || f.audit.objIDs[0] != scanNode1 {
		t.Errorf("audit = %v %v", f.audit.actions, f.audit.objIDs)
	}
	if d := f.audit.details[1]; d["model_ref"] != "org/g" || d["quantization"] != "Q4_K_M" || d["format"] != "gguf" {
		t.Errorf("audit detail = %+v", d)
	}
}

func TestImport_ItemsComeFromTheScanNotTheRequest(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t, stModel("org/new"))

	_, fails, err := f.svc.Import(context.Background(), adminActor, id, []ImportItem{
		{NodeID: scanNode1, ModelRef: "org/never-scanned", Format: db.ModelFormatSafetensors},
		{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatGGUF},        // wrong format
		{NodeID: scanNode2, ModelRef: "org/new", Format: db.ModelFormatSafetensors}, // node not in scan
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 3 || len(f.store.upserts) != 0 {
		t.Fatalf("fails=%d upserts=%d, want 3 failures and no writes", len(fails), len(f.store.upserts))
	}
	for _, fl := range fails {
		if !errors.Is(fl.Err, ErrNotInScan) {
			t.Errorf("%+v: err = %v, want ErrNotInScan", fl.Item, fl.Err)
		}
	}
	if _, _, err := f.svc.Import(context.Background(), adminActor, "gone", nil); err != nil {
		t.Errorf("empty import of a missing scan = %v", err)
	}
	if _, _, err := f.svc.Import(context.Background(), adminActor, "gone", []ImportItem{{NodeID: scanNode1}}); !errors.Is(err, ErrScanNotFound) {
		t.Errorf("missing scan: err = %v", err)
	}
}

func TestImport_RefusesWhatAppearedSinceTheScan(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t, stModel("org/new"))
	f.store.getErr = nil
	f.store.getResult = entry(scanNode1, "org/new", "", db.ModelFormatSafetensors, 1)

	_, fails, _ := f.svc.Import(context.Background(), adminActor, id, []ImportItem{{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatSafetensors}})
	if len(fails) != 1 || !errors.Is(fails[0].Err, ErrAlreadyKnown) || len(f.store.upserts) != 0 {
		t.Errorf("fails=%+v upserts=%d, want ErrAlreadyKnown", fails, len(f.store.upserts))
	}

	// A removed row is revived, not refused.
	f.store.getResult.Status = db.InventoryStatusRemoved
	n, _, _ := f.svc.Import(context.Background(), adminActor, id, []ImportItem{{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatSafetensors}})
	if n != 1 || len(f.store.upserts) != 1 {
		t.Errorf("imported %d, upserts %d, want a revived row", n, len(f.store.upserts))
	}
}

func TestImport_QuantizationOverride(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t,
		ggufModel("org/solo", "UNKNOWN", "solo-q5km.gguf"),
		ggufModel("org/pair", "Q4_0", "pair.Q4_0.gguf"),
		ggufModel("org/pair", "Q4_0_4_4", "pair.Q4_0_4_4.gguf"),
		stModel("org/st"),
	)
	imp := func(it ImportItem) error {
		it.NodeID = scanNode1
		_, fails, err := f.svc.Import(context.Background(), adminActor, id, []ImportItem{it})
		if err != nil {
			t.Fatal(err)
		}
		if len(fails) == 0 {
			return nil
		}
		return fails[0].Err
	}

	if err := imp(ImportItem{ModelRef: "org/pair", Quantization: "Q4_0", Format: db.ModelFormatGGUF}); !errors.Is(err, ErrAmbiguousQuantization) {
		t.Errorf("Q4_0 also matches Q4_0_4_4's file: err = %v, want ErrAmbiguousQuantization", err)
	}
	if err := imp(ImportItem{ModelRef: "org/pair", Quantization: "Q4_0_4_4", Format: db.ModelFormatGGUF}); err != nil {
		t.Errorf("Q4_0_4_4 matches only its own file: %v", err)
	}
	if err := imp(ImportItem{ModelRef: "org/solo", Quantization: "UNKNOWN", Format: db.ModelFormatGGUF, AsQuantization: "Q9"}); !errors.Is(err, ErrAmbiguousQuantization) {
		t.Errorf("a label absent from the file name: err = %v", err)
	}
	if err := imp(ImportItem{ModelRef: "org/solo", Quantization: "UNKNOWN", Format: db.ModelFormatGGUF, AsQuantization: "bad label!"}); !errors.Is(err, ErrInvalidQuantization) {
		t.Errorf("malformed label: err = %v", err)
	}
	if err := imp(ImportItem{ModelRef: "org/solo", Quantization: "UNKNOWN", Format: db.ModelFormatGGUF, AsQuantization: "q5km"}); err != nil {
		t.Errorf("a label found in the file name is accepted: %v", err)
	}
	if last := f.store.upserts[len(f.store.upserts)-1]; last.quantization != "q5km" {
		t.Errorf("stored quantization = %q, want the override", last.quantization)
	}
	if err := imp(ImportItem{ModelRef: "org/st", Format: db.ModelFormatSafetensors, AsQuantization: "FP16"}); !errors.Is(err, ErrInvalidQuantization) {
		t.Errorf("safetensors is whole-repo only: err = %v", err)
	}
}

func TestImport_SharedDirectoryNeedsAQuantization(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t,
		ggufModel("org/mix", "UNKNOWN", "mystery-q8.gguf"),
		ggufModel("org/mix", "Q4_K_M", "m.Q4_K_M.gguf"),
	)
	item := ImportItem{NodeID: scanNode1, ModelRef: "org/mix", Quantization: "UNKNOWN", Format: db.ModelFormatGGUF}
	_, fails, _ := f.svc.Import(context.Background(), adminActor, id, []ImportItem{item})
	if len(fails) != 1 || !errors.Is(fails[0].Err, ErrSharedDirectory) {
		t.Fatalf("fails = %+v, want ErrSharedDirectory", fails)
	}
	item.AsQuantization = "q8"
	n, fails, _ := f.svc.Import(context.Background(), adminActor, id, []ImportItem{item})
	if n != 1 || len(fails) != 0 {
		t.Errorf("with a distinguishing quantization: imported=%d fails=%+v", n, fails)
	}
}

func TestImport_StoreAndAuditErrors(t *testing.T) {
	f := newScanFixture()
	id := f.scanAndReply(t, stModel("org/new"))
	f.store.upsertErr = errors.New("db down")
	_, fails, _ := f.svc.Import(context.Background(), adminActor, id, []ImportItem{{NodeID: scanNode1, ModelRef: "org/new", Format: db.ModelFormatSafetensors}})
	if len(fails) != 1 || len(f.audit.actions) != 0 {
		t.Errorf("fails=%d audit=%v, want one failure and no audit for a write that did not happen", len(fails), f.audit.actions)
	}
}
