// SPDX-License-Identifier: AGPL-3.0-or-later

package peertransfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

type env struct {
	root, grants, used string
	auth               *Authorizer
	now                time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	grants := t.TempDir()
	used := t.TempDir()
	e := &env{root: root, grants: grants, used: used, now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	e.auth = NewAuthorizer(grants, used, root, time.Hour)
	e.auth.now = func() time.Time { return e.now }
	return e
}

func (e *env) write(t *testing.T, rel string) {
	t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func req(id, ref, quant, format string) agentproto.AuthorizePeerPull {
	return agentproto.AuthorizePeerPull{TransferID: id, DestPublicKey: testKey, DestIPAddress: "10.0.0.9,10.0.1.9", ModelRef: ref, Quantization: quant, Format: format}
}

func TestAuthorize_WholeDirForSafetensors(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/model.safetensors")
	if err := e.auth.Authorize(req("t-1", "org/m", "FP16", "safetensors")); err != nil {
		t.Fatal(err)
	}
	g, err := ReadGrant(e.grants, e.used, "t-1", e.now)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(e.root, "org/m"))
	if g.ModelDir != real || len(g.Files) != 0 || g.DestPublicKey != testKey || len(g.DestIPs) != 2 {
		t.Errorf("grant = %+v", g)
	}
	if info, _ := os.Stat(filepath.Join(e.grants, "t-1.json")); info.Mode().Perm() != 0o640 {
		t.Errorf("grant mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestAuthorize_GGUFQuantizationCoversOnlyMatchingFiles(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q4_K_M-00001-of-00002.gguf")
	e.write(t, "org/m/m-Q4_K_M-00002-of-00002.gguf")
	e.write(t, "org/m/m-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q4_K_M", "gguf")); err != nil {
		t.Fatal(err)
	}
	g, _ := ReadGrant(e.grants, e.used, "t-1", e.now)
	if len(g.Files) != 2 || strings.Contains(strings.Join(g.Files, ","), "Q8_0") {
		t.Errorf("files = %v, want only the two Q4_K_M shards - never a sibling quantization", g.Files)
	}
}

func TestAuthorize_GGUFUnknownQuantizationCoversWholeDir(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "UNKNOWN", "gguf")); err != nil {
		t.Fatal(err)
	}
	if g, _ := ReadGrant(e.grants, e.used, "t-1", e.now); len(g.Files) != 0 {
		t.Errorf("files = %v, want whole dir", g.Files)
	}
}

func TestAuthorize_Rejections(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q8_0.gguf")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.root, "escape")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*agentproto.AuthorizePeerPull)
	}{
		{"bad transfer id", func(r *agentproto.AuthorizePeerPull) { r.TransferID = "../x" }},
		{"transfer id with quote", func(r *agentproto.AuthorizePeerPull) { r.TransferID = `a"b` }},
		{"empty transfer id", func(r *agentproto.AuthorizePeerPull) { r.TransferID = "" }},
		{"key with options", func(r *agentproto.AuthorizePeerPull) { r.DestPublicKey = `command="x" ` + testKey }},
		{"key with newline", func(r *agentproto.AuthorizePeerPull) { r.DestPublicKey = testKey + "\n" + testKey }},
		{"empty key", func(r *agentproto.AuthorizePeerPull) { r.DestPublicKey = "" }},
		{"wildcard ip", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = "*" }},
		{"cidr ip", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = "10.0.0.0/8" }},
		{"hostname ip", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = "evil.example.com" }},
		{"ip list with injection", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = `10.0.0.1",command="x` }},
		{"unspecified ip", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = "0.0.0.0" }},
		{"empty ip", func(r *agentproto.AuthorizePeerPull) { r.DestIPAddress = "" }},
		{"bad format", func(r *agentproto.AuthorizePeerPull) { r.Format = "onnx" }},
		{"path traversal", func(r *agentproto.AuthorizePeerPull) { r.ModelRef = "../../etc" }},
		{"absolute ref", func(r *agentproto.AuthorizePeerPull) { r.ModelRef = "/etc" }},
		{"missing model", func(r *agentproto.AuthorizePeerPull) { r.ModelRef = "org/none" }},
		{"symlink escaping storage", func(r *agentproto.AuthorizePeerPull) { r.ModelRef = "escape" }},
		{"no matching quantization", func(r *agentproto.AuthorizePeerPull) { r.Quantization, r.Format = "Q4_K_M", "gguf" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := req("t-1", "org/m", "Q8_0", "gguf")
			tt.mutate(&r)
			if err := e.auth.Authorize(r); err == nil {
				t.Fatal("Authorize succeeded, want an error")
			}
			if entries, _ := os.ReadDir(e.grants); len(entries) != 0 {
				t.Errorf("a refused authorization left files behind: %v", entries)
			}
		})
	}
}

