// SPDX-License-Identifier: AGPL-3.0-or-later

package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// Scan and import: an Admin asks nodes what model copies they hold on disk,
// sees which of those Node model inventory does not know about, and adopts
// the ones they choose. Nothing here trusts the node's answer beyond what a
// human then confirms - format and quantization are guesses from file
// names (agent/modelscan), completeness is unknowable, and every field a
// node sends is re-validated before it is shown or stored.

var (
	// ErrScanNotFound is returned for an unknown or expired scan id.
	ErrScanNotFound = errors.New("scan not found or expired")
	// ErrTooManyScans is returned when too many unexpired scans are held.
	ErrTooManyScans = errors.New("too many recent scans; wait a few minutes and try again")
	// ErrNoScanNodes is returned when a scan names no (valid) nodes.
	ErrNoScanNodes = errors.New("select at least one node to scan")
	// ErrInvalidScanNode is returned for a node id that is not a uuid.
	ErrInvalidScanNode = errors.New("invalid node id")
	// ErrNotInScan is returned by Import for a model the scan did not find
	// (or that was not offered as unknown) - import takes its facts from the
	// stored scan, never from the request.
	ErrNotInScan = errors.New("model was not found in this scan")
	// ErrAlreadyKnown is returned by Import when inventory gained an entry
	// for the model since the scan ran.
	ErrAlreadyKnown = errors.New("inventory already has this model")
	// ErrInvalidQuantization is returned for a quantization override that
	// is malformed, or not allowed for the model's format.
	ErrInvalidQuantization = errors.New("invalid quantization")
	// ErrSharedDirectory is returned when a model whose delete would remove
	// its whole directory (whole-repo or UNKNOWN quantization) shares that
	// directory with other models on the node.
	ErrSharedDirectory = errors.New("shares a directory with other models; deleting it later would remove them too - give it a quantization that appears in its file name")
	// ErrAmbiguousQuantization is returned when a quantization label does
	// not pick out exactly this model's gguf file in its directory.
	ErrAmbiguousQuantization = errors.New("quantization does not match exactly this model's file in its directory")
)

// Per-node scan states reported in NodeScanView.Status.
const (
	ScanPending = "pending"
	ScanDone    = "done"
	ScanOffline = "offline"
	ScanFailed  = "failed"
)

const (
	scanTTL = 15 * time.Minute
	// scanReplyTimeout is how long a pending node is waited for before the
	// page reports it as failed. A late reply is still accepted until
	// scanTTL, so a slow disk is not lost, only no longer waited on.
	scanReplyTimeout = 2 * time.Minute
	maxScanNodes     = 50
	maxLiveScans     = 100
	maxScanModels    = 500
	maxRefLen        = 512
	maxQuantLen      = 32
)

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	quantRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)
)

// ScanCandidate is a model copy a node holds that inventory does not know.
type ScanCandidate struct {
	ModelRef     string
	Quantization string
	Format       db.ModelFormat
	SizeBytes    int64
	// FileName is the gguf file the quantization was read from; empty for
	// safetensors.
	FileName           string
	PossiblyIncomplete bool
	// PreviouslyRemoved means inventory has a removed row for this model
	// (an earlier in-app delete); importing revives it.
	PreviouslyRemoved bool
	// BlockedReason is non-empty when the model cannot be imported as
	// scanned (see ErrSharedDirectory, ErrAmbiguousQuantization). For gguf,
	// importing with a different quantization can clear it.
	BlockedReason string
}

// NodeScanView is one node's part of a scan, safe to hand to the UI.
type NodeScanView struct {
	NodeID string
	Status string
	// Error explains ScanOffline/ScanFailed.
	Error string
	// KnownCount is how many of the node's models inventory already has.
	KnownCount int
	Truncated  bool
	Candidates []ScanCandidate
}

// ScanView is a snapshot of a scan.
type ScanView struct {
	ID    string
	Nodes []NodeScanView
	// Complete is true once no node is still pending.
	Complete bool
}

type nodeScanState struct {
	status     string
	err        string
	truncated  bool
	known      int
	models     []agentproto.ScannedModel // every valid model the node reported
	candidates []ScanCandidate
}

