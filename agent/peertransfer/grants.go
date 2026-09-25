// SPDX-License-Identifier: AGPL-3.0-or-later

// Package peertransfer implements the node-to-node model transfer
// mechanism: rsync over OpenSSH, pull-based. The destination pulls (an
// outbound connection, same shape as a Hugging Face download); the source
// briefly accepts one inbound SSH connection - the single, scoped
// exception to ARCHITECTURE.md's "zero inbound network exposure"
// constraint, whose full threat model lives in ARCHITECTURE.md Security
// Considerations.
//
// The source-side authorization is a grant file, not an edit to any
// authorized_keys file. The central app asks the source agent (over the
// existing trusted WebSocket) to authorize exactly one transfer; the agent
// writes a small JSON grant into GrantDir. sshd's AuthorizedKeysCommand
// (`sparky-agent peer-authkeys`) turns every unexpired grant into one
// restricted authorized_keys line on demand, and the forced command
// (`sparky-agent peer-serve <id>`) re-validates and consumes the grant
// before exec'ing rsync. Because the lines are computed from grant files
// at connect time, expiry needs no cleanup to be effective, there is no
// shared file to corrupt, and a crashed agent can never leave a live
// authorization behind.
package peertransfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/agent/modelpath"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

const (
	// GrantDir holds per-transfer grants. A fixed path, not configuration:
	// sshd invokes peer-authkeys / peer-serve with a scrubbed environment,
	// so they cannot read the agent's env vars. Owned by serviceloop
	// (the agent writes), group sparky-peer (peer-serve reads), setgid.
	GrantDir = "/opt/sparky/serviceloop/peer-grants"

	// UsedDir holds one empty marker per grant that has been used. It
	// exists because single use must be recorded by peer-serve, which runs
	// as PeerUser and deliberately has no write access to GrantDir (it must
	// not be able to create or alter grants). PeerUser owns this directory
	// and can only add markers here; the agent (group serviceloop) reads
	// and clears them.
	UsedDir = "/opt/sparky/serviceloop/peer-used"

	// PeerUser is the dedicated account the destination authenticates as.
	PeerUser = "sparky-peer"

	// AgentBinary is the installed agent, referenced from authorized_keys
	// forced commands and the sshd drop-in.
	AgentBinary = "/opt/sparky/bin/sparky-agent"

	// DefaultAuthTTL bounds how long a grant stays usable if never revoked.
	DefaultAuthTTL = 2 * time.Hour

	maxDestIPs = 16
)

var transferIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ValidTransferID reports whether id is safe to use as a grant file name
// and inside an authorized_keys command line.
func ValidTransferID(id string) bool { return transferIDPattern.MatchString(id) }

// Grant is one authorized pull, as stored in GrantDir/<id>.json.
type Grant struct {
	TransferID    string    `json:"transfer_id"`
	DestPublicKey string    `json:"dest_public_key"`
	DestIPs       []string  `json:"dest_ips"`
	ModelDir      string    `json:"model_dir"`
	Files         []string  `json:"files,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ErrNoGrant is returned when no grant exists for a transfer ID.
var ErrNoGrant = errors.New("no such peer-transfer grant")

// Authorizer creates and removes grants on the source node.
type Authorizer struct {
	dir         string
	usedDir     string
	storageRoot string
	ttl         time.Duration
	now         func() time.Time
}

// NewAuthorizer returns an Authorizer writing grants into dir (and clearing
// used-markers from usedDir) for models under storageRoot. ttl <= 0 means
// DefaultAuthTTL.
func NewAuthorizer(dir, usedDir, storageRoot string, ttl time.Duration) *Authorizer {
	if ttl <= 0 {
		ttl = DefaultAuthTTL
	}
	return &Authorizer{dir: dir, usedDir: usedDir, storageRoot: storageRoot, ttl: ttl, now: time.Now}
}

// ParseDestIPs validates the comma-separated destination address list.
// Every entry must be a literal IP - no hostnames, wildcards, or CIDRs,
// since the result goes into an authorized_keys from= option.
func ParseDestIPs(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("no destination address")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxDestIPs {
		return nil, fmt.Errorf("more than %d destination addresses", maxDestIPs)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		ip := net.ParseIP(p)
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, fmt.Errorf("invalid destination address %q", p)
		}
		out = append(out, ip.String())
	}
	return out, nil
}

// resolveSource works out which directory and files a grant covers.
// Safetensors, and GGUF entries with no concrete quantization ("" or
// "UNKNOWN"), cover the whole model directory; a GGUF entry with a
// quantization covers only the .gguf file(s) whose name contains it, so
// sibling quantizations are never exposed.
func (a *Authorizer) resolveSource(modelRef, quantization, format string) (dir string, files []string, err error) {
	dir, err = modelpath.Resolve(a.storageRoot, modelRef)
	if err != nil {
		return "", nil, err
	}
	// The lexical check above is not enough here: a symlink inside model
	// storage must not let a grant serve a directory elsewhere.
	realRoot, err := filepath.EvalSymlinks(a.storageRoot)
	if err != nil {
		return "", nil, fmt.Errorf("resolve model storage: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("model directory: %w", err)
	}
	if rel, err := filepath.Rel(realRoot, realDir); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("model directory resolves outside model storage")
	}
	if info, err := os.Stat(realDir); err != nil || !info.IsDir() {
		return "", nil, fmt.Errorf("model directory is not a directory")
	}

	if format == "gguf" && quantization != "" && quantization != "UNKNOWN" {
		matches, err := filepath.Glob(filepath.Join(realDir, "*.gguf"))
		if err != nil {
			return "", nil, fmt.Errorf("glob .gguf files: %w", err)
		}
		for _, m := range matches {
			base := filepath.Base(m)
			if !strings.Contains(base, quantization) {
				continue
			}
			// A basename comes from a third-party repository and ends up as
			// an rsync argument (see PrepareServe), so a name that could be
			// read as an option, or that contains anything unusual, is
			// refused outright rather than served.
			if !SafeFileName(base) {
				return "", nil, fmt.Errorf("model file %q has a name that cannot be served safely", base)
			}
			files = append(files, base)
		}
		if len(files) == 0 {
			return "", nil, fmt.Errorf("no .gguf file matching quantization %q", quantization)
		}
	}
	return realDir, files, nil
}

var safeFileName = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._+=,@%-]*$`)

