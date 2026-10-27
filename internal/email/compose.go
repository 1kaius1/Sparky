// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/db"
)

const (
	// maxTitleInMail and maxBodyInMail bound what of an alert goes in an email.
	// Alert text is already a summary (names and counts, never log or prompt
	// content - PLANNING.md, 2026-10-26); the bound only keeps a runaway
	// producer from making a huge message.
	maxTitleInMail = 200
	maxBodyInMail  = 1500

	// maxDigestItems is how many alerts one digest lists; the rest are counted.
	maxDigestItems = 50
)

// oneLine makes s safe for a subject or a single line of a body: control
// characters (including line breaks and tabs) become spaces, runs of space
// collapse, and it is cut to max runes.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max-3]) + "..."
	}
	return s
}

// multiLine keeps an alert's body readable: line breaks stay, every other
// control character goes, and it is cut to max runes.
func multiLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max-3]) + "..."
	}
	return s
}

// alertLink is where an alert can be opened, "" when this server has no public
// address to point at.
func alertLink(publicURL, alertID string) string {
	if publicURL == "" {
		return ""
	}
	return publicURL + "/alerts?selected=" + alertID
}

func footer(publicURL, how string) string {
	where := "on the My account page in Sparky"
	if publicURL != "" {
		where = publicURL + "/account"
	}
	return "\n--\nYou get this because an alert was addressed to you and your alert email setting is \"" + how + "\". " +
		"You can change that, or turn alert email off, " + where + ".\n"
}

// alertMessage writes the email for one alert: a summary and a link, never the
// content of any log or prompt.
func alertMessage(a *db.Alert, escalated bool, loc *time.Location, publicURL string) (subject, body string) {
	title := oneLine(a.Title, maxTitleInMail)
	label := string(a.Severity)
	if escalated {
		label += " (escalated)"
	}
	subject = "[Sparky] " + label + ": " + title

	var b strings.Builder
	b.WriteString(title + "\n\n")
	fmt.Fprintf(&b, "Severity: %s\n", a.Severity)
	fmt.Fprintf(&b, "When:     %s\n", a.LastRaisedAt.In(loc).Format("2006-01-02 15:04 MST"))
	if escalated {
		b.WriteString("This alert was already open and has become more severe.\n")
	} else if a.Occurrences > 1 {
		fmt.Fprintf(&b, "Seen:     %d times\n", a.Occurrences)
	}
	if body := multiLine(a.Body, maxBodyInMail); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	if link := alertLink(publicURL, a.ID); link != "" {
		b.WriteString("\nOpen this alert: " + link + "\n")
	} else {
		b.WriteString("\nOpen the Alerts inbox in Sparky to see it.\n")
	}
	b.WriteString(footer(publicURL, "immediately"))
	return subject, b.String()
}

// digestEntry is one alert in a digest.
type digestEntry struct {
	alertID  string
	severity db.AlertSeverity
	title    string
	queued   time.Time
}

// digestMessage writes one email listing the alerts that arrived since a
// person's last digest, worst first.
func digestMessage(entries []digestEntry, how string, loc *time.Location, publicURL string) (subject, body string) {
	sort.SliceStable(entries, func(i, j int) bool {
		if ri, rj := entries[i].severity.Rank(), entries[j].severity.Rank(); ri != rj {
			return ri > rj
		}
		return entries[i].queued.Before(entries[j].queued)
	})

	noun := "alerts"
	if len(entries) == 1 {
		noun = "alert"
	}
	subject = fmt.Sprintf("[Sparky] %d new %s (%s digest)", len(entries), noun, how)

	var b strings.Builder
	fmt.Fprintf(&b, "These alerts arrived since your last %s digest and are still open:\n\n", how)
	for i, e := range entries {
		if i >= maxDigestItems {
			fmt.Fprintf(&b, "... and %d more. Open the Alerts inbox in Sparky to see them all.\n", len(entries)-maxDigestItems)
			break
		}
		fmt.Fprintf(&b, "[%s] %s  (%s)\n", e.severity, oneLine(e.title, maxTitleInMail), e.queued.In(loc).Format("2006-01-02 15:04 MST"))
		if link := alertLink(publicURL, e.alertID); link != "" {
			b.WriteString("    " + link + "\n")
		}
	}
	b.WriteString(footer(publicURL, how+" digest"))
	return subject, b.String()
}
