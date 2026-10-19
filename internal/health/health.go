// Package health decides whether PBP is healthy from its state store (design
// section 10). It is the backstop for nights when the PVE node itself cannot
// notify, so it needs no network access.
package health

import (
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
)

const (
	// missGrace is how late a scheduled run may start before it counts as
	// missed.
	missGrace = 30 * time.Minute
	// defaultMaxRun bounds a run when max_run_duration is unset.
	defaultMaxRun = 24 * time.Hour
)

// Unhealthy reasons.
const (
	ReasonLastRunError = "last run error"
	ReasonMissed       = "scheduled run missed"
	ReasonOverran      = "run overran"
)

// Evaluate reports whether PBP is healthy at now. It is unhealthy when the
// last finished run ended in a run-level error, when the newest run is still
// running after maxRun (24h if 0), or when the scheduled time following the
// newest run's start (startedAt, the daemon's start, if there are no runs)
// passed more than 30 minutes ago. A zero startedAt with no runs is healthy.
func Evaluate(st state.State, now time.Time, sched cron.Schedule, maxRun time.Duration, startedAt time.Time) (ok bool, reason string) {
	if maxRun <= 0 {
		maxRun = defaultMaxRun
	}
	runs := st.Runs
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Outcome == "running" {
			continue
		}
		if runs[i].Outcome == "error" {
			return false, ReasonLastRunError
		}
		break
	}

	base := startedAt
	if n := len(runs); n > 0 {
		last := runs[n-1]
		if last.Outcome == "running" {
			// The daemon skips ticks while a run is active, so only the
			// run's own length matters.
			if now.Sub(last.Started) > maxRun {
				return false, ReasonOverran
			}
			return true, ""
		}
		base = last.Started
	}
	if base.IsZero() {
		return true, ""
	}
	if due := sched.Next(base); !due.IsZero() && now.After(due.Add(missGrace)) {
		return false, ReasonMissed
	}
	return true, ""
}
