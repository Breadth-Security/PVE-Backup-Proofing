// Package notes composes, merges and parses the one-line PBP verdict that is
// kept at the top of a backup's notes, after the guest's name: the first
// line is the backup's comment in PVE and PBS, which is what a search by
// guest name looks at (issue #76).
package notes

import (
	"regexp"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

const (
	linePrefix = "PBP "
	sep        = " · "
	// timeLayout is how a line shows when the verdict was reached, and
	// timePattern matches it (TestLineRoundTrips guards against drift).
	timeLayout  = "2006-01-02 15:04"
	timePattern = `\d{4}-\d{2}-\d{2} \d{2}:\d{2}`
)

// maxName caps the guest name in front of a PBP line, in runes.
const maxName = 64

// maxReason caps the reason in a notes line, in characters (runes), so the
// line stays readable as the backup's comment. Notifications keep the full
// reason.
const maxReason = 200

// linePattern matches the start of a PBP line carrying one of verdicts:
// "<name> · PBP <VERDICT> <time> · ", or the name-less form 0.5 to 0.7
// wrote ("PBP <VERDICT> <time> · ") when old is set.
func linePattern(old bool, verdicts ...model.Verdict) *regexp.Regexp {
	alt := make([]string, len(verdicts))
	for i, v := range verdicts {
		alt[i] = string(v)
	}
	name := `(?:.*` + regexp.QuoteMeta(sep) + `)?`
	if old {
		name = ""
	}
	return regexp.MustCompile(`^` + name + linePrefix + `(` + strings.Join(alt, "|") + `) ` + timePattern + regexp.QuoteMeta(sep))
}

var (
	allVerdicts = []model.Verdict{model.Proven, model.Failed, model.Booted, model.Skipped, model.Error}
	pbpLine     = linePattern(false, allVerdicts...)
	oldLine     = linePattern(true, allVerdicts...)
	provenLine  = linePattern(false, model.Proven)
	lineBreak   = regexp.MustCompile(`[ \t]*[\r\n][ \t\r\n]*`)
)

// Line composes a PBP notes line for the guest called name, formatting at in
// its own location: "db01 · PBP PROVEN 2026-10-19 02:14 · 12 checks ok".
// Line breaks in name and reason (and the whitespace around them) become a
// single space so the PBP line never spills into the user's notes. A name
// longer than maxName or a reason longer than maxReason characters is cut
// and ends in "…". An empty name gives the line without one.
func Line(name string, v model.Verdict, at time.Time, reason string) string {
	line := linePrefix + string(v) + " " + at.Format(timeLayout) + sep + clean(reason, maxReason)
	if name = clean(name, maxName); name != "" {
		line = name + sep + line
	}
	return line
}

// clean makes s one trimmed line of at most n runes.
func clean(s string, n int) string {
	s = strings.TrimSpace(lineBreak.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// Merge puts line first in existing, dropping any previous PBP lines (in
// either format) and preserving all other lines. When the first remaining
// line is just the guest's name, as vzdump's default notes template
// ("{{guestname}}") writes it, it is dropped too: line already starts with
// the name. Trailing newlines are trimmed.
func Merge(existing, line, name string) string {
	existing = strings.TrimRight(existing, "\n")
	out := []string{line}
	if existing != "" {
		for _, l := range strings.Split(existing, "\n") {
			if !pbpLine.MatchString(l) {
				out = append(out, l)
			}
		}
	}
	if len(out) > 1 && name != "" && isName(out[1], name) {
		out = append(out[:1], out[2:]...)
	}
	return strings.Join(out, "\n")
}

// isName reports whether a notes line is just the guest's name, uncut or as
// Line cuts it.
func isName(line, name string) bool {
	line = strings.TrimSpace(line)
	return line == clean(name, maxName) || line == clean(name, len(name)+1)
}

// namedLine captures the guest's name in front of a PBP line.
var namedLine = regexp.MustCompile(`^(.+?)` + regexp.QuoteMeta(sep) + linePrefix + `(?:PROVEN|FAILED|BOOTED|SKIPPED|ERROR) ` + timePattern + regexp.QuoteMeta(sep))

// Name is the guest's name in front of the PBP line that starts notes, or ""
// when the first line is not one in that form.
func Name(notes string) string {
	if m := namedLine.FindStringSubmatch(notes); m != nil {
		return m[1]
	}
	return ""
}

// OldFormat reports whether notes start with a PBP line in the name-less
// format 0.5 to 0.7 wrote, which hides the guest's name from the backup's
// comment.
func OldFormat(notes string) bool {
	return oldLine.MatchString(notes)
}

// Migrate rewrites notes whose first line is a PBP line in the old format so
// that it starts with name, keeping the verdict, time and reason. It
// reports false, and returns notes unchanged, for notes in any other form.
func Migrate(notes, name string) (string, bool) {
	short := clean(name, maxName)
	if short == "" || !OldFormat(notes) {
		return notes, false
	}
	first, _, _ := strings.Cut(notes, "\n")
	first = strings.TrimRight(first, "\r")
	return Merge(notes, short+sep+first, name), true
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