func TestAuthorize_DuplicateRefused(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q8_0", "gguf")); err != nil {
		t.Fatal(err)
	}
	if err := e.auth.Authorize(req("t-1", "org/m", "Q8_0", "gguf")); err == nil {
		t.Error("a second grant for the same transfer id must be refused")
	}
}

func TestRevokeAndSweep(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q8_0.gguf")
	for _, id := range []string{"t-1", "t-2"} {
		if err := e.auth.Authorize(req(id, "org/m", "Q8_0", "gguf")); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.auth.Revoke("t-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.auth.Revoke("t-1"); err != nil {
		t.Errorf("revoking twice must not error: %v", err)
	}
	if _, err := ReadGrant(e.grants, e.used, "t-1", e.now); !errors.Is(err, ErrNoGrant) {
		t.Errorf("revoked grant still readable: %v", err)
	}
	if err := e.auth.Revoke("../x"); err == nil {
		t.Error("revoke must validate the id")
	}

	if err := os.WriteFile(filepath.Join(e.grants, ".tmp-abc"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(3 * time.Hour)
	removed, err := e.auth.SweepStale()
	if err != nil || removed != 2 {
		t.Errorf("SweepStale() = %d, %v; want the expired grant and the temp file removed", removed, err)
	}
	if entries, _ := os.ReadDir(e.grants); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestReadGrant_ExpiredIsNotHonored(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q8_0", "gguf")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGrant(e.grants, e.used, "t-1", e.now.Add(59*time.Minute)); err != nil {
		t.Errorf("grant unusable before expiry: %v", err)
	}
	if _, err := ReadGrant(e.grants, e.used, "t-1", e.now.Add(time.Hour)); err == nil {
		t.Error("an expired grant must never be honored, swept or not")
	}
}

func TestWriteAuthorizedKeys(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q8_0", "gguf")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteAuthorizedKeys(&buf, e.grants, e.used, PeerUser, e.now); err != nil {
		t.Fatal(err)
	}
	want := `restrict,from="10.0.0.9,10.0.1.9",command="/opt/sparky/bin/sparky-agent peer-serve t-1" ` + testKey + "\n"
	if buf.String() != want {
		t.Errorf("output = %q\nwant     %q", buf.String(), want)
	}

	for _, other := range []string{"root", "serviceloop", "sparky-peer2", ""} {
		buf.Reset()
		if err := WriteAuthorizedKeys(&buf, e.grants, e.used, other, e.now); err != nil || buf.Len() != 0 {
			t.Errorf("user %q: output %q, err %v - must print nothing for anyone but %s", other, buf.String(), err, PeerUser)
		}
	}

	buf.Reset()
	if err := WriteAuthorizedKeys(&buf, e.grants, e.used, PeerUser, e.now.Add(2*time.Hour)); err != nil || buf.Len() != 0 {
		t.Errorf("expired grant produced output %q", buf.String())
	}
}

func TestWriteAuthorizedKeys_TamperedGrantProducesNoLine(t *testing.T) {
	e := newEnv(t)
	for name, g := range map[string]string{
		"key-injection": `{"transfer_id":"key-injection","dest_public_key":"ssh-ed25519 AAAA\nssh-ed25519 BBBB","dest_ips":["10.0.0.1"],"model_dir":"/x","expires_at":"2099-01-01T00:00:00Z"}`,
		"ip-injection":  `{"transfer_id":"ip-injection","dest_public_key":"` + testKey + `","dest_ips":["10.0.0.1\",command=\"x"],"model_dir":"/x","expires_at":"2099-01-01T00:00:00Z"}`,
		"id-mismatch":   `{"transfer_id":"other","dest_public_key":"` + testKey + `","dest_ips":["10.0.0.1"],"model_dir":"/x","expires_at":"2099-01-01T00:00:00Z"}`,
		"not-json":      `garbage`,
	} {
		if err := os.WriteFile(filepath.Join(e.grants, name+".json"), []byte(g), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := WriteAuthorizedKeys(&buf, e.grants, e.used, PeerUser, e.now); err != nil || buf.Len() != 0 {
		t.Errorf("tampered grants produced output %q (err %v)", buf.String(), err)
	}
}

func TestParseSenderCommand(t *testing.T) {
	good := "rsync --server --sender -logDtpre.iLsfxCIvu . ."
	if opts, err := ParseSenderCommand(good); err != nil || strings.Join(opts, " ") != "-logDtpre.iLsfxCIvu" {
		t.Errorf("plain client command rejected: %q, %v", opts, err)
	}
	// Exactly what a real rsync 3.2.7 client run by the puller sends.
	real := "rsync --server --sender -logDtpre.LsfxCIvu --timeout=120 --safe-links . ."
	if opts, err := ParseSenderCommand(real); err != nil || strings.Join(opts, " ") != "-logDtpre.LsfxCIvu --timeout=120 --safe-links" {
		t.Errorf("real puller command rejected: %q, %v", opts, err)
	}
	if _, err := ParseSenderCommand("rsync --server --sender -logDtpre.iLsfxC . ."); err != nil {
		t.Errorf("older client command rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		"sh",
		"rsync --server --sender -logDtpre.iLsfxCIvu . /etc",
		"rsync --server --sender -logDtpre.iLsfxCIvu . ./x",
		"rsync --server --sender -logDtpre.iLsfxCIvu /etc .",
		"rsync --server --sender -logDtpre.iLsfxCIvu . . ; id",
		"rsync --server --sender -logDtpre.iLsfxCIvu . .; id",
		"rsync --server --sender -logDtpre.iLsfxCIvu . . && id",
		"rsync --server --sender -logDtpre.iLsfxCIvu . .\nid",
		"rsync --server --sender -logDtpre.iLsfxCIvu . $(id)",
		"rsync --server --sender -logDtpre.iLsfxCIvu . `id`",
		"rsync --server --sender -LlogDtpre.iLsfxCIvu . .",
		"rsync --server --sender -logDtkpre . .",
		"rsync --server --sender -logDtpre . .",
		"rsync --server --sender --files-from=/etc/passwd . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu --timeout=abc . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu --timeout=120000 . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu --safe-links --files-from=/etc/passwd . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu --timeout=1 --timeout=2 --timeout=3 . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu --copy-links . .",
		"rsync --server --sender --write-batch=/tmp/x -logDtpre.iLsfxCIvu . .",
		"rsync --server --sender --log-file=/tmp/x -logDtpre.iLsfxCIvu . .",
		"rsync --server --sender --rsync-path=/bin/sh -logDtpre.iLsfxCIvu . .",
		"rsync --server --sender -e/bin/sh . .",
		"rsync --daemon --config=/etc/rsyncd.conf",
		"rsync --server -logDtpre.iLsfxCIvu . .",
		"rsync --server --sender -logDtpre.iLsfxCIvu . . extra",
		"rsync  --server --sender -logDtpre.iLsfxCIvu . .",
		" rsync --server --sender -logDtpre.iLsfxCIvu . .",
		"scp -f /etc/passwd",
		"/usr/bin/rsync --server --sender -logDtpre.iLsfxCIvu . .",
		"rsync --server --sender -" + strings.Repeat("l", 200) + " . .",
	} {
		if _, err := ParseSenderCommand(bad); err == nil {
			t.Errorf("ParseSenderCommand(%q) succeeded, want rejection", bad)
		}
	}
}

func servePrep(t *testing.T, e *env) {
	t.Helper()
	e.write(t, "org/m/m-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q8_0", "gguf")); err != nil {
		t.Fatal(err)
	}
}

const goodCmd = "rsync --server --sender -logDtpre.LsfxCIvu --timeout=120 --safe-links . ."

func TestPrepareServe_HappyPathServesGrantNotClientPath(t *testing.T) {
	e := newEnv(t)
	servePrep(t, e)
	plan, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.0.9 51234 10.0.0.5 22", e.now)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(e.root, "org/m"))
	wantArgv := []string{"rsync", "--server", "--sender", "-logDtpre.LsfxCIvu", "--timeout=120", "--safe-links", ".", "./m-Q8_0.gguf"}
	if plan.Path != "/usr/bin/rsync" || plan.Dir != real || strings.Join(plan.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("plan = %+v", plan)
	}
	for _, a := range plan.Argv[1:] {
		if filepath.IsAbs(a) {
			t.Errorf("absolute path %q reached rsync's argv", a)
		}
	}
}

func TestPrepareServe_WholeDirServesDot(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/config.json")
	if err := e.auth.Authorize(req("t-1", "org/m", "FP16", "safetensors")); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.1.9 1 10.0.0.5 22", e.now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Argv[len(plan.Argv)-1] != "." {
		t.Errorf("argv = %v, want the model dir served as \".\"", plan.Argv)
	}
}

func TestPrepareServe_GrantIsSingleUse(t *testing.T) {
	e := newEnv(t)
	servePrep(t, e)
	if _, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", e.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", e.now); err == nil {
		t.Error("a grant must authorize exactly one connection")
	}
	var buf bytes.Buffer
	_ = WriteAuthorizedKeys(&buf, e.grants, e.used, PeerUser, e.now)
	if buf.Len() != 0 {
		t.Errorf("a consumed grant still yields an authorized_keys line: %q", buf.String())
	}
}

func TestPrepareServe_Refusals(t *testing.T) {
	tests := []struct {
		name, id, cmd, conn string
		later               time.Duration
	}{
		{"wrong address", "t-1", goodCmd, "10.9.9.9 1 10.0.0.5 22", 0},
		{"no connection info", "t-1", goodCmd, "", 0},
		{"bad command", "t-1", "sh", "10.0.0.9 1 10.0.0.5 22", 0},
		{"traversal id", "../t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", 0},
		{"unknown id", "t-9", goodCmd, "10.0.0.9 1 10.0.0.5 22", 0},
		{"expired", "t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			servePrep(t, e)
			if _, err := PrepareServe(e.grants, e.used, tt.id, tt.cmd, tt.conn, e.now.Add(tt.later)); err == nil {
				t.Fatal("PrepareServe succeeded, want a refusal")
			}
			// A refused connection must not burn the grant for the
			// legitimate destination that has not connected yet.
			if tt.name != "expired" && tt.id == "t-1" {
				if _, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", e.now); err != nil {
					t.Errorf("a refused attempt consumed the grant: %v", err)
				}
			}
		})
	}
}

