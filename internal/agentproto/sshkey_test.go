// SPDX-License-Identifier: AGPL-3.0-or-later

package agentproto

import (
	"strings"
	"testing"
)

func TestValidSSHPublicKey(t *testing.T) {
	good := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"ed25519", good, true},
		{"rsa", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC+/=", true},
		{"empty", "", false},
		{"comment appended", good + " user@host", false},
		{"trailing newline", good + "\n", false},
		{"embedded newline second key", good + "\nssh-ed25519 AAAA", false},
		{"options prefix", `command="rm -rf /" ` + good, false},
		{"from option", `from="*",` + good, false},
		{"unknown type", "ssh-dss AAAA", false},
		{"no body", "ssh-ed25519", false},
		{"quote in body", `ssh-ed25519 AAA"A`, false},
		{"space in body", "ssh-ed25519 AAAA BBBB", false},
		{"too long", "ssh-ed25519 " + strings.Repeat("A", 2000), false},
	}
	for _, tt := range tests {
		if got := ValidSSHPublicKey(tt.in); got != tt.want {
			t.Errorf("%s: ValidSSHPublicKey(%q) = %v, want %v", tt.name, tt.in, got, tt.want)
		}
	}
}
