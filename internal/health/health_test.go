package health

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/state"
)

func TestEvaluate(t *testing.T) {
	sched, err := cron.ParseStandard("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.Local
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) }
	rec := func(outcome string, started time.Time) state.RunRecord {
		r := state.RunRecord{ID: started.Format(time.RFC3339), Trigger: "schedule", Outcome: outcome, Started: started}
		if outcome != state.OutcomeRunning {
			r.Ended = started.Add(time.Hour)
		}
		return r
	}
	longAgo := at(1, 0, 0)

	tests := []struct {
		name      string
		runs      []state.RunRecord
		now       time.Time
		limits    RunLimits
		startedAt time.Time
		ok        bool
		reason    string
	}{
		{"last run error", []state.RunRecord{rec(state.OutcomeOK, at(17, 2, 0)), rec(state.OutcomeError, at(18, 2, 0))},
			at(18, 9, 0), RunLimits{}, longAgo, false, "last run error"},
		{"error before a run in progress", []state.RunRecord{rec(state.OutcomeError, at(17, 2, 0)), rec(state.OutcomeRunning, at(18, 2, 0))},
			at(18, 3, 0), RunLimits{}, longAgo, false, "last run error"},
		{"problems is healthy", []state.RunRecord{rec(state.OutcomeProblems, at(18, 2, 0))},
			at(18, 9, 0), RunLimits{}, longAgo, true, ""},
		{"missed 02:00 at 03:00", []state.RunRecord{rec(state.OutcomeOK, at(18, 2, 0))},
			at(19, 3, 0), RunLimits{}, longAgo, false, "scheduled run missed"},
		{"02:00 within 30m grace", []state.RunRecord{rec(state.OutcomeOK, at(18, 2, 0))},
			at(19, 2, 29), RunLimits{}, longAgo, true, ""},
		{"run started today", []state.RunRecord{rec(state.OutcomeOK, at(18, 2, 0)), rec(state.OutcomeOK, at(19, 2, 0))},
			at(19, 9, 0), RunLimits{}, longAgo, true, ""},
		{"unset: running 25h past the 24h limit", []state.RunRecord{rec(state.OutcomeRunning, at(18, 2, 0))},
			at(19, 3, 0), RunLimits{}, longAgo, false, "run overran"},
		{"unset: running 23h within the 24h limit", []state.RunRecord{rec(state.OutcomeRunning, at(18, 2, 0))},
			at(19, 1, 0), RunLimits{}, longAgo, true, ""},
		{"running past the limit", []state.RunRecord{rec(state.OutcomeRunning, at(19, 2, 0))},
			at(19, 6, 1), RunLimits{MaxRun: 3 * time.Hour, BootTimeout: 25 * time.Minute, ProbeTimeout: 25 * time.Minute}, longAgo, false, "run overran"},
		{"running within the limit spans a tick", []state.RunRecord{rec(state.OutcomeRunning, at(19, 2, 0))},
			at(20, 2, 45), RunLimits{MaxRun: 48 * time.Hour}, longAgo, true, ""},
		{"fresh daemon, no runs", nil, at(19, 1, 10), RunLimits{}, at(19, 1, 0), true, ""},
		{"daemon up, no runs, missed", nil, at(19, 3, 0), RunLimits{}, at(18, 12, 0), false, "scheduled run missed"},
		{"no start file, no runs", nil, at(19, 9, 0), RunLimits{}, time.Time{}, true, ""},
		{"no start file, old run", []state.RunRecord{rec(state.OutcomeOK, at(17, 2, 0))},
			at(19, 9, 0), RunLimits{}, time.Time{}, false, "scheduled run missed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Evaluate(state.State{Runs: tt.runs}, tt.now, sched, tt.limits, tt.startedAt, nil)
			if ok != tt.ok || reason != tt.reason {
				t.Errorf("Evaluate = %v %q, want %v %q", ok, reason, tt.ok, tt.reason)
			}
		})
	}
}