func TestParseProgress(t *testing.T) {
	b, total, ok := ParseProgress("  1,048,576  25%   10.00MB/s    0:00:03 (xfr#1, to-chk=0/2)")
	if !ok || b != 1048576 || total != 4194304 {
		t.Errorf("ParseProgress = %d, %d, %v", b, total, ok)
	}
	if b, total, ok := ParseProgress("        32,768   0%    0.00kB/s"); !ok || b != 32768 || total != 0 {
		t.Errorf("0%% line = %d, %d, %v, want bytes with unknown total", b, total, ok)
	}
	for _, line := range []string{"", "sending incremental file list", "m.gguf", "total size is 5  speedup is 1.00"} {
		if _, _, ok := ParseProgress(line); ok {
			t.Errorf("ParseProgress(%q) matched a non-progress line", line)
		}
	}
}

func startReq() agentproto.StartPeerTransfer {
	return agentproto.StartPeerTransfer{
		TransferID: "t-1", SourceNodeID: "src", SourceHost: "10.0.1.5", SourceSSHPort: 22,
		SourceHostPublicKey: testKey, ModelRef: "org/m", Quantization: "Q8_0", Format: "gguf",
	}
}

func TestPullerCommand_PinsHostKeyAndBuildsRsyncArgs(t *testing.T) {
	p := NewPuller("/keys/id_ed25519", "/models")
	args, kh, err := p.command(startReq(), "/models/org/m", "/tmp/kh")
	if err != nil {
		t.Fatal(err)
	}
	if kh != "10.0.1.5 "+testKey+"\n" {
		t.Errorf("known_hosts = %q", kh)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-a", "--partial", "--safe-links", "--info=progress2",
		"-i /keys/id_ed25519", "IdentitiesOnly=yes", "BatchMode=yes", "StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/tmp/kh", "GlobalKnownHostsFile=/dev/null", "HostKeyAlgorithms=ssh-ed25519",
		"PasswordAuthentication=no", "ClearAllForwardings=yes", "-p 22",
		"sparky-peer@10.0.1.5:. /models/org/m/",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rsync args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "StrictHostKeyChecking=no") || strings.Contains(joined, "accept-new") {
		t.Error("host key checking must be strict - no trust on first use")
	}
}

