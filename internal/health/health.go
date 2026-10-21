// Package health decides whether PBP is healthy from its state store (design
// section 10). It is the backstop for nights when the PVE node itself cannot
// notify, so it needs no network access.
package health

import (
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
)

const (
	// missGrace is how late a scheduled run may start before it counts as
	// missed.
	missGrace = 30 * time.Minute
	// defaultMaxRun bounds a run when max_run_duration is unset.
	defaultMaxRun = 24 * time.Hour
	// overrunGrace is added to the configured run length before a run
	// counts as overrun.
	overrunGrace = 10 * time.Minute
)

// Unhealthy reasons.
const (
	ReasonLastRunError = "last run error"
	ReasonMissed       = "scheduled run missed"
	ReasonOverran      = "run overran"
)

// RunLimits are the configured durations that bound one run (restore
// max_run_duration, boot_timeout and probe_timeout).
type RunLimits struct {
	MaxRun, BootTimeout, ProbeTimeout time.Duration
}

// OverrunLimit is how long a run may last before it counts as overrun. When
// max_run_duration is set, a run may legitimately finish its in-flight guest
// after the limit (restore boot and probe) plus a grace period; when it is
// unset the limit is 24h.
func OverrunLimit(maxRun, bootTimeout, probeTimeout time.Duration) time.Duration {
	if maxRun <= 0 {
		return defaultMaxRun
	}
	return maxRun + bootTimeout + probeTimeout + overrunGrace
}

// Evaluate reports whether PBP is healthy at now. It is unhealthy when:
//   - the last finished run ended in a run-level error;
//   - the newest run is still running for longer than OverrunLimit allows
//     for limits;
//   - the scheduled time following the newest run's end (its start if no end
//     was recorded; startedAt, the daemon's start, if there are no runs)
//     passed more than 30 minutes ago.
//
// A zero startedAt with no runs is healthy.
func Evaluate(st state.State, now time.Time, sched cron.Schedule, limits RunLimits, startedAt time.Time, zone *time.Location) (ok bool, reason string) {
	if zone == nil {
		zone = time.Local
	}
	limit := OverrunLimit(limits.MaxRun, limits.BootTimeout, limits.ProbeTimeout)
	runs := st.Runs
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Outcome == state.OutcomeRunning {
			continue
		}
		if runs[i].Outcome == state.OutcomeError {
			return false, ReasonLastRunError
		}
		break
	}

	base := startedAt
	if n := len(runs); n > 0 {
		last := runs[n-1]
		if last.Outcome == state.OutcomeRunning {
			// The daemon skips ticks while a run is active, so only the
			// run's own length matters. A restore in progress has no
			// timeout of its own: a large one admitted just before
			// max_run_duration may run long, and is not an overrun until
			// the hard cap (code review 6.12).
			el := now.Sub(last.Started)
			if el > limit && (!restoring(st) || el > defaultMaxRun) {
				return false, ReasonOverran
			}
			return true, ""
		}
		// Ticks during a run are skipped on purpose, so the next one due
		// is the first after the run ended.
		base = last.Ended
		if base.IsZero() {
			base = last.Started
		}
	}
	if base.IsZero() {
		return true, ""
	}
	// Stored times keep the offset they were written with; the schedule is
	// in the daemon's zone, which may differ from this process's (a TZ set
	// only on the service; code review 6.6).
	if due := sched.Next(base.In(zone)); !due.IsZero() && now.After(due.Add(missGrace)) {
		return false, ReasonMissed
	}
	return true, ""
}

// restoring reports whether a proof in flight is restoring (its restore task
// may still be running).
func restoring(st state.State) bool {
	for _, j := range st.InFlight {
		if j.Stage == state.StageRestored {
			return true
		}
	}
	return false
}