type scanState struct {
	at    time.Time
	order []string
	nodes map[string]*nodeScanState
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// CanImport reports whether actor may scan and import - exported so
// internal/httpapi can decide whether to show the action at all, not a
// security boundary (every method here re-checks).
func (s *Service) CanImport(actor rbac.Actor) bool {
	return rbac.CanImportModels(actor)
}

// pruneScansLocked drops expired scans. Caller holds scansMu.
func (s *Service) pruneScansLocked() {
	for id, sc := range s.scans {
		if s.clock().Sub(sc.at) > scanTTL {
			delete(s.scans, id)
		}
	}
}

func newScanID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate scan id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StartScan asks each named node to list the model copies on its disk and
// returns the scan id to poll with ScanResult. A node with no live agent
// connection is recorded as offline rather than failing the whole scan.
// Reading is not audited (like a rescan of a node's interfaces); the
// resulting Import is.
func (s *Service) StartScan(ctx context.Context, actor rbac.Actor, nodeIDs []string) (string, error) {
	if !rbac.CanImportModels(actor) {
		return "", rbac.ErrNotPermitted
	}

	seen := make(map[string]bool)
	var ids []string
	for _, raw := range nodeIDs {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if !uuidRe.MatchString(id) {
			return "", ErrInvalidScanNode
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", ErrNoScanNodes
	}
	if len(ids) > maxScanNodes {
		return "", fmt.Errorf("scan at most %d nodes at once", maxScanNodes)
	}

	scanID, err := newScanID()
	if err != nil {
		return "", err
	}
	sc := &scanState{at: s.clock(), order: ids, nodes: make(map[string]*nodeScanState, len(ids))}
	for _, id := range ids {
		sc.nodes[id] = &nodeScanState{status: ScanPending}
	}

	s.scansMu.Lock()
	s.pruneScansLocked()
	if len(s.scans) >= maxLiveScans {
		s.scansMu.Unlock()
		return "", ErrTooManyScans
	}
	if s.scans == nil {
		s.scans = make(map[string]*scanState)
	}
	s.scans[scanID] = sc
	s.scansMu.Unlock()

	env, err := agentproto.NewEnvelope(agentproto.TypeScanModels, "", agentproto.ScanModels{ScanID: scanID})
	if err != nil {
		return "", fmt.Errorf("build scan_models envelope: %w", err)
	}
	for _, id := range ids {
		if !s.dispatch.Connected(id) {
			s.finishNode(scanID, id, func(n *nodeScanState) {
				n.status, n.err = ScanOffline, "node is not connected"
			})
			continue
		}
		if err := s.dispatch.Send(ctx, id, env); err != nil {
			s.logger.Printf("inventory: dispatch scan_models to node %s: %v", id, err)
			s.finishNode(scanID, id, func(n *nodeScanState) {
				n.status, n.err = ScanFailed, "could not reach the node"
			})
		}
	}
	return scanID, nil
}

// finishNode applies update to a node's state if the scan still exists and
// the node is still pending.
func (s *Service) finishNode(scanID, nodeID string, update func(*nodeScanState)) bool {
	s.scansMu.Lock()
	defer s.scansMu.Unlock()
	sc, ok := s.scans[scanID]
	if !ok {
		return false
	}
	n, ok := sc.nodes[nodeID]
	if !ok || n.status != ScanPending {
		return false
	}
	update(n)
	return true
}

// HandleScanModelsResult implements agentconn.OnMessageFunc for
// agentproto.TypeScanModelsResult. nodeID is the sending connection's
// authenticated identity: a result is accepted only for a scan that
// is still live and that is still waiting on exactly this node, once.
func (s *Service) HandleScanModelsResult(nodeID string, env agentproto.Envelope) {
	if env.Type != agentproto.TypeScanModelsResult {
		return
	}
	var result agentproto.ScanModelsResult
	if err := env.DecodePayload(&result); err != nil {
		s.logger.Printf("inventory: node %s sent a malformed scan_models_result: %v", nodeID, err)
		return
	}
	if !s.scanWaitingOn(result.ScanID, nodeID) {
		s.logger.Printf("inventory: ignoring scan_models_result from node %s for scan %q that is not waiting on it", nodeID, result.ScanID)
		return
	}

	if result.Error != "" {
		reason := result.Error
		if len(reason) > maxFailureLen {
			reason = reason[:maxFailureLen]
		}
		s.finishNode(result.ScanID, nodeID, func(n *nodeScanState) {
			n.status, n.err = ScanFailed, reason
		})
		return
	}

	models, dropped := validScannedModels(result.Models)
	if dropped > 0 {
		s.logger.Printf("inventory: dropped %d invalid model entries from node %s's scan", dropped, nodeID)
	}

	entries, err := s.inventory.List(context.Background())
	if err != nil {
		s.logger.Printf("inventory: list inventory for scan of node %s: %v", nodeID, err)
		s.finishNode(result.ScanID, nodeID, func(n *nodeScanState) {
			n.status, n.err = ScanFailed, "could not read inventory"
		})
		return
	}
	known := make(map[string]bool)
	removed := make(map[string]bool)
	for _, e := range entries {
		if e.NodeID != nodeID {
			continue
		}
		k := deleteKey(nodeID, e.ModelRef, e.Quantization, e.Format)
		if e.Status == db.InventoryStatusRemoved {
			removed[k] = true
		} else {
			known[k] = true
		}
	}

	var candidates []ScanCandidate
	knownCount := 0
	for _, m := range models {
		k := deleteKey(nodeID, m.ModelRef, m.Quantization, db.ModelFormat(m.Format))
		if known[k] {
			knownCount++
			continue
		}
		c := ScanCandidate{
			ModelRef:           m.ModelRef,
			Quantization:       m.Quantization,
			Format:             db.ModelFormat(m.Format),
			SizeBytes:          m.SizeBytes,
			FileName:           m.FileName,
			PossiblyIncomplete: m.PossiblyIncomplete,
			PreviouslyRemoved:  removed[k],
		}
		if err := importBlock(models, m, m.Quantization); err != nil {
			c.BlockedReason = err.Error()
		}
		candidates = append(candidates, c)
	}

	s.finishNode(result.ScanID, nodeID, func(n *nodeScanState) {
		n.status = ScanDone
		n.truncated = result.Truncated
		n.known = knownCount
		n.models = models
		n.candidates = candidates
	})
}

func (s *Service) scanWaitingOn(scanID, nodeID string) bool {
	s.scansMu.Lock()
	defer s.scansMu.Unlock()
	sc, ok := s.scans[scanID]
	if !ok || s.clock().Sub(sc.at) > scanTTL {
		return false
	}
	n, ok := sc.nodes[nodeID]
	return ok && n.status == ScanPending
}

// validScannedModels keeps only entries safe to show and later import,
// returning how many were dropped. Everything here arrives from a node, so
// nothing is assumed: refs must be relative and traversal-free, formats and
// quantizations must be known shapes.
func validScannedModels(in []agentproto.ScannedModel) (out []agentproto.ScannedModel, dropped int) {
	for _, m := range in {
		if len(out) >= maxScanModels || !validScannedModel(m) {
			dropped++
			continue
		}
		out = append(out, m)
	}
	return out, dropped
}

func validScannedModel(m agentproto.ScannedModel) bool {
	if m.ModelRef == "" || len(m.ModelRef) > maxRefLen || !utf8.ValidString(m.ModelRef) || strings.HasPrefix(m.ModelRef, "/") {
		return false
	}
	for _, r := range m.ModelRef {
		if unicode.IsControl(r) || r == '\\' {
			return false
		}
	}
	for _, seg := range strings.Split(m.ModelRef, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	if m.SizeBytes < 0 {
		return false
	}
	switch db.ModelFormat(m.Format) {
	case db.ModelFormatSafetensors:
		return m.Quantization == "" && m.FileName == ""
	case db.ModelFormatGGUF:
		return validQuantization(m.Quantization) && m.Quantization != "" &&
			m.FileName != "" && !strings.ContainsAny(m.FileName, "/\\") && utf8.ValidString(m.FileName)
	default:
		return false
	}
}

func validQuantization(q string) bool {
	return q == "" || (len(q) <= maxQuantLen && quantRe.MatchString(q))
}

// importBlock reports why m cannot be imported under quantization q, or nil.
// all is every valid model the node reported (known or not), because the
// danger is to its neighbours: the agent's delete removes a whole directory
// for a whole-repo or UNKNOWN entry, and removes every file whose name
// contains q for a quantized gguf one.
func importBlock(all []agentproto.ScannedModel, m agentproto.ScannedModel, q string) error {
	siblings := 0
	for _, o := range all {
		if o.ModelRef == m.ModelRef && !(o.Format == m.Format && o.FileName == m.FileName) {
			siblings++
		}
	}
	wholeDir := q == "" || q == "UNKNOWN"
	if wholeDir {
		if siblings > 0 {
			return ErrSharedDirectory
		}
		return nil
	}
	if m.Format != string(db.ModelFormatGGUF) || !strings.Contains(m.FileName, q) {
		return ErrAmbiguousQuantization
	}
	matches := 0
	for _, o := range all {
		if o.ModelRef == m.ModelRef && o.Format == string(db.ModelFormatGGUF) && strings.Contains(o.FileName, q) {
			matches++
		}
	}
	if matches != 1 {
		return ErrAmbiguousQuantization
	}
	return nil
}

// ScanResult returns a snapshot of a scan for the page to render or poll.
func (s *Service) ScanResult(actor rbac.Actor, scanID string) (*ScanView, error) {
	if !rbac.CanImportModels(actor) {
		return nil, rbac.ErrNotPermitted
	}
	s.scansMu.Lock()
	defer s.scansMu.Unlock()
	s.pruneScansLocked()
	sc, ok := s.scans[scanID]
	if !ok {
		return nil, ErrScanNotFound
	}

	view := &ScanView{ID: scanID, Complete: true}
	for _, id := range sc.order {
		n := sc.nodes[id]
		nv := NodeScanView{NodeID: id, Status: n.status, Error: n.err, KnownCount: n.known, Truncated: n.truncated}
		if n.status == ScanPending && s.clock().Sub(sc.at) > scanReplyTimeout {
			nv.Status, nv.Error = ScanFailed, "no reply from the node"
		}
		if nv.Status == ScanPending {
			view.Complete = false
		}
		nv.Candidates = append([]ScanCandidate(nil), n.candidates...)
		view.Nodes = append(view.Nodes, nv)
	}
	return view, nil
}

// ImportItem names one scanned model to import. ModelRef, Quantization and
// Format identify it within the scan exactly as listed; AsQuantization, if
// set, imports a gguf under a different quantization label than the
// filename-based guess.
type ImportItem struct {
	NodeID         string
	ModelRef       string
	Quantization   string
	Format         db.ModelFormat
	AsQuantization string
}

// ImportFailure is one item Import did not import, and why.
type ImportFailure struct {
	Item ImportItem
	Err  error
}

// Import adds the chosen models from a scan to inventory as present, with no
// placing transfer. Each item is checked against the stored scan (size and
// file name come from there, never from the request) and imported
// independently: failures are returned per item and do not stop the rest.
// Every import is audited as "imported_model" against the node.
func (s *Service) Import(ctx context.Context, actor rbac.Actor, scanID string, items []ImportItem) (imported int, failures []ImportFailure, err error) {
	if !rbac.CanImportModels(actor) {
		return 0, nil, rbac.ErrNotPermitted
	}
	for _, item := range items {
		if ierr := s.importOne(ctx, actor, scanID, item); ierr != nil {
			if errors.Is(ierr, ErrScanNotFound) {
				return imported, failures, ierr
			}
			failures = append(failures, ImportFailure{Item: item, Err: ierr})
			continue
		}
		imported++
	}
	return imported, failures, nil
}

func (s *Service) importOne(ctx context.Context, actor rbac.Actor, scanID string, item ImportItem) error {
	item.NodeID = strings.ToLower(strings.TrimSpace(item.NodeID))
	cand, models, err := s.findScanned(scanID, item)
	if err != nil {
		return err
	}

	q := cand.Quantization
	if item.AsQuantization != "" {
		if cand.Format != db.ModelFormatGGUF || !validQuantization(item.AsQuantization) {
			return ErrInvalidQuantization
		}
		q = item.AsQuantization
	}
	var scanned agentproto.ScannedModel
	for _, m := range models {
		if m.ModelRef == cand.ModelRef && m.Format == string(cand.Format) && m.FileName == cand.FileName && m.Quantization == cand.Quantization {
			scanned = m
			break
		}
	}
	if err := importBlock(models, scanned, q); err != nil {
		return err
	}

	existing, err := s.inventory.Get(ctx, item.NodeID, cand.ModelRef, q, cand.Format)
	switch {
	case err == nil && existing.Status != db.InventoryStatusRemoved:
		return ErrAlreadyKnown
	case err == nil, errors.Is(err, db.ErrNodeModelInventoryNotFound):
	default:
		return fmt.Errorf("check existing inventory entry: %w", err)
	}

	if _, err := s.inventory.Upsert(ctx, item.NodeID, cand.ModelRef, q, cand.Format, db.InventoryStatusPresent, cand.SizeBytes, ""); err != nil {
		return fmt.Errorf("record imported model: %w", err)
	}

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	detail := map[string]any{
		"model_ref":           cand.ModelRef,
		"quantization":        q,
		"format":              string(cand.Format),
		"size_bytes":          cand.SizeBytes,
		"possibly_incomplete": cand.PossiblyIncomplete,
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "imported_model", "node", item.NodeID, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// findScanned returns the stored candidate an item names, plus the node's
// full model list for the safety checks.
func (s *Service) findScanned(scanID string, item ImportItem) (ScanCandidate, []agentproto.ScannedModel, error) {
	s.scansMu.Lock()
	defer s.scansMu.Unlock()
	sc, ok := s.scans[scanID]
	if !ok || s.clock().Sub(sc.at) > scanTTL {
		return ScanCandidate{}, nil, ErrScanNotFound
	}
	n, ok := sc.nodes[item.NodeID]
	if !ok || n.status != ScanDone {
		return ScanCandidate{}, nil, ErrNotInScan
	}
	for _, c := range n.candidates {
		if c.ModelRef == item.ModelRef && c.Quantization == item.Quantization && c.Format == item.Format {
			return c, append([]agentproto.ScannedModel(nil), n.models...), nil
		}
	}
	return ScanCandidate{}, nil, ErrNotInScan
}