func TestPullerCommand_IPv6AndPort(t *testing.T) {
	p := NewPuller("/keys/id", "/models")
	r := startReq()
	r.SourceHost, r.SourceSSHPort = "2001:db8::5", 2222
	args, kh, err := p.command(r, "/models/org/m", "/tmp/kh")
	if err != nil {
		t.Fatal(err)
	}
	if kh != "[2001:db8::5]:2222 "+testKey+"\n" {
		t.Errorf("known_hosts = %q", kh)
	}
	if !strings.Contains(strings.Join(args, " "), "sparky-peer@[2001:db8::5]:. ") {
		t.Errorf("args = %v", args)
	}
}

func TestPullerCommand_Rejections(t *testing.T) {
	p := NewPuller("/keys/id", "/models")
	tests := map[string]func(*agentproto.StartPeerTransfer){
		"hostname":             func(r *agentproto.StartPeerTransfer) { r.SourceHost = "evil.example.com" },
		"option-like host":     func(r *agentproto.StartPeerTransfer) { r.SourceHost = "-oProxyCommand=id" },
		"port zero":            func(r *agentproto.StartPeerTransfer) { r.SourceSSHPort = 0 },
		"port too big":         func(r *agentproto.StartPeerTransfer) { r.SourceSSHPort = 70000 },
		"host key with option": func(r *agentproto.StartPeerTransfer) { r.SourceHostPublicKey = "cert-authority " + testKey },
		"empty host key":       func(r *agentproto.StartPeerTransfer) { r.SourceHostPublicKey = "" },
	}
	for name, mutate := range tests {
		r := startReq()
		mutate(&r)
		if _, _, err := p.command(r, "/models/org/m", "/tmp/kh"); err == nil {
			t.Errorf("%s: command() succeeded, want an error", name)
		}
	}
	if _, _, err := NewPuller("/keys/my id", "/models").command(startReq(), "/models/org/m", "/tmp/kh"); err == nil {
		t.Error("a key path containing a space cannot be passed through rsync -e and must be refused")
	}
}

