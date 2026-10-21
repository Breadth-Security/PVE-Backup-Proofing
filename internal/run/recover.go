package run

import (
	"context"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
)

// reasonInterrupted is the run error of a run that never finished.
const reasonInterrupted = "the run was interrupted: PBP stopped or crashed before it finished; the janitor cleans up its guests"

// markInterrupted marks every run still recorded as running as an error,
// ended now, and returns them. The caller holds the run lock, so none of
// them is still going: PBP was killed or crashed during each (code review
// I4). Without this, health kept reporting "ok" for such a run.
func markInterrupted(d Deps) []state.RunRecord {
	var out []state.RunRecord
	err := d.Store.Update(func(st *state.State) error {
		out = nil
		for i := range st.Runs {
			if st.Runs[i].Outcome == state.OutcomeRunning {
				st.Runs[i].Outcome, st.Runs[i].Ended = state.OutcomeError, d.Now()
				out = append(out, st.Runs[i])
			}
		}
		return nil
	})
	if err != nil {
		logger(d).Error("record interrupted runs", "err", err)
		return nil
	}
	return out
}

// Recover runs once when the daemon starts, holding the run lock. With no
// API or target (the node is unreachable) it only marks the runs, and with
// no notifier it sends nothing. A run
// left "running" was interrupted (PBP was killed, crashed, or its host went
// down): it is marked an error, so health reports it, and a notification
// says so. Jobs left in flight are cleaned up by the janitor now, instead of
// at the next scheduled run (code review I4, C1).
func Recover(ctx context.Context, d Deps) {
	log := logger(d)
	for _, r := range markInterrupted(d) {
		log.Error("run " + r.ID + " was interrupted; PBP stopped or crashed before it finished")
		if d.Notifier == nil {
			continue
		}
		s := model.RunSummary{ID: r.ID, Trigger: r.Trigger, Started: r.Started, Ended: r.Ended, RunError: reasonInterrupted}
		nctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		if err := d.Notifier.Send(nctx, s); err != nil {
			log.Error("send notification for the interrupted run", "run", r.ID, "err", err)
		}
		cancel()
	}
	if len(d.Store.Snapshot().InFlight) == 0 || d.API == nil || d.Target == nil {
		// Without a working connection the next run's janitor does it.
		return
	}
	log.Info("cleaning up jobs left in flight")
	if err := Janitor(ctx, d); err != nil {
		log.Error("janitor at startup", "err", err)
	}
}
