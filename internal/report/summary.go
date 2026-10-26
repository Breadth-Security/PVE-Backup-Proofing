package report

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// DataKeys are the keys of TemplateData, which the node helper accepts in a
// notify payload and no others (code review 2.4).
var DataKeys = []string{"node", "run-id", "headline", "mark", "summary", "counts", "logs", "run-error", "environment-errors",
	"failed", "errored", "booted", "skipped", "previously-proven", "proven", "not-covered"}

// TemplateData builds the data passed to the PVE notification templates.
// The lists hold ready-made lines so the templates only print them (no key
// may be a proxmox-notify helper name, and the templates use only plain
// variables, #if and #each over lists of strings).
func TemplateData(s model.RunSummary, node string) map[string]any {
	loc := s.Started.Location()
	trigger := s.Trigger
	if trigger == "schedule" {
		trigger = "scheduled"
	}
	logs := ""
	if s.LogDir != "" {
		logs = s.LogDir + "/"
	}

	failed, errored, booted := []string{}, []string{}, []string{}
	notCovered := []string{}
	var proven, previously []model.GuestResult
	skipReasons := []string{} // reasons in the order they first occur
	skipGuests := map[string][]string{}
	problemSkips := 0
	for _, r := range s.Results {
		label := r.Backup.Key.Label()
		switch r.Verdict {
		case model.Failed:
			failed = append(failed, guestLine(r, loc))
		case model.Error:
			errored = append(errored, guestLine(r, loc))
		case model.Booted:
			booted = append(booted, guestLine(r, loc))
		case model.Proven:
			proven = append(proven, r)
		case model.Skipped:
			if r.AlreadyProven {
				previously = append(previously, r)
				break
			}
			if _, ok := skipGuests[r.Reason]; !ok {
				skipReasons = append(skipReasons, r.Reason)
			}
			skipGuests[r.Reason] = append(skipGuests[r.Reason], label)
			if !r.AlreadyProven && !r.Template && !r.Excluded {
				problemSkips++
			}
		}
		for _, n := range r.NotCovered {
			notCovered = append(notCovered, clipLine(label+": "+n.LogLine()))
		}
	}
	skipped := []string{}
	for _, reason := range skipReasons {
		skipped = append(skipped, reason+": "+strings.Join(skipGuests[reason], ", "))
	}

	return map[string]any{
		"node":   node,
		"run-id": s.ID,
		"headline": fmt.Sprintf("PBP run on %s, %s (%s), %s", node,
			s.Started.Format("Mon 2006-01-02 15:04 MST"), trigger, Dur(s.Ended.Sub(s.Started))),
		"mark":    SeverityMark(s.Severity()),
		"counts":  CountsLine(s),
		"summary": subjectSummary(s.RunError != "", len(failed), len(errored), len(booted), problemSkips, len(proven)),
		"logs":    logs,
		// run-error and environment-errors are unchanged.
		"run-error":          s.RunError,
		"environment-errors": append([]string{}, s.EnvironmentErrors...),
		"failed":             failed,
		"errored":            errored,
		"booted":             booted,
		"skipped":            skipped,
		"previously-proven":  nameLines(previously),
		"proven":             nameLines(proven),
		"not-covered":        notCovered,
	}
}

// nameLines lists guests one per line as "VM110: db01" ("VM110" without a
// name), containers first, then by VMID (issue #77).
func nameLines(rs []model.GuestResult) []string {
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b model.GuestResult) int {
		if c := strings.Compare(a.Backup.Key.Label()[:2], b.Backup.Key.Label()[:2]); c != 0 {
			return c
		}
		return a.Backup.Key.VMID - b.Backup.Key.VMID
	})
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		line := r.Backup.Key.Label()
		if r.Name != "" {
			line += ": " + r.Name
		}
		out = append(out, clipLine(line))
	}
	return out
}

// guestLine is one guest in a problem section:
// "VM102 web01 · backup Oct 20 02:00 · reason" (no name: no extra space).
func guestLine(r model.GuestResult, loc *time.Location) string {
	who := r.Backup.Key.Label()
	if r.Name != "" {
		who += " " + r.Name
	}
	return clipLine(fmt.Sprintf("%s · backup %s · %s", who, r.Backup.Time.In(loc).Format("Jan 2 15:04"), r.Reason))
}

// maxLine bounds one line of the notification; the full text is in the
// guest's log file (code review I14).
const maxLine = 600

// clipLine cuts s to at most maxLine bytes on a rune boundary, marking the
// cut.
func clipLine(s string) string {
	if len(s) <= maxLine {
		return s
	}
	n := maxLine - len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// subjectSummary is the notification subject's text: the non-zero problem
// counts first (a run error leads), then the proven count; a clean run reads
// "all N proven", an empty one "nothing to proof".
func subjectSummary(runError bool, failed, errored, booted, skipped, proven int) string {
	var parts []string
	if runError {
		parts = append(parts, "run error")
	}
	for _, c := range []struct {
		n    int
		word string
	}{{failed, "failed"}, {errored, "error"}, {booted, "booted"}, {skipped, "skipped"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	switch {
	case len(parts) > 0:
		return fmt.Sprintf("%s · %d proven", strings.Join(parts, ", "), proven)
	case proven > 0:
		return fmt.Sprintf("all %d proven", proven)
	}
	return "nothing to proof"
}

// Markers for the verdicts, so a summary reads at a glance: green for
// proven, red for a backup with a problem, yellow for a guest PBP could
// not prove either way.
const (
	MarkProven  = "✅"
	MarkFailed  = "❌"
	MarkWarn    = "⚠️"
	MarkSkipped = "⏭️"
	// MarkAlready is a backup proven by an earlier run.
	MarkAlready = "☑️"
)

// SeverityMark is the marker for a run's severity: ✅ info, ⚠️ warning,
// ❌ error.
func SeverityMark(severity string) string {
	switch severity {
	case "error":
		return MarkFailed
	case "warning":
		return MarkWarn
	}
	return MarkProven
}

// CountsLine is the run's verdict counts with their markers, the non-zero
// ones only: "✅ 13 proven · ☑️ 20 previously proven · ❌ 1 failed · ⚠️ 2
// error · ⚠️ 4 booted · ⏭️ 3 skipped"; "nothing proofed" when there are
// none. A backup skipped as already proven has its own count, so "skipped"
// holds only those not proofed for another reason.
func CountsLine(s model.RunSummary) string {
	c := s.Counts()
	already := 0
	for _, r := range s.Results {
		if r.Verdict == model.Skipped && r.AlreadyProven {
			already++
		}
	}
	var parts []string
	for _, v := range []struct {
		n    int
		mark string
		word string
	}{
		{c[model.Proven], MarkProven, "proven"},
		{already, MarkAlready, "previously proven"},
		{c[model.Failed], MarkFailed, "failed"},
		{c[model.Error], MarkWarn, "error"},
		{c[model.Booted], MarkWarn, "booted"},
		{c[model.Skipped] - already, MarkSkipped, "skipped"},
	} {
		if n := v.n; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d %s", v.mark, n, v.word))
		}
	}
	if len(parts) == 0 {
		return "nothing proofed"
	}
	return strings.Join(parts, " · ")
}
