// SPDX-License-Identifier: AGPL-3.0-or-later

package peertransfer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// WriteAuthorizedKeys prints, for sshd's AuthorizedKeysCommand, one
// restricted authorized_keys line per unexpired grant. It prints nothing
// for any user other than PeerUser, so the command can never contribute
// keys to anyone else's login.
//
// Each line is:
//
//	restrict,from="<dest ips>",command="<agent> peer-serve <id>" <key>
//
// restrict disables forwarding, pty, agent/X11 forwarding and user rc in
// one go; command forces this transfer's serve wrapper regardless of what
// the client asked to run; from= is an additional restriction, never the
// only one - the key is the credential. Every value interpolated into the
// line is re-validated here (key, IPs, transfer id), so a corrupt or
// tampered grant file can produce no line at all but never a dangerous
// one.
func WriteAuthorizedKeys(w io.Writer, dir, usedDir, user string, now time.Time) error {
	if user != PeerUser {
		return nil
	}
	for _, g := range ActiveGrants(dir, usedDir, now) {
		if !agentproto.ValidSSHPublicKey(g.DestPublicKey) || !ValidTransferID(g.TransferID) {
			continue
		}
		ips, err := ParseDestIPs(strings.Join(g.DestIPs, ","))
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "restrict,from=\"%s\",command=\"%s peer-serve %s\" %s\n",
			strings.Join(ips, ","), AgentBinary, g.TransferID, g.DestPublicKey); err != nil {
			return err
		}
	}
	return nil
}

// rsyncSenderPattern matches the one command an rsync client may run on
// the source: "rsync --server --sender <options> . .". The options are a
// short-flag cluster of plain sender options plus rsync's own protocol-
// capability suffix ("e." followed by capability letters, which are not
// options), then optionally --timeout=<digits> and --safe-links, which the
// client mirrors to the server (found empirically against rsync 3.2.7 -
// the puller passes both). Deliberately absent: L/k/K (follow symlinks -
// would let a link inside a model escape it), and every other long option
// (--files-from, --write-batch, --log-file, --rsync-path, ...).
var rsyncSenderPattern = regexp.MustCompile(`^rsync --server --sender (-[logDtprzv]+(?:e\.[A-Za-z.]*)?)((?: --timeout=[0-9]{1,4}| --safe-links){0,2}) \. \.$`)

// ParseSenderCommand validates the command a client asked sshd to run
// (SSH_ORIGINAL_COMMAND) and returns the validated options to hand to
// rsync (the flag cluster, then any --timeout/--safe-links). It never
// interprets the string as a shell command - it is only pattern-matched -
// and the client-supplied source path is required to be "." and is then
// ignored: what is served comes from the grant, never from the client.
func ParseSenderCommand(cmd string) (options []string, err error) {
	if len(cmd) > 160 {
		return nil, errors.New("command not permitted")
	}
	m := rsyncSenderPattern.FindStringSubmatch(cmd)
	if m == nil {
		return nil, errors.New("command not permitted")
	}
	options = []string{m[1]}
	options = append(options, strings.Fields(m[2])...)
	return options, nil
}

// ServePlan is what peer-serve executes: rsync's argv, and the directory to
// run it in.
type ServePlan struct {
	Path string
	Argv []string
	Dir  string
}

// rsyncPath is the only rsync ever executed, so a PATH the client cannot
// influence anyway is not consulted at all.
const rsyncPath = "/usr/bin/rsync"

// PrepareServe validates a peer-serve invocation and consumes its grant.
// It is the forced command's entire security decision:
//   - the transfer id must be well-formed and have a live grant;
//   - the connecting address (SSH_CONNECTION, set by sshd, not the client)
//     must be one of the grant's destination addresses;
//   - the client's requested command must be exactly the rsync sender
//     command (ParseSenderCommand);
//   - the grant is then consumed (an O_EXCL used-marker), so it authorizes
//     exactly one connection: a second one finds nothing.
//
// The plan serves only the grant's model directory (or its named files),
// with rsync's cwd set there and relative sources, so no absolute path from
// any input is ever passed to rsync.
func PrepareServe(dir, usedDir, transferID, originalCommand, sshConnection string, now time.Time) (*ServePlan, error) {
	g, err := ReadGrant(dir, usedDir, transferID, now)
	if err != nil {
		return nil, err
	}

	fields := strings.Fields(sshConnection)
	if len(fields) < 1 {
		return nil, errors.New("no connection information")
	}
	allowed := false
	for _, ip := range g.DestIPs {
		if ip == fields[0] {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("connection from an address this grant does not cover")
	}

	options, err := ParseSenderCommand(originalCommand)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(g.ModelDir) {
		return nil, errors.New("grant has a non-absolute model directory")
	}

	// Sources are relative to the model directory (rsync's cwd). A named
	// file is passed as "./<name>" and re-checked here, so even a tampered
	// grant can never make a file name parse as an rsync option; the
	// whole-directory case is just ".".
	sources := []string{"."}
	if len(g.Files) > 0 {
		sources = sources[:0]
		for _, f := range g.Files {
			if !SafeFileName(f) {
				return nil, errors.New("grant names a file that cannot be served safely")
			}
			sources = append(sources, "./"+f)
		}
	}
	// Consume: exactly one connection may use a grant. O_EXCL makes the
	// claim atomic - of two simultaneous connections, one wins - and the
	// marker also makes peer-authkeys stop offering the key at once.
	marker, err := os.OpenFile(filepath.Join(usedDir, transferID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("consume grant: %w", err)
	}
	marker.Close()

	argv := append([]string{"rsync", "--server", "--sender"}, options...)
	argv = append(argv, ".")
	argv = append(argv, sources...)
	return &ServePlan{Path: rsyncPath, Argv: argv, Dir: g.ModelDir}, nil
}
