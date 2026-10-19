// Package notes composes, merges and parses the one-line PBP verdict that is
// kept at the top of a backup's notes.
package notes

import (
	"regexp"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

const (
	linePrefix = "PBP "
	sep        = " · "
	// timeLayout is how a line shows when the verdict was reached; the
	// parser's pattern is derived from it so the two cannot drift.
	timeLayout = "2006-01-02 15:04"
)

// timePattern is timeLayout as a regular expression.
var timePattern = strings.NewReplacer("2006", `\d{4}`, "01", `\d{2}`, "02", `\d{2}`, "15", `\d{2}`, "04", `\d{2}`).Replace(timeLayout)

// maxReason caps the reason in a notes line, in characters (runes), so the
// line stays readable as the backup's comment. Notifications keep the full
// reason.
const maxReason = 200

// linePattern matches the start of a PBP line carrying one of verdicts.
func linePattern(verdicts ...model.Verdict) *regexp.Regexp {
	alt := make([]string, len(verdicts))
	for i, v := range verdicts {
		alt[i] = string(v)
	}
	return regexp.MustCompile(`^` + linePrefix + `(` + strings.Join(alt, "|") + `) ` + timePattern + regexp.QuoteMeta(sep))
}

var (
	pbpLine    = linePattern(model.Proven, model.Failed, model.Booted, model.Skipped, model.Error)
	provenLine = linePattern(model.Proven)
	lineBreak  = regexp.MustCompile(`[ \t]*[\r\n][ \t\r\n]*`)
)

// Line composes a PBP notes line, formatting at in its own location. Line
// breaks in reason (and the whitespace around them) become a single space so
// the PBP line never spills into the user's notes. A reason longer than
// maxReason characters is cut and ends in "…".
func Line(v model.Verdict, at time.Time, reason string) string {
	reason = strings.TrimSpace(lineBreak.ReplaceAllString(reason, " "))
	if r := []rune(reason); len(r) > maxReason {
		reason = string(r[:maxReason-1]) + "…"
	}
	return linePrefix + string(v) + " " + at.Format(timeLayout) + sep + reason
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
