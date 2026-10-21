package report

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

// DataKeys are the keys of TemplateData, which the node helper accepts in a
// notify payload and no others (code review 2.4).
var DataKeys = []string{"node", "run-id", "headline", "summary", "logs", "run-error", "environment-errors",
	"failed", "errored", "booted", "skipped", "proven", "not-covered"}

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
	var proven []string
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
			proven = append(proven, label)
		case model.Skipped:
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
		"summary": subjectSummary(s.RunError != "", len(failed), len(errored), len(booted), problemSkips, len(proven)),
		"logs":    logs,
		// run-error and environment-errors are unchanged.
		"run-error":          s.RunError,
		"environment-errors": append([]string{}, s.EnvironmentErrors...),
		"failed":             failed,
		"errored":            errored,
		"booted":             booted,
		"skipped":            skipped,
		"proven":             strings.Join(proven, ", "),
		"not-covered":        notCovered,
	}
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
