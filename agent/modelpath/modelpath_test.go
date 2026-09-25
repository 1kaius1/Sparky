// SPDX-License-Identifier: AGPL-3.0-or-later

package modelpath

import "testing"

func TestResolve(t *testing.T) {
	root := "/models"
	good := map[string]string{
		"org/m":     "/models/org/m",
		"org/m/":    "/models/org/m",
		"a/./b":     "/models/a/b",
		"a/../a/b":  "/models/a/b",
		"single":    "/models/single",
		"org/m.v1_": "/models/org/m.v1_",
	}
	for in, want := range good {
		got, err := Resolve(root, in)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/etc", "/models/org", "a/..", "./"} {
		if got, err := Resolve(root, bad); err == nil {
			t.Errorf("Resolve(%q) = %q, want an error", bad, got)
		}
	}
}
