// SPDX-License-Identifier: AGPL-3.0-or-later

package transfers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/rbac"
)

type fakeNodeDirectory struct {
	nodes      map[string]*db.Node
	interfaces map[string][]*db.NodeNetworkInterface
	resolveErr error
	resolved   nodes.TransferSource
	resolveArg string
}

func (f *fakeNodeDirectory) GetNode(_ context.Context, id string) (*db.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, db.ErrNodeNotFound
}

func (f *fakeNodeDirectory) ListInterfaces(_ context.Context, id string) ([]*db.NodeNetworkInterface, error) {
	return f.interfaces[id], nil
}

func (f *fakeNodeDirectory) ResolveTransferSource(_ context.Context, _, override string) (nodes.TransferSource, error) {
	f.resolveArg = override
	return f.resolved, f.resolveErr
}

type fakeSourceInventory struct {
	entry *db.NodeModelInventory
	err   error
}

func (f *fakeSourceInventory) Get(context.Context, string, string, string, db.ModelFormat) (*db.NodeModelInventory, error) {
	return f.entry, f.err
}

func sp(s string) *string { return &s }

type peerFixture struct {
	svc      *Service
	store    *fakeTransferStore
	inv      *fakeInventoryStore
	dir      *fakeNodeDirectory
	src      *fakeSourceInventory
	dispatch *fakeDispatcher
	audit    *fakeAuditRecorder
}

func newPeerFixture() *peerFixture {
	f := &peerFixture{
		store: &fakeTransferStore{nextID: "t-1"},
		inv:   &fakeInventoryStore{},
		dir: &fakeNodeDirectory{
			nodes: map[string]*db.Node{
				"dest": {ID: "dest", Name: "dest-node", IPAddress: "10.0.0.9", SSHPublicKey: sp("ssh-ed25519 AAAAdest")},
				"src":  {ID: "src", Name: "src-node", IPAddress: "10.0.0.5", SSHHostPublicKey: sp("ssh-ed25519 AAAAsrchost")},
			},
			interfaces: map[string][]*db.NodeNetworkInterface{
				"dest": {{InterfaceName: "eth0", IPAddress: "10.0.1.9"}, {InterfaceName: "eth1", IPAddress: "10.0.1.9"}},
			},
			resolved: nodes.TransferSource{Name: "eth0", IPAddress: "10.0.1.5"},
		},
		src:      &fakeSourceInventory{entry: &db.NodeModelInventory{Status: db.InventoryStatusPresent}},
		dispatch: &fakeDispatcher{connectedNodes: map[string]bool{"dest": true, "src": true}},
		audit:    &fakeAuditRecorder{},
	}
	f.svc = NewService(f.store, f.inv, &fakeOverrideStore{}, f.dispatch, f.audit, f.dir, f.src, testLogger())
	return f
}

var (
	adminActor = rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}
	peerParams = InitiateTransferParams{
		DestNodeID: "dest", ModelRef: "org/m", SourceType: db.TransferSourcePeerNode,
		SourceNodeID: "src", Quantization: "Q4_K_M", Format: db.ModelFormatGGUF,
	}
)

func TestInitiatePeer_HappyPath(t *testing.T) {
	f := newPeerFixture()
	got, err := f.svc.InitiateTransfer(context.Background(), adminActor, peerParams)
	if err != nil {
		t.Fatalf("InitiateTransfer() error: %v", err)
	}
	if got.SourceType != db.TransferSourcePeerNode || got.SourceNodeID == nil || *got.SourceNodeID != "src" {
		t.Errorf("transfer = %+v", got)
	}
	if len(f.store.formatCalls) != 1 || *f.store.formatCalls[0] != db.ModelFormatGGUF {
		t.Errorf("format not recorded: %v", f.store.formatCalls)
	}
	if len(f.store.sourceInterfaceCalls) != 0 {
		t.Errorf("no override was given, source_interface must stay NULL: %v", f.store.sourceInterfaceCalls)
	}

	if len(f.dispatch.sent) != 1 || f.dispatch.sentTo[0] != "src" || f.dispatch.sent[0].Type != agentproto.TypeAuthorizePeerPull {
		t.Fatalf("sent = %v to %v, want only authorize_peer_pull to the source (destination is told later)", f.dispatch.sent, f.dispatch.sentTo)
	}
	var auth agentproto.AuthorizePeerPull
	if err := f.dispatch.sent[0].DecodePayload(&auth); err != nil {
		t.Fatal(err)
	}
	if auth.TransferID != "t-1" || auth.DestPublicKey != "ssh-ed25519 AAAAdest" || auth.ModelRef != "org/m" || auth.Quantization != "Q4_K_M" || auth.Format != "gguf" {
		t.Errorf("authorize payload = %+v", auth)
	}
	if auth.DestIPAddress != "10.0.1.9,10.0.0.9" {
		t.Errorf("DestIPAddress = %q, want the deduplicated destination addresses", auth.DestIPAddress)
	}
	if len(f.audit.calls) != 1 || f.audit.calls[0].action != "initiated_transfer" || f.audit.calls[0].detail["source_node_id"] != "src" {
		t.Errorf("audit = %+v", f.audit.calls)
	}
}

