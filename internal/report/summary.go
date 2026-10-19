package report

import (
	"fmt"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// TemplateData builds the data passed to the PVE notification templates.
func TemplateData(s model.RunSummary, node string) map[string]any {
	counts := map[string]int{}
	for v, n := range s.Counts() {
		counts[string(v)] = n
	}
	guests := []map[string]any{}
	booted := []map[string]any{}
	notCovered := []string{}
	for _, r := range s.Results {
		g := map[string]any{
			"guest":   r.Backup.Key.String(),
			"name":    r.Name,
			"verdict": string(r.Verdict),
			"reason":  r.Reason,
		}
		guests = append(guests, g)
		if r.Verdict == model.Booted {
			booted = append(booted, g)
		}
		for _, n := range r.NotCovered {
			notCovered = append(notCovered, n.LogLine())
		}
	}
	return map[string]any{
		"node":        node,
		"run-id":      s.ID,
		"started":     s.Started.Format(time.RFC3339),
		"elapsed":     s.Ended.Sub(s.Started).Round(time.Second).String(),
		"counts":      counts,
		"summary":     summaryLine(counts),
		"guests":      guests,
		"booted":      booted,
		"not-covered": notCovered,
		"run-error":   s.RunError,
	}
}

func summaryLine(c map[string]int) string {
	return fmt.Sprintf("%d proven, %d failed, %d booted, %d skipped, %d error",
		c["PROVEN"], c["FAILED"], c["BOOTED"], c["SKIPPED"], c["ERROR"])
}