// SafeFileName reports whether a model file's basename is safe to hand to
// rsync as a source argument: it must not start with "-" (which rsync would
// parse as an option), and is limited to a conservative character set - no
// spaces, quotes, slashes, or control characters.
func SafeFileName(name string) bool {
	return name != "." && name != ".." && len(name) <= 255 && safeFileName.MatchString(name)
}

// Authorize validates req and writes its grant. Every field is validated
// here even though the central app already did - this agent is the last
// line before an SSH key is trusted. The model path is resolved from
// ModelRef/Quantization/Format locally; the wire never supplies a path.
func (a *Authorizer) Authorize(req agentproto.AuthorizePeerPull) error {
	if !ValidTransferID(req.TransferID) {
		return errors.New("invalid transfer id")
	}
	if !agentproto.ValidSSHPublicKey(req.DestPublicKey) {
		return errors.New("invalid destination SSH public key")
	}
	ips, err := ParseDestIPs(req.DestIPAddress)
	if err != nil {
		return err
	}
	if req.Format != "safetensors" && req.Format != "gguf" {
		return fmt.Errorf("invalid format %q", req.Format)
	}
	dir, files, err := a.resolveSource(req.ModelRef, req.Quantization, req.Format)
	if err != nil {
		return err
	}

	g := Grant{
		TransferID: req.TransferID, DestPublicKey: req.DestPublicKey, DestIPs: ips,
		ModelDir: dir, Files: files, ExpiresAt: a.now().Add(a.ttl).UTC(),
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode grant: %w", err)
	}
	final := filepath.Join(a.dir, g.TransferID+".json")
	if _, err := os.Stat(final); err == nil {
		return errors.New("a grant for this transfer already exists")
	}
	tmp, err := os.CreateTemp(a.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("write grant: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return fmt.Errorf("write grant: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write grant: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write grant: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("write grant: %w", err)
	}
	return nil
}

// Revoke removes a transfer's grant (and its used marker). Removing
// what isn't there is not an error - revoke is best-effort and repeatable.
func (a *Authorizer) Revoke(transferID string) error {
	if !ValidTransferID(transferID) {
		return errors.New("invalid transfer id")
	}
	for _, path := range []string{filepath.Join(a.dir, transferID+".json"), filepath.Join(a.usedDir, transferID)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove grant: %w", err)
		}
	}
	return nil
}

// SweepStale deletes expired grants, used-markers older than the TTL, and
// leftover temp files. Run at agent startup. Not needed for
// correctness - an expired grant is never honored regardless - only to
// keep the directory tidy.
func (a *Authorizer) SweepStale() (removed int, err error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return 0, err
	}
	now := a.now()
	for _, e := range entries {
		path := filepath.Join(a.dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), ".tmp-"):
			if os.Remove(path) == nil {
				removed++
			}
		case strings.HasSuffix(e.Name(), ".json"):
			g, err := readGrantFile(path)
			if err != nil || !now.Before(g.ExpiresAt) {
				if os.Remove(path) == nil {
					removed++
				}
			}
		}
	}
	if used, err := os.ReadDir(a.usedDir); err == nil {
		for _, e := range used {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > a.ttl {
				if os.Remove(filepath.Join(a.usedDir, e.Name())) == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

func readGrantFile(path string) (*Grant, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("decode grant: %w", err)
	}
	return &g, nil
}

// ReadGrant loads a transfer's unexpired, unused grant.
func ReadGrant(dir, usedDir, transferID string, now time.Time) (*Grant, error) {
	if !ValidTransferID(transferID) {
		return nil, errors.New("invalid transfer id")
	}
	if _, err := os.Stat(filepath.Join(usedDir, transferID)); err == nil {
		return nil, errors.New("grant has already been used")
	}
	g, err := readGrantFile(filepath.Join(dir, transferID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoGrant
	}
	if err != nil {
		return nil, err
	}
	if g.TransferID != transferID {
		return nil, errors.New("grant does not match its file name")
	}
	if !now.Before(g.ExpiresAt) {
		return nil, errors.New("grant has expired")
	}
	return g, nil
}

// ActiveGrants returns every unexpired, well-formed grant in dir.
func ActiveGrants(dir, usedDir string, now time.Time) []*Grant {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*Grant
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if g, err := ReadGrant(dir, usedDir, id, now); err == nil {
			out = append(out, g)
		}
	}
	return out
}