func TestInitiatePeer_RecordsExplicitOverrideOnly(t *testing.T) {
	f := newPeerFixture()
	p := peerParams
	p.SourceInterface = "eth1"
	if _, err := f.svc.InitiateTransfer(context.Background(), adminActor, p); err != nil {
		t.Fatal(err)
	}
	if f.dir.resolveArg != "eth1" {
		t.Errorf("resolver got override %q", f.dir.resolveArg)
	}
	if len(f.store.sourceInterfaceCalls) != 1 || *f.store.sourceInterfaceCalls[0] != "eth1" {
		t.Errorf("sourceInterfaceCalls = %v", f.store.sourceInterfaceCalls)
	}
}

func TestInitiatePeer_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*peerFixture)
		params  func(InitiateTransferParams) InitiateTransferParams
		actor   rbac.Actor
		wantErr error
	}{
		{"not permitted", nil, nil, rbac.Actor{Tier: db.TierDeveloper, UserID: "d"}, rbac.ErrNotPermitted},
		{"dest offline", func(f *peerFixture) { f.dispatch.connectedNodes["dest"] = false }, nil, adminActor, ErrDestNodeOffline},
		{"source offline", func(f *peerFixture) { f.dispatch.connectedNodes["src"] = false }, nil, adminActor, ErrSourceNodeOffline},
		{"source lacks model", func(f *peerFixture) { f.src.err = db.ErrNodeModelInventoryNotFound }, nil, adminActor, ErrSourceNotPresent},
		{"source entry removed", func(f *peerFixture) { f.src.entry.Status = db.InventoryStatusRemoved }, nil, adminActor, ErrSourceNotPresent},
		{"dest has no ssh key", func(f *peerFixture) { f.dir.nodes["dest"].SSHPublicKey = nil }, nil, adminActor, ErrPeerNotReady},
		{"source has no host key", func(f *peerFixture) { f.dir.nodes["src"].SSHHostPublicKey = nil }, nil, adminActor, ErrPeerNotReady},
		{"source reports no interface", func(f *peerFixture) { f.dir.resolveErr = nodes.ErrNoInterfaces }, nil, adminActor, ErrPeerNotReady},
		{"unknown interface override", func(f *peerFixture) { f.dir.resolveErr = nodes.ErrUnknownInterface }, nil, adminActor, ErrPeerNotReady},
		{"dest has no address at all", func(f *peerFixture) { f.dir.interfaces["dest"] = nil; f.dir.nodes["dest"].IPAddress = "not-an-ip" }, nil, adminActor, ErrPeerNotReady},
		{"self transfer", nil, func(p InitiateTransferParams) InitiateTransferParams { p.SourceNodeID = "dest"; return p }, adminActor, ErrInvalidTransfer},
		{"bad format", nil, func(p InitiateTransferParams) InitiateTransferParams { p.Format = "onnx"; return p }, adminActor, ErrInvalidTransfer},
		{"missing source id", nil, func(p InitiateTransferParams) InitiateTransferParams { p.SourceNodeID = ""; return p }, adminActor, ErrInvalidTransfer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPeerFixture()
			if tt.mutate != nil {
				tt.mutate(f)
			}
			p := peerParams
			if tt.params != nil {
				p = tt.params(p)
			}
			_, err := f.svc.InitiateTransfer(context.Background(), tt.actor, p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if len(f.store.created) != 0 || len(f.dispatch.sent) != 0 {
				t.Errorf("a refused transfer must not persist or dispatch anything (created=%d sent=%d)", len(f.store.created), len(f.dispatch.sent))
			}
		})
	}
}

func TestInitiatePeer_DisabledWithoutDependencies(t *testing.T) {
	svc := NewService(&fakeTransferStore{}, &fakeInventoryStore{}, &fakeOverrideStore{}, &fakeDispatcher{connected: true}, &fakeAuditRecorder{}, nil, nil, testLogger())
	_, err := svc.InitiateTransfer(context.Background(), adminActor, peerParams)
	if !errors.Is(err, ErrInvalidTransfer) {
		t.Errorf("error = %v, want ErrInvalidTransfer", err)
	}
}

func queuedPeerTransfer() *db.ModelTransfer {
	format := db.ModelFormatGGUF
	return &db.ModelTransfer{
		ID: "t-1", DestNodeID: "dest", ModelRef: "org/m", SourceType: db.TransferSourcePeerNode,
		SourceNodeID: sp("src"), Status: db.TransferStatusQueued, Quantization: sp("Q4_K_M"), Format: &format,
	}
}

