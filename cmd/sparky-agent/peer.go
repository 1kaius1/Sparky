// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"log/syslog"
	"os"
	"syscall"
	"time"

	"github.com/1kaius1/Sparky/agent/peertransfer"
)

// runPeerAuthorizedKeys implements `sparky-agent peer-authkeys <user>` -
// sshd's AuthorizedKeysCommand for the peer-transfer account. It is invoked
// by sshd (as serviceloop, with a scrubbed environment), prints zero or
// more authorized_keys lines, and never fails loudly: an error means "no
// keys", which sshd treats as a refused login.
func runPeerAuthorizedKeys(args []string) {
	if len(args) != 1 {
		os.Exit(0)
	}
	_ = peertransfer.WriteAuthorizedKeys(os.Stdout, peertransfer.GrantDir, peertransfer.UsedDir, args[0], time.Now())
}

// runPeerServe implements `sparky-agent peer-serve <transfer-id>` - the
// forced command in a peer-transfer authorized_keys line. sshd runs it as
// the peer-transfer account after key authentication; it replaces itself
// with rsync in sender mode only if peertransfer.PrepareServe approves.
// Refusals tell the client nothing beyond "refused" (the reason goes to
// syslog), and the process never returns to a shell.
func runPeerServe(args []string) {
	logRefusal := func(err error) {
		if w, werr := syslog.New(syslog.LOG_AUTH|syslog.LOG_NOTICE, "sparky-peer-serve"); werr == nil {
			_ = w.Notice(fmt.Sprintf("refused: %v", err))
		}
		fmt.Fprintln(os.Stderr, "sparky-peer-serve: refused")
		os.Exit(1)
	}
	if len(args) != 1 {
		logRefusal(fmt.Errorf("expected exactly one transfer id argument"))
	}
	plan, err := peertransfer.PrepareServe(peertransfer.GrantDir, peertransfer.UsedDir, args[0], os.Getenv("SSH_ORIGINAL_COMMAND"), os.Getenv("SSH_CONNECTION"), time.Now())
	if err != nil {
		logRefusal(err)
	}
	if err := os.Chdir(plan.Dir); err != nil {
		logRefusal(err)
	}
	// A fixed, minimal environment - nothing the client sent (sshd copies
	// only what AcceptEnv allows, and this account gets none) reaches rsync.
	if err := syscall.Exec(plan.Path, plan.Argv, []string{"PATH=/usr/bin:/bin", "LANG=C"}); err != nil {
		logRefusal(err)
	}
}