type progressRec struct {
	calls []struct {
		b, total      int64
		status, error string
	}
}

func (r *progressRec) fn(b, total int64, status, errMsg string) {
	r.calls = append(r.calls, struct {
		b, total      int64
		status, error string
	}{b, total, status, errMsg})
}

func TestPull_SuccessReportsProgressAndCompletes(t *testing.T) {
	root := t.TempDir()
	p := NewPuller("/keys/id", root)
	p.progressGap = 0
	var gotArgs []string
	p.run = func(_ context.Context, args []string, onLine func(string)) (string, error) {
		gotArgs = args
		onLine("sending incremental file list")
		onLine("  500  50%   1.00MB/s    0:00:01")
		onLine("  1,000  100%   1.00MB/s    0:00:02 (xfr#1, to-chk=0/1)")
		return "", nil
	}
	rec := &progressRec{}
	if err := p.Pull(context.Background(), startReq(), rec.fn); err != nil {
		t.Fatal(err)
	}
	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != filepath.Join(root, "org/m")+"/" {
		t.Errorf("rsync destination = %v", gotArgs)
	}
	if info, err := os.Stat(filepath.Join(root, "org/m")); err != nil || !info.IsDir() {
		t.Error("destination directory was not created")
	}
	first, last := rec.calls[0], rec.calls[len(rec.calls)-1]
	if first.status != "transferring" || last.status != "completed" || last.b != 1000 || last.total != 1000 {
		t.Errorf("progress = %+v", rec.calls)
	}
}