func authResult(t *testing.T, accepted bool, reason string) agentproto.Envelope {
	return newEnvelope(t, agentproto.TypePeerAuthorizeResult, agentproto.PeerAuthorizeResult{TransferID: "t-1", Accepted: accepted, Reason: reason})
}

func TestHandlePeerAuthorizeResult_AcceptedStartsDestination(t *testing.T) {
	f := newPeerFixture()
	f.store.findByIDResult = queuedPeerTransfer()
	f.svc.HandlePeerAuthorizeResult("src", authResult(t, true, ""))

	if len(f.dispatch.sent) != 1 || f.dispatch.sentTo[0] != "dest" || f.dispatch.sent[0].Type != agentproto.TypeStartPeerTransfer {
		t.Fatalf("sent = %v to %v, want start_peer_transfer to the destination", f.dispatch.sent, f.dispatch.sentTo)
	}
	var start agentproto.StartPeerTransfer
	if err := f.dispatch.sent[0].DecodePayload(&start); err != nil {
		t.Fatal(err)
	}
	want := agentproto.StartPeerTransfer{
		TransferID: "t-1", SourceNodeID: "src", SourceHost: "10.0.1.5", SourceSSHPort: 22,
		SourceHostPublicKey: "ssh-ed25519 AAAAsrchost", ModelRef: "org/m", Quantization: "Q4_K_M", Format: "gguf",
	}
	if start != want {
		t.Errorf("start = %+v, want %+v", start, want)
	}
	if len(f.store.statusCalls) != 0 {
		t.Errorf("status must not change on accept (the destination reports progress): %+v", f.store.statusCalls)
	}
}

func TestHandlePeerAuthorizeResult_RejectedFailsAndRevokes(t *testing.T) {
	f := newPeerFixture()
	f.store.findByIDResult = queuedPeerTransfer()
	f.svc.HandlePeerAuthorizeResult("src", authResult(t, false, "model path missing"))

	if len(f.store.statusCalls) != 1 || f.store.statusCalls[0].status != db.TransferStatusFailed || !strings.Contains(*f.store.statusCalls[0].errMsg, "model path missing") {
		t.Errorf("statusCalls = %+v, want failed with the source's reason", f.store.statusCalls)
	}
	for _, to := range f.dispatch.sentTo {
		if to == "dest" {
			t.Error("the destination must never be told to start after a rejection")
		}
	}
}

func TestHandlePeerAuthorizeResult_IgnoredUnlessFromRecordedSourceAndQueued(t *testing.T) {
	tests := []struct {
		name   string
		from   string
		mutate func(*db.ModelTransfer)
	}{
		{"wrong node", "dest", nil},
		{"third node", "other", nil},
		{"not queued", "src", func(t *db.ModelTransfer) { t.Status = db.TransferStatusTransferring }},
		{"internet transfer", "src", func(t *db.ModelTransfer) { t.SourceType = db.TransferSourceInternet }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPeerFixture()
			tr := queuedPeerTransfer()
			if tt.mutate != nil {
				tt.mutate(tr)
			}
			f.store.findByIDResult = tr
			f.svc.HandlePeerAuthorizeResult(tt.from, authResult(t, true, ""))
			if len(f.dispatch.sent) != 0 || len(f.store.statusCalls) != 0 {
				t.Errorf("acted on an illegitimate authorize result: sent=%v status=%v", f.dispatch.sent, f.store.statusCalls)
			}
		})
	}
}

func TestHandlePeerAuthorizeResult_DestDisconnectedFailsAndRevokes(t *testing.T) {
	f := newPeerFixture()
	f.store.findByIDResult = queuedPeerTransfer()
	f.dispatch.connectedNodes["dest"] = false
	f.svc.HandlePeerAuthorizeResult("src", authResult(t, true, ""))

	if len(f.store.statusCalls) != 1 || f.store.statusCalls[0].status != db.TransferStatusFailed {
		t.Fatalf("statusCalls = %+v", f.store.statusCalls)
	}
	if len(f.dispatch.sent) != 1 || f.dispatch.sent[0].Type != agentproto.TypeRevokePeerPull || f.dispatch.sentTo[0] != "src" {
		t.Errorf("sent = %v to %v, want a revoke to the source", f.dispatch.sent, f.dispatch.sentTo)
	}
}

func TestHandleTransferProgress_PeerTerminalStatusRevokesSourceGrant(t *testing.T) {
	for _, status := range []db.TransferStatus{db.TransferStatusCompleted, db.TransferStatusFailed, db.TransferStatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := newPeerFixture()
			tr := queuedPeerTransfer()
			tr.Status = db.TransferStatusTransferring
			f.store.findByIDResult = tr
			f.svc.HandleTransferProgress("dest", newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{TransferID: "t-1", BytesTotal: 10, Status: string(status)}))
			if len(f.dispatch.sent) != 1 || f.dispatch.sent[0].Type != agentproto.TypeRevokePeerPull || f.dispatch.sentTo[0] != "src" {
				t.Errorf("sent = %v to %v, want revoke_peer_pull to the source", f.dispatch.sent, f.dispatch.sentTo)
			}
		})
	}
}

