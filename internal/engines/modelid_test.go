// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"strings"
	"testing"
)

func TestValidModelID(t *testing.T) {
	valid := []string{
		"a", "7", "qwen3", "qwen3-coder_30b.v2", "qwen2.5-coder:7b", "team/qwen:7b",
		"org/sub/Model-1", "Qwen3-Coder-Next-NVFP4-GB10-vllm-tp2-ctx32k",
		strings.Repeat("a", 64),
	}
	for _, n := range valid {
		if !ValidModelID(n) {
			t.Errorf("ValidModelID(%q) = false, want true", n)
		}
		if err := CheckModelID(n); err != nil {
			t.Errorf("CheckModelID(%q) = %v, want nil", n, err)
		}
	}

	invalid := map[string]string{
		"":                      "required",
		"my model":              "whitespace",
		" lead":                 "whitespace",
		"trail ":                "whitespace",
		"tab\tname":             "whitespace",
		"a,b":                   "comma",
		"/x":                    "not a valid",
		"x/":                    "not a valid",
		"a//b":                  "not a valid",
		"../x":                  "not a valid",
		"a/./b":                 "not a valid",
		"-fast":                 "not a valid",
		"_x":                    "not a valid",
		".x":                    "not a valid",
		":7b":                   "not a valid",
		"a/-b":                  "not a valid",
		"qwen@3":                "not a valid",
		"qwen 3":                "whitespace",
		"modèle":                "not a valid",
		"name\n":                "whitespace",
		strings.Repeat("a", 65): "limit",
	}
	for n, wantErr := range invalid {
		if ValidModelID(n) {
			t.Errorf("ValidModelID(%q) = true, want false", n)
		}
		err := CheckModelID(n)
		if err == nil {
			t.Errorf("CheckModelID(%q) = nil, want an error", n)
			continue
		}
		if !strings.Contains(err.Error(), wantErr) {
			t.Errorf("CheckModelID(%q) = %q, want it to mention %q", n, err, wantErr)
		}
	}
}