func TestOverrunLimit(t *testing.T) {
	const m = time.Minute
	for _, tt := range []struct {
		name                string
		maxRun, boot, probe time.Duration
		want                time.Duration
	}{
		{"unset", 0, 5 * m, 5 * m, 24 * time.Hour},
		{"negative", -time.Hour, 5 * m, 5 * m, 24 * time.Hour},
		{"set", 6 * time.Hour, 5 * m, 7 * m, 6*time.Hour + 22*m},
	} {
		if got := OverrunLimit(tt.maxRun, tt.boot, tt.probe); got != tt.want {
			t.Errorf("%s: OverrunLimit = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEvaluateRunSpanningTick(t *testing.T) {
	sched, err := cron.ParseStandard("0 */6 * * *")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, time.Local) }
	st := state.State{Runs: []state.RunRecord{{ID: "r", Outcome: state.OutcomeOK, Started: at(0, 0), Ended: at(7, 0)}}}
	if ok, reason := Evaluate(st, at(7, 30), sched, RunLimits{}, at(0, 0).Add(-time.Hour), nil); !ok {
		t.Errorf("07:30 after a run that skipped the 06:00 tick: %q", reason)
	}
	if ok, reason := Evaluate(st, at(12, 31), sched, RunLimits{}, time.Time{}, nil); ok || reason != ReasonMissed {
		t.Errorf("12:31 = %v %q, want missed", ok, reason)
	}
}

func TestEvaluateStoredOffsetUsesLocalSchedule(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC-5", -5*60*60)
	t.Cleanup(func() { time.Local = old })
	sched, err := cron.ParseStandard("0 2 * * *") // 07:00 UTC
	if err != nil {
		t.Fatal(err)
	}
	// Last run ended 02:30 local, stored (as after a JSON round trip) in UTC.
	ended := time.Date(2026, 10, 18, 7, 30, 0, 0, time.UTC)
	st := state.State{Runs: []state.RunRecord{{ID: "r", Outcome: state.OutcomeOK, Started: ended.Add(-30 * time.Minute), Ended: ended}}}
	// 03:00 UTC is 22:00 local: the next local 02:00 is still ahead.
	if ok, reason := Evaluate(st, time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC), sched, RunLimits{}, time.Time{}, nil); !ok {
		t.Errorf("unhealthy: %q", reason)
	}
	if ok, _ := Evaluate(st, time.Date(2026, 10, 19, 7, 29, 0, 0, time.UTC), sched, RunLimits{}, time.Time{}, nil); !ok {
		t.Error("unhealthy within grace of the local 02:00 run")
	}
	if ok, reason := Evaluate(st, time.Date(2026, 10, 19, 7, 31, 0, 0, time.UTC), sched, RunLimits{}, time.Time{}, nil); ok || reason != ReasonMissed {
		t.Errorf("after grace = %v %q, want missed", ok, reason)
	}
}

// The schedule is read in the daemon's zone, not the zone of the process
// running health: a service with TZ=America/New_York on a UTC host runs at
// 03:00 New York time, and health must not call that missed at 04:00 UTC
// (code review 6.6).
func TestEvaluateInTheDaemonsZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	sched, _ := cron.ParseStandard("0 3 * * *")
	last := time.Date(2026, 10, 20, 3, 0, 0, 0, ny)
	st := state.State{Runs: []state.RunRecord{{Started: last, Ended: last.Add(time.Hour), Outcome: state.OutcomeOK}}}
	now := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC) // midnight in New York, 03:00 still to come
	if ok, reason := Evaluate(st, now, sched, RunLimits{}, time.Time{}, ny); !ok {
		t.Errorf("in the daemon's zone: %s", reason)
	}
	utc := time.UTC
	if ok, _ := Evaluate(st, now, sched, RunLimits{}, time.Time{}, utc); ok {
		t.Error("the UTC reading should call it missed (the bug this guards against)")
	}
}

// A run past its overrun limit is not reported while a proof is restoring
// (the restore has no timeout of its own), up to the 24h hard cap (code
// review 6.12).
func TestOverrunWhileRestoring(t *testing.T) {
	sched, _ := cron.ParseStandard("0 2 * * *")
	start := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	limits := RunLimits{MaxRun: time.Hour, BootTimeout: 5 * time.Minute, ProbeTimeout: 5 * time.Minute}
	st := state.State{Runs: []state.RunRecord{{Started: start, Outcome: state.OutcomeRunning}}}
	late := start.Add(3 * time.Hour)
	if ok, reason := Evaluate(st, late, sched, limits, time.Time{}, time.UTC); ok || reason != ReasonOverran {
		t.Errorf("no restore in flight: %v %q, want overran", ok, reason)
	}
	st.InFlight = map[int]state.Job{990000: {TempVMID: 990000, Stage: state.StageRestored}}
	if ok, reason := Evaluate(st, late, sched, limits, time.Time{}, time.UTC); !ok {
		t.Errorf("restoring: %q, want ok", reason)
	}
	if ok, reason := Evaluate(st, start.Add(25*time.Hour), sched, limits, time.Time{}, time.UTC); ok || reason != ReasonOverran {
		t.Errorf("restoring past 24h: %v %q, want overran", ok, reason)
	}
}
