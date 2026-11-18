// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The report is plain text that a person reads and then chooses to share outside
// their organization, so it is built to be safe by construction, in layers:
//
//  1. A step can only add typed facts: a pass or fail, a count, a yes or no, a
//     short token from a fixed vocabulary, or a set of names. There is no way to
//     add free text or an error message.
//  2. A text fact is dropped unless it looks like a protocol token, not an
//     identifier (safeValue).
//  3. Names that come from a response (claim names, document members) are shown
//     only if they are on a list of standard names; anything else is shown as
//     "other-N", because a custom name can embed an application ID.
//  4. When the report is rendered, it is refused outright if it contains a value
//     the probe registered as sensitive (the tenant ID, the secret, every token
//     and identifier it handled), or anything shaped like a GUID, a JWT, an
//     email address or a URL, whether or not it was registered.
//
// The refusal in (4) is the net under (1) to (3): it means a mistake in the
// code that builds a step cannot reach the output.

var (
	guidRE  = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	jwtRE   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}`)
	runRE   = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
	tokenRE = regexp.MustCompile(`^[A-Za-z0-9_.:/ ,+()-]{0,80}$`)
)

// errUnsafeReport is returned by Render. It never says what matched.
var errUnsafeReport = errors.New("the report was not written because it contained something that looks like an identifier, a token, an address or a URL")

// hasOpaqueRun reports whether s contains a long unbroken run that looks random
// - several digits or capitals - as an identifier, key or token does. A long
// protocol name such as token_endpoint_auth_methods_supported is all lower case
// and does not count.
func hasOpaqueRun(s string) bool {
	for _, run := range runRE.FindAllString(s, -1) {
		digits, upper := 0, 0
		for _, c := range run {
			switch {
			case c >= '0' && c <= '9':
				digits++
			case c >= 'A' && c <= 'Z':
				upper++
			}
		}
		if digits >= 2 || upper >= 3 {
			return true
		}
	}
	return false
}

// safeValue reports whether s may appear in the report as a fact's text: short,
// made of the characters protocol keywords use, and nothing like a GUID, token
// or other long opaque string.
func safeValue(s string) bool {
	return tokenRE.MatchString(s) && !guidRE.MatchString(s) && !jwtRE.MatchString(s) && !hasOpaqueRun(s) &&
		!strings.Contains(s, "://")
}

// Report collects the results of the steps.
type Report struct {
	mu        sync.Mutex
	sensitive map[string]bool
	steps     []*Step
	suppress  int
	pseudo    *Pseudonyms
}

// NewReport starts a report. sensitive are values that must never appear in it.
func NewReport(sensitive ...string) *Report {
	r := &Report{sensitive: map[string]bool{}, pseudo: NewPseudonyms()}
	r.Watch(sensitive...)
	return r
}

// Watch registers values that must not appear in the report. Anything the probe
// receives that identifies a person, an application or a tenant, or that is a
// credential, is registered as soon as it is seen.
func (r *Report) Watch(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) >= 4 {
			r.sensitive[strings.ToLower(v)] = true
		}
	}
}

// Pseudonyms returns the report's pseudonym table.
func (r *Report) Pseudonyms() *Pseudonyms { return r.pseudo }

// Begin starts a step.
func (r *Report) Begin(name string) *Step {
	s := &Step{report: r, Name: name, started: time.Now()}
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
	return s
}

// Step is one numbered check in the report.
type Step struct {
	report  *Report
	Name    string
	Outcome string // "pass", "fail" or "skipped"
	Reason  string // a fixed-vocabulary description of a failure or skip
	started time.Time
	elapsed time.Duration
	lines   []string
}

// Pass marks the step as passed.
func (s *Step) Pass() { s.finish("pass", "") }

// Fail marks the step as failed. kind is a fixed-vocabulary description such as
// entra.ErrorKind returns, never an error message.
func (s *Step) Fail(kind string) { s.finish("fail", kind) }

// Skip marks the step as not run, with a fixed reason.
func (s *Step) Skip(reason string) { s.finish("skipped", reason) }

func (s *Step) finish(outcome, reason string) {
	s.Outcome = outcome
	s.Reason = s.clean(reason)
	s.elapsed = time.Since(s.started)
}

// clean returns v if it is safe to print and a marker otherwise, counting what
// it hid so the reader knows something was left out.
func (s *Step) clean(v string) string {
	if safeValue(v) {
		return v
	}
	s.report.mu.Lock()
	s.report.suppress++
	s.report.mu.Unlock()
	return "[not shown]"
}

func (s *Step) add(format string, args ...any) {
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

// Fact records a keyword-like value.
func (s *Step) Fact(key, value string) { s.add("%s: %s", key, s.clean(value)) }

// Count records a number.
func (s *Step) Count(key string, n int) { s.add("%s: %d", key, n) }

// Flag records a yes or no.
func (s *Step) Flag(key string, v bool) {
	if v {
		s.add("%s: yes", key)
		return
	}
	s.add("%s: no", key)
}

// Set records a sorted set of keyword-like values. Values that are not safe are
// counted, not shown.
func (s *Step) Set(key string, values []string) {
	shown := make([]string, 0, len(values))
	hidden := 0
	for _, v := range values {
		if safeValue(v) {
			shown = append(shown, v)
		} else {
			hidden++
		}
	}
	sort.Strings(shown)
	shown = dedupe(shown)
	line := key + ": " + strings.Join(shown, ", ")
	if len(shown) == 0 {
		line = key + ": (none)"
	}
	if hidden > 0 {
		line += fmt.Sprintf(" (+%d not shown)", hidden)
		s.report.mu.Lock()
		s.report.suppress += hidden
		s.report.mu.Unlock()
	}
	s.lines = append(s.lines, line)
}

// Names records a set of names, showing only those on the vocabulary.
func (s *Step) Names(key string, names []string, vocab Vocabulary) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	shown := make([]string, 0, len(sorted))
	other := 0
	for _, n := range sorted {
		if vocab.Known(n) {
			shown = append(shown, n)
		} else {
			other++
			shown = append(shown, fmt.Sprintf("other-%d", other))
		}
	}
	if len(shown) == 0 {
		s.add("%s: (none)", key)
		return
	}
	s.add("%s: %s", key, strings.Join(shown, ", "))
}

// Shape records names with the type of each one's value, such as the claims in a
// token, showing only names on the vocabulary.
func (s *Step) Shape(key string, types map[string]string, vocab Vocabulary) {
	names := make([]string, 0, len(types))
	for n := range types {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	other := 0
	for _, n := range names {
		shown := n
		if !vocab.Known(n) {
			other++
			shown = fmt.Sprintf("other-%d", other)
		}
		parts = append(parts, shown+":"+types[n])
	}
	if len(parts) == 0 {
		s.add("%s: (none)", key)
		return
	}
	s.add("%s: %s", key, strings.Join(parts, ", "))
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Overall reports whether no step failed.
func (r *Report) Overall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.steps {
		if s.Outcome == "fail" || s.Outcome == "" {
			return false
		}
	}
	return true
}

// Render writes the report, or refuses and writes nothing.
func (r *Report) Render(w io.Writer, header ...string) error {
	r.mu.Lock()
	var b strings.Builder
	for _, h := range header {
		b.WriteString(h)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	passed, failed, skipped := 0, 0, 0
	for i, s := range r.steps {
		outcome := s.Outcome
		if outcome == "" {
			outcome = "fail"
		}
		switch outcome {
		case "pass":
			passed++
		case "skipped":
			skipped++
		default:
			failed++
		}
		fmt.Fprintf(&b, "Step %d: %s\n", i+1, s.Name)
		fmt.Fprintf(&b, "  result: %s", outcome)
		if s.Reason != "" {
			fmt.Fprintf(&b, " (%s)", s.Reason)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "  took: %s\n", roundDuration(s.elapsed))
		for _, l := range s.lines {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Summary: %d passed, %d failed, %d skipped\n", passed, failed, skipped)
	if r.suppress > 0 {
		fmt.Fprintf(&b, "Values left out of this report because they did not look safe to show: %d\n", r.suppress)
	}
	text := b.String()
	watched := make([]string, 0, len(r.sensitive))
	for v := range r.sensitive {
		watched = append(watched, v)
	}
	r.mu.Unlock()

	if looksUnsafe(text, watched) {
		return errUnsafeReport
	}
	_, err := io.WriteString(w, text)
	return err
}

// looksUnsafe is the last check: anything registered as sensitive, and anything
// shaped like an identifier, a token, an address or a URL.
func looksUnsafe(text string, watched []string) bool {
	lower := strings.ToLower(text)
	for _, v := range watched {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return guidRE.MatchString(text) || jwtRE.MatchString(text) || hasOpaqueRun(text) ||
		strings.Contains(text, "://") || strings.Contains(text, "@")
}

// roundDuration keeps timings coarse: they are useful for spotting a slow
// endpoint and finer than that identify nothing, but there is no reason to
// print more.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(100 * time.Millisecond).String()
	}
}

// Pseudonyms gives the same label to the same value for the length of one run,
// so a report can say that two things are the same person or tenant without
// saying who. The mapping lives only in memory and is never written anywhere,
// so a label cannot be turned back into the value or matched with another run.
type Pseudonyms struct {
	mu     sync.Mutex
	labels map[string]map[string]string
}

// NewPseudonyms returns an empty table.
func NewPseudonyms() *Pseudonyms { return &Pseudonyms{labels: map[string]map[string]string{}} }

// Label returns the label for value within kind: "user-1", "user-2" for kind
// "user" and "tenant-A", "tenant-B" for kind "tenant", in order of first sight.
func (p *Pseudonyms) Label(kind, value string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.labels[kind]
	if m == nil {
		m = map[string]string{}
		p.labels[kind] = m
	}
	key := strings.ToLower(value)
	if l, ok := m[key]; ok {
		return l
	}
	n := len(m)
	var l string
	if kind == "tenant" {
		l = fmt.Sprintf("%s-%c", kind, 'A'+rune(n%26))
	} else {
		l = fmt.Sprintf("%s-%d", kind, n+1)
	}
	m[key] = l
	return l
}
