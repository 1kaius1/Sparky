// SPDX-License-Identifier: AGPL-3.0-or-later

package engines

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxModelIDLength is the longest profile name (the model id an engine
// serves) Sparky accepts. Long enough for a self-describing name such as
// Qwen3-Coder-Next-NVFP4-GB10-vllm-tp2-ctx32k, short enough to stay
// readable in a client's model picker, a log line and a metrics label.
const MaxModelIDLength = 64

// modelIDSegment is one "/"-separated piece of a model id. It must start
// with a letter or digit, so no segment can be empty, ".", "..", or begin
// with "-" (which an engine's argument parser could mistake for a flag).
const modelIDSegment = `[A-Za-z0-9][A-Za-z0-9._:-]*`

var modelIDPattern = regexp.MustCompile(`^` + modelIDSegment + `(/` + modelIDSegment + `)*$`)

// ValidModelID reports whether name can be used as the id an engine serves
// its model under (vLLM --served-model-name, llama.cpp --alias). The rule is
// shaped like a model reference rather than free text: letters, digits and
// . _ - : inside one or more /-separated segments.
//
// What it rules out, and why: whitespace and unusual punctuation (hard to
// type and copy, awkward in CLI arguments, URLs and metrics labels); a comma
// (llama.cpp splits --alias on commas, silently turning one name into
// several ids); a leading, trailing or doubled "/" and "." or ".." segments
// (they make the id ambiguous as a path-like identifier); non-ASCII.
//
// "/" and ":" are allowed, but some clients treat them specially (GitHub
// Copilot composes its own identifier as vendor/group/id, and llama.cpp's
// router mode needs a URL-encoded id in GET requests), so the profile form
// says so.
func ValidModelID(name string) bool {
	return len(name) <= MaxModelIDLength && modelIDPattern.MatchString(name)
}

// ModelIDRule is the operator-facing statement of ValidModelID, for error
// messages and the profile form.
const ModelIDRule = "letters, digits and . _ - : only, with / allowed between parts; each part must start with a letter or digit; at most 64 characters; no spaces or commas"

// CheckModelID returns nil for a valid model id, or an error naming what is
// wrong with name in terms an operator can act on.
func CheckModelID(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is required")
	case len(name) > MaxModelIDLength:
		return fmt.Errorf("name %q is %d characters; the limit is %d, because it is the model id clients use", name, len(name), MaxModelIDLength)
	case ValidModelID(name):
		return nil
	case strings.ContainsAny(name, " \t\r\n"):
		return fmt.Errorf("name %q contains whitespace; it is the model id clients use, so use only %s", name, ModelIDRule)
	case strings.Contains(name, ","):
		return fmt.Errorf("name %q contains a comma; llama.cpp would split it into several model ids, so use only %s", name, ModelIDRule)
	default:
		return fmt.Errorf("name %q is not a valid model id; use only %s", name, ModelIDRule)
	}
}
