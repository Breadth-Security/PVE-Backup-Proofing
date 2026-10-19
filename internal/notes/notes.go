// Package notes composes, merges and parses the one-line PBP verdict that is
// kept at the top of a backup's notes.
package notes

import (
	"regexp"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

const sep = " · "

var (
	pbpLine    = regexp.MustCompile(`^PBP (PROVEN|FAILED|BOOTED|SKIPPED|ERROR) \d{4}-\d{2}-\d{2} \d{2}:\d{2} · `)
	provenLine = regexp.MustCompile(`^PBP PROVEN \d{4}-\d{2}-\d{2} \d{2}:\d{2} · `)
)

// Line composes a PBP notes line, formatting at in its own location.
func Line(v model.Verdict, at time.Time, reason string) string {
	return "PBP " + string(v) + " " + at.Format("2006-01-02 15:04") + sep + reason
}

// Merge puts pbpLine on the first line of existing, dropping any previous PBP
// lines and preserving all other lines. Trailing newlines are trimmed.
func Merge(existing, line string) string {
	existing = strings.TrimRight(existing, "\n")
	out := []string{line}
	if existing != "" {
		for _, l := range strings.Split(existing, "\n") {
			if !pbpLine.MatchString(l) {
				out = append(out, l)
			}
		}
	}
	return strings.Join(out, "\n")
}

// HasProven reports whether notes contain a PBP line with verdict PROVEN.
func HasProven(notes string) bool {
	for _, l := range strings.Split(notes, "\n") {
		if provenLine.MatchString(l) {
			return true
		}
	}
	return false
}