func TestPull_RsyncFailureReportsFailedWithStderr(t *testing.T) {
	p := NewPuller("/keys/id", t.TempDir())
	p.run = func(context.Context, []string, func(string)) (string, error) {
		return "Host key verification failed.\n", errors.New("exit status 255")
	}
	rec := &progressRec{}
	err := p.Pull(context.Background(), startReq(), rec.fn)
	if err == nil {
		t.Fatal("want an error")
	}
	last := rec.calls[len(rec.calls)-1]
	if last.status != "failed" || !strings.Contains(last.error, "Host key verification failed") {
		t.Errorf("last progress = %+v", last)
	}
}

func TestPull_ValidationFailuresNeverRunRsync(t *testing.T) {
	p := NewPuller("/keys/id", t.TempDir())
	ran := false
	p.run = func(context.Context, []string, func(string)) (string, error) { ran = true; return "", nil }
	for name, mutate := range map[string]func(*agentproto.StartPeerTransfer){
		"traversal ref": func(r *agentproto.StartPeerTransfer) { r.ModelRef = "../../etc" },
		"bad id":        func(r *agentproto.StartPeerTransfer) { r.TransferID = "a b" },
		"bad format":    func(r *agentproto.StartPeerTransfer) { r.Format = "x" },
		"hostname":      func(r *agentproto.StartPeerTransfer) { r.SourceHost = "example.com" },
	} {
		r := startReq()
		mutate(&r)
		rec := &progressRec{}
		if err := p.Pull(context.Background(), r, rec.fn); err == nil {
			t.Errorf("%s: Pull succeeded", name)
		}
		if len(rec.calls) == 0 || rec.calls[len(rec.calls)-1].status != "failed" {
			t.Errorf("%s: a rejected request must report failed, got %+v", name, rec.calls)
		}
	}
	if ran {
		t.Error("rsync ran for a request that failed validation")
	}
}

