// SPDX-License-Identifier: AGPL-3.0-or-later

package agentproto

import (
	"regexp"
	"strings"
)

// maxSSHPublicKeyLen bounds a reported key - an ed25519 key is ~80 bytes,
// a 4096-bit RSA one ~750.
const maxSSHPublicKeyLen = 1024

var sshKeyBase64 = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,3}$`)

var sshKeyTypes = map[string]bool{
	"ssh-ed25519":         true,
	"ssh-rsa":             true,
	"ecdsa-sha2-nistp256": true,
	"ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true,
}

// ValidSSHPublicKey reports whether s is exactly a bare "<type> <base64>"
// OpenSSH public key on one line - no comment, no options, no whitespace
// beyond the single separator. Deliberately strict: a public key reported
// by an agent is later written into another node's authorized_keys file
// (peer-to-peer model transfer), where anything beyond the bare key -
// a newline, a leading options field such as command=, a quote - could
// smuggle in an extra authorization or override a restriction. Both
// binaries validate with this same function (the server on receipt, the
// agent again before writing), so neither has to trust the other's check.
func ValidSSHPublicKey(s string) bool {
	if s == "" || len(s) > maxSSHPublicKeyLen {
		return false
	}
	typ, body, ok := strings.Cut(s, " ")
	if !ok || !sshKeyTypes[typ] {
		return false
	}
	return sshKeyBase64.MatchString(body)
}