func TestHandleTransferProgress_PeerNonTerminalDoesNotRevoke(t *testing.T) {
	f := newPeerFixture()
	tr := queuedPeerTransfer()
	f.store.findByIDResult = tr
	f.svc.HandleTransferProgress("dest", newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{TransferID: "t-1", Status: string(db.TransferStatusTransferring)}))
	if len(f.dispatch.sent) != 0 {
		t.Errorf("sent = %v, want nothing while transferring", f.dispatch.sent)
	}
}

func TestHandleTransferProgress_PeerCompletedUsesRecordedFormat(t *testing.T) {
	f := newPeerFixture()
	tr := queuedPeerTransfer()
	format := db.ModelFormatSafetensors
	tr.Format, tr.Quantization = &format, sp("FP16")
	f.store.findByIDResult = tr
	f.svc.HandleTransferProgress("dest", newEnvelope(t, agentproto.TypeTransferProgress, agentproto.TransferProgress{TransferID: "t-1", BytesTotal: 99, Status: string(db.TransferStatusCompleted)}))
	if len(f.inv.calls) != 1 {
		t.Fatalf("upserts = %d", len(f.inv.calls))
	}
	if got := f.inv.calls[0]; got.format != db.ModelFormatSafetensors || got.quantization != "FP16" || got.nodeID != "dest" {
		t.Errorf("upsert = %+v, want the source's real format (safetensors) despite a non-empty quantization", got)
	}
}

func TestCheckConnectivity_FlowAndAuthorization(t *testing.T) {
	f := newPeerFixture()
	ctx := context.Background()

	if _, err := f.svc.CheckConnectivity(ctx, rbac.Actor{Tier: db.TierDeveloper, UserID: "d"}, "dest", "src", ""); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer error = %v", err)
	}
	if _, err := f.svc.CheckConnectivity(ctx, adminActor, "dest", "dest", ""); !errors.Is(err, ErrInvalidTransfer) {
		t.Errorf("self check error = %v", err)
	}

	id, err := f.svc.CheckConnectivity(ctx, adminActor, "dest", "src", "eth0")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.dispatch.sent) != 1 || f.dispatch.sentTo[0] != "dest" || f.dispatch.sent[0].Type != agentproto.TypeCheckPeerConnectivity {
		t.Fatalf("sent = %v to %v", f.dispatch.sent, f.dispatch.sentTo)
	}
	var check agentproto.CheckPeerConnectivity
	if err := f.dispatch.sent[0].DecodePayload(&check); err != nil {
		t.Fatal(err)
	}
	if check.CheckID != id || check.SourceHost != "10.0.1.5" || check.SourceSSHPort != 22 {
		t.Errorf("check = %+v", check)
	}

	if res, ok := f.svc.GetConnectivityResult(id); !ok || res != nil {
		t.Errorf("pending check = (%v, %v), want (nil, true)", res, ok)
	}
	if _, ok := f.svc.GetConnectivityResult("unknown"); ok {
		t.Error("unknown check id reported as known")
	}

	reply := newEnvelope(t, agentproto.TypeConnectivityCheckResult, agentproto.ConnectivityCheckResult{CheckID: id, Reachable: true, LatencyMs: 3})
	f.svc.HandleConnectivityCheckResult("src", reply)
	if res, _ := f.svc.GetConnectivityResult(id); res != nil {
		t.Error("a result from a node the check was not sent to must be ignored")
	}
	f.svc.HandleConnectivityCheckResult("dest", reply)
	res, _ := f.svc.GetConnectivityResult(id)
	if res == nil || !res.Reachable || res.LatencyMs != 3 {
		t.Fatalf("result = %+v", res)
	}
	f.svc.HandleConnectivityCheckResult("dest", newEnvelope(t, agentproto.TypeConnectivityCheckResult, agentproto.ConnectivityCheckResult{CheckID: id, Reachable: false, Reason: "late"}))
	if res, _ := f.svc.GetConnectivityResult(id); !res.Reachable {
		t.Error("a check may only be answered once")
	}
}

func TestCheckConnectivity_DestOffline(t *testing.T) {
	f := newPeerFixture()
	f.dispatch.connectedNodes["dest"] = false
	if _, err := f.svc.CheckConnectivity(context.Background(), adminActor, "dest", "src", ""); !errors.Is(err, ErrDestNodeOffline) {
		t.Errorf("error = %v", err)
	}
}