func TestSafeFileName(t *testing.T) {
	for _, ok := range []string{"m-Q4_K_M.gguf", "model-Q4_K_M-00001-of-00002.gguf", "Llama-3.1-8B.Q8_0.gguf", ".hidden.gguf", "a+b=c,d@e%f.gguf"} {
		if !SafeFileName(ok) {
			t.Errorf("SafeFileName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "-x.gguf", "--files-from=x-Q4_K_M.gguf", "--log-file=Q4_K_M.gguf", "a b.gguf", "a\nb.gguf", `a"b.gguf`, "a/b.gguf", "a;b.gguf", "a$b.gguf", "a`b.gguf", strings.Repeat("a", 300)} {
		if SafeFileName(bad) {
			t.Errorf("SafeFileName(%q) = true, want false", bad)
		}
	}
}

func TestAuthorize_RefusesOptionShapedModelFileName(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q4_K_M.gguf")
	e.write(t, "org/m/--files-from=list-Q4_K_M.gguf")
	err := e.auth.Authorize(req("t-1", "org/m", "Q4_K_M", "gguf"))
	if err == nil {
		t.Fatal("a repo-supplied option-shaped filename must never make it into a grant")
	}
	if entries, _ := os.ReadDir(e.grants); len(entries) != 0 {
		t.Errorf("refused authorization left %v", entries)
	}
}

func TestAuthorize_OptionShapedFileNotMatchingQuantizationIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.write(t, "org/m/m-Q4_K_M.gguf")
	e.write(t, "org/m/--files-from=list-Q8_0.gguf")
	if err := e.auth.Authorize(req("t-1", "org/m", "Q4_K_M", "gguf")); err != nil {
		t.Fatalf("a hostile name that is not part of the requested quantization must not block it: %v", err)
	}
	g, _ := ReadGrant(e.grants, e.used, "t-1", e.now)
	if len(g.Files) != 1 || g.Files[0] != "m-Q4_K_M.gguf" {
		t.Errorf("files = %v", g.Files)
	}
}

func TestPrepareServe_TamperedGrantWithOptionShapedFileIsRefusedAndNotConsumed(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "org", "m")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"transfer_id":"t-1","dest_public_key":"` + testKey + `","dest_ips":["10.0.0.9"],"model_dir":"` + dir + `","files":["--files-from=x.gguf"],"expires_at":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(e.grants, "t-1.json"), []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServe(e.grants, e.used, "t-1", goodCmd, "10.0.0.9 1 10.0.0.5 22", e.now); err == nil {
		t.Fatal("PrepareServe must refuse a grant naming an option-shaped file")
	}
	if entries, _ := os.ReadDir(e.used); len(entries) != 0 {
		t.Error("a refused serve must not consume the grant")
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// A killed child is briefly a zombie until its parent reaps it; a
		// zombie is dead for our purposes.
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err != nil || strings.Contains(string(b), ") Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still running after the group was cancelled", pid)
}

func TestRunProcess_CancelKillsTheWholeProcessGroupNotJustTheLeader(t *testing.T) {
	// A leader that starts a helper (like rsync starting ssh) and waits.
	ctx, cancel := context.WithCancel(context.Background())
	pidCh := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		_, err := runProcess(ctx, "bash", []string{"-c", "sleep 300 & echo $!; wait"}, func(line string) {
			var pid int
			if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
				pidCh <- pid
			}
		})
		done <- err
	}()
	var helper int
	select {
	case helper = <-pidCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the helper process never started")
	}
	if !alive(helper) {
		t.Fatal("helper not running before the cancel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runProcess did not return after cancellation")
	}
	waitDead(t, helper)
}

func TestRunProcess_CancelIsGracefulFirstSoRsyncCanSavePartialData(t *testing.T) {
	oldGrace := cancelGrace
	cancelGrace = 5 * time.Second
	defer func() { cancelGrace = oldGrace }()

	marker := filepath.Join(t.TempDir(), "saved-partial")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		// Traps SIGTERM the way rsync does: does its cleanup, then exits.
		runProcess(ctx, "bash", []string{"-c", fmt.Sprintf("trap 'echo saved > %s; exit 0' TERM; echo ready; while :; do sleep 0.1; done", marker)}, func(line string) {
			if line == "ready" {
				close(started)
			}
		})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a process that handles SIGTERM must be allowed to exit on its own")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("SIGTERM must be delivered first so the process can save its state - it was killed outright")
	}
}

func TestRunProcess_EscalatesToKillIfTheProcessIgnoresTerm(t *testing.T) {
	oldGrace := cancelGrace
	cancelGrace = 300 * time.Millisecond
	defer func() { cancelGrace = oldGrace }()

	ctx, cancel := context.WithCancel(context.Background())
	pidCh := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		runProcess(ctx, "bash", []string{"-c", "trap '' TERM; echo $$; while :; do sleep 0.1; done"}, func(line string) {
			var pid int
			if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
				pidCh <- pid
			}
		})
		close(done)
	}()
	pid := <-pidCh
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a process ignoring SIGTERM must still be killed after the grace period")
	}
	waitDead(t, pid)
}

func TestRunProcess_NormalCompletionReturnsStderrAndOutput(t *testing.T) {
	var lines []string
	tail, err := runProcess(context.Background(), "bash", []string{"-c", "echo one; echo oops >&2; exit 3"}, func(l string) { lines = append(lines, l) })
	if err == nil || len(lines) != 1 || lines[0] != "one" || !strings.Contains(tail, "oops") {
		t.Errorf("lines=%v tail=%q err=%v", lines, tail, err)
	}
}
