// SPDX-License-Identifier: AGPL-3.0-or-later

package peertransfer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/agent/modelpath"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// ProgressFunc reports a transfer's state, same shape as
// agent/transfer.ProgressFunc.
type ProgressFunc func(bytesTransferred, bytesTotal int64, status, errMsg string)

// Transfer status strings, matching agentproto.TransferProgress.Status.
const (
	statusTransferring = "transferring"
	statusCompleted    = "completed"
	statusFailed       = "failed"
)

// runRsyncFunc runs rsync, calling onLine for each progress line, and
// returns the tail of its stderr. Overridable for tests.
type runRsyncFunc func(ctx context.Context, args []string, onLine func(string)) (stderrTail string, err error)

// Puller is the destination side: it pulls a model from a source node.
type Puller struct {
	keyPath     string
	storageRoot string
	run         runRsyncFunc
	progressGap time.Duration
}

// NewPuller returns a Puller that authenticates with the private key at
// keyPath and writes models under storageRoot.
func NewPuller(keyPath, storageRoot string) *Puller {
	return &Puller{keyPath: keyPath, storageRoot: storageRoot, run: runRsync, progressGap: time.Second}
}

var progressLine = regexp.MustCompile(`^\s*([\d,]+)\s+(\d+)%`)

// ParseProgress extracts (bytes so far, estimated total) from one rsync
// --info=progress2 line. total is estimated from the percentage.
func ParseProgress(line string) (bytes, total int64, ok bool) {
	m := progressLine.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	pct, _ := strconv.Atoi(m[2])
	if pct <= 0 || pct > 100 {
		return b, 0, true
	}
	return b, b * 100 / int64(pct), true
}

// noWhitespace guards values spliced into rsync's -e string, which rsync
// splits on whitespace itself.
func noWhitespace(s string) bool { return !strings.ContainsAny(s, " \t\r\n\"'\\") }

// hostKeyLine builds the pinned known_hosts entry: the source's reported
// sshd host key is the only key this connection will accept, so there is
// no trust-on-first-use window.
func hostKeyLine(host string, port int, hostKey string) string {
	name := host
	if strings.Contains(host, ":") {
		name = "[" + host + "]"
	}
	if port != 22 {
		name = "[" + host + "]:" + strconv.Itoa(port)
	}
	return name + " " + hostKey + "\n"
}

// Command builds the rsync argv for a pull, and the known_hosts content
// that must exist at knownHostsPath. Pure - exported for testing.
func (p *Puller) command(req agentproto.StartPeerTransfer, destDir, knownHostsPath string) (args []string, knownHosts string, err error) {
	ip := net.ParseIP(req.SourceHost)
	if ip == nil {
		return nil, "", fmt.Errorf("source host %q is not an IP address", req.SourceHost)
	}
	if req.SourceSSHPort < 1 || req.SourceSSHPort > 65535 {
		return nil, "", fmt.Errorf("invalid source ssh port %d", req.SourceSSHPort)
	}
	if !agentproto.ValidSSHPublicKey(req.SourceHostPublicKey) {
		return nil, "", errors.New("invalid source host public key")
	}
	keyType, _, _ := strings.Cut(req.SourceHostPublicKey, " ")
	if !noWhitespace(p.keyPath) || !noWhitespace(knownHostsPath) {
		return nil, "", errors.New("key or known_hosts path contains characters that cannot be passed to rsync -e")
	}

	host := ip.String()
	ssh := strings.Join([]string{
		"ssh", "-i", p.keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlgorithms=" + keyType,
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=15",
		"-p", strconv.Itoa(req.SourceSSHPort),
	}, " ")

	remoteHost := host
	if strings.Contains(host, ":") {
		remoteHost = "[" + host + "]"
	}
	args = []string{
		"-a", "--partial", "--safe-links", "--info=progress2", "--no-inc-recursive", "--timeout=120",
		"-e", ssh,
		PeerUser + "@" + remoteHost + ":.",
		destDir + "/",
	}
	return args, hostKeyLine(host, req.SourceSSHPort, req.SourceHostPublicKey), nil
}

// Pull runs one transfer to completion, reporting progress. The
// destination directory is resolved locally from ModelRef (never a wire
// path), the source's host key is pinned, and rsync's --safe-links refuses
// to recreate any symlink pointing outside the transferred tree.
func (p *Puller) Pull(ctx context.Context, req agentproto.StartPeerTransfer, progress ProgressFunc) error {
	fail := func(err error) error {
		progress(0, 0, statusFailed, err.Error())
		return err
	}
	if !ValidTransferID(req.TransferID) {
		return fail(errors.New("invalid transfer id"))
	}
	if req.Format != "safetensors" && req.Format != "gguf" {
		return fail(fmt.Errorf("invalid format %q", req.Format))
	}
	destDir, err := modelpath.Resolve(p.storageRoot, req.ModelRef)
	if err != nil {
		return fail(err)
	}

	tmp, err := os.MkdirTemp("", "sparky-peer-*")
	if err != nil {
		return fail(fmt.Errorf("create scratch dir: %w", err))
	}
	defer os.RemoveAll(tmp)
	knownHostsPath := filepath.Join(tmp, "known_hosts")
	args, knownHosts, err := p.command(req, destDir, knownHostsPath)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(knownHostsPath, []byte(knownHosts), 0o600); err != nil {
		return fail(fmt.Errorf("write known_hosts: %w", err))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fail(fmt.Errorf("create destination directory: %w", err))
	}

	progress(0, 0, statusTransferring, "")
	var last time.Time
	var lastBytes, lastTotal int64
	onLine := func(line string) {
		b, total, ok := ParseProgress(line)
		if !ok {
			return
		}
		lastBytes, lastTotal = b, total
		if time.Since(last) >= p.progressGap {
			last = time.Now()
			progress(b, total, statusTransferring, "")
		}
	}
	stderrTail, err := p.run(ctx, args, onLine)
	if err != nil {
		msg := fmt.Sprintf("rsync failed: %v", err)
		if t := strings.TrimSpace(stderrTail); t != "" {
			msg += ": " + t
		}
		return fail(errors.New(msg))
	}
	total := lastTotal
	if lastBytes > total {
		total = lastBytes
	}
	progress(total, total, statusCompleted, "")
	return nil
}

// runRsync is the real runRsyncFunc.
func runRsync(ctx context.Context, args []string, onLine func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(stdout)
	// rsync redraws progress with carriage returns, so split on \r as well
	// as \n.
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		onLine(sc.Text())
	}
	return stderr.String(), cmd.Wait()
}

// tailBuffer keeps the last few KB written to it.
type tailBuffer struct{ b []byte }

const tailMax = 2048

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = t.b[len(t.b)-tailMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
