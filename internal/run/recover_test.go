package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
)

// After PBP was killed or crashed mid-run, Recover (at daemon start) marks
// the run that was left "running" as an error, so health reports it, sends a
// notification saying so, and runs the janitor at once instead of at the
// next scheduled run (code review I4, C1).
func TestRecoverInterruptedRun(t *testing.T) {
	f := newJanitorFixture(t)
	notif := &fakeNotifier{}
	f.d.Notifier = notif
	f.guest(model.KindQEMU, 990000, true)
	f.backup("pbs:v0", true)
	f.inFlight(state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageStarted})
	started := now.Add(-3 * time.Hour)
	if err := f.store.AppendRun(state.RunRecord{ID: "r-old", Trigger: "schedule", Outcome: state.OutcomeOK, Started: started.Add(-24 * time.Hour), Ended: started.Add(-23 * time.Hour)}, 30); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendRun(state.RunRecord{ID: "r-killed", Trigger: "schedule", Outcome: state.OutcomeRunning, Started: started}, 30); err != nil {
		t.Fatal(err)
	}

	Recover(context.Background(), f.d)

	runs := f.store.Snapshot().Runs
	if last := runs[len(runs)-1]; last.ID != "r-killed" || last.Outcome != state.OutcomeError || !last.Ended.Equal(now) {
		t.Errorf("interrupted run = %+v, want outcome error, ended now", last)
	}
	if runs[0].Outcome != state.OutcomeOK {
		t.Errorf("an earlier finished run changed: %+v", runs[0])
	}
	sent := notif.sends()
	if len(sent) != 1 || sent[0].ID != "r-killed" || !strings.Contains(sent[0].RunError, "interrupted") {
		t.Fatalf("notifications = %+v, want one for r-killed saying interrupted", sent)
	}
	if f.exists(990000) || f.protected("pbs:v0") {
		t.Error("the janitor did not clean up the interrupted job")
	}
	if len(f.store.Snapshot().InFlight) != 0 {
		t.Error("InFlight not empty after the janitor")
	}
}

// Nothing interrupted: Recover changes nothing and sends nothing.
func TestRecoverNothingToDo(t *testing.T) {
	f := newJanitorFixture(t)
	notif := &fakeNotifier{}
	f.d.Notifier = notif
	if err := f.store.AppendRun(state.RunRecord{ID: "r1", Trigger: "schedule", Outcome: state.OutcomeOK, Started: now, Ended: now}, 30); err != nil {
		t.Fatal(err)
	}
	Recover(context.Background(), f.d)
	if r := f.store.Snapshot().Runs[0]; r.Outcome != state.OutcomeOK {
		t.Errorf("run = %+v", r)
	}
	if n := len(notif.sends()); n != 0 {
		t.Errorf("%d notifications, want none", n)
	}
}

// A run that starts while an earlier one is still recorded "running" holds
// the lock, so that one was interrupted (for example a killed manual run):
// it is marked an error, and this run's summary says so.
func TestRunMarksStaleRunningRecord(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	if err := f.d.Store.AppendRun(state.RunRecord{ID: "r-killed", Trigger: "manual", Outcome: state.OutcomeRunning, Started: now.Add(-time.Hour)}, 30); err != nil {
		t.Fatal(err)
	}
	s := f.run(context.Background(), Options{Trigger: "schedule"})
	var killed state.RunRecord
	for _, r := range f.d.Store.Snapshot().Runs {
		if r.ID == "r-killed" {
			killed = r
		}
	}
	if killed.Outcome != state.OutcomeError {
		t.Errorf("stale record = %+v, want outcome error", killed)
	}
	if !strings.Contains(strings.Join(s.EnvironmentErrors, "\n"), "r-killed was interrupted") {
		t.Errorf("environment errors = %q", s.EnvironmentErrors)
	}
}
