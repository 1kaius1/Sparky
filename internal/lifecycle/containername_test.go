// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var validContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestContainerName(t *testing.T) {
	at := time.Date(2026, 10, 8, 10, 4, 59, 0, time.UTC)
	for _, tc := range []struct {
		profile string
		want    string
	}{
		{"qwen3-8b", "sparky-qwen3-8b-20261008-100459"},
		{"Qwen3 8B (FP8)", "sparky-Qwen3-8B-FP8-20261008-100459"},
		{"team/llama 3.1", "sparky-team-llama-3.1-20261008-100459"},
		{"a   b", "sparky-a-b-20261008-100459"},
		{"--leading-and-trailing--", "sparky-leading-and-trailing-20261008-100459"},
		{"_x_", "sparky-x-20261008-100459"},
		{"modele-été", "sparky-modele-t-20261008-100459"},
		{"中文", "sparky-profile-20261008-100459"},
		{"", "sparky-profile-20261008-100459"},
		{"!!!", "sparky-profile-20261008-100459"},
		{strings.Repeat("a", 100), "sparky-" + strings.Repeat("a", 40) + "-20261008-100459"},
		{strings.Repeat("a", 39) + " b", "sparky-" + strings.Repeat("a", 39) + "-20261008-100459"},
	} {
		got := containerName(tc.profile, at)
		if got != tc.want {
			t.Errorf("containerName(%q) = %q, want %q", tc.profile, got, tc.want)
		}
		if !validContainerName.MatchString(got) {
			t.Errorf("containerName(%q) = %q is not a valid Docker/Podman container name", tc.profile, got)
		}
	}
}

func TestContainerName_UsesUTC(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*3600)
	at := time.Date(2026, 10, 8, 19, 4, 59, 0, loc) // 10:04:59 UTC
	if got := containerName("p", at); got != "sparky-p-20261008-100459" {
		t.Errorf("containerName with a non-UTC time = %q, want the UTC rendering", got)
	}
}

func TestContainerName_NewerLaunchSortsAfterOlder(t *testing.T) {
	older := containerName("p", time.Date(2026, 10, 8, 9, 59, 59, 0, time.UTC))
	newer := containerName("p", time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC))
	if !(older < newer) {
		t.Errorf("%q should sort before %q so the newest container is the last in a listing", older, newer)
	}
}
