package health

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
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
		if outcome != "running" {
			r.Ended = started.Add(time.Hour)
		}
		return r
	}
	longAgo := at(1, 0, 0)

	tests := []struct {
		name      string
		runs      []state.RunRecord
		now       time.Time
		maxRun    time.Duration
		startedAt time.Time
		ok        bool
		reason    string
	}{
		{"last run error", []state.RunRecord{rec("ok", at(17, 2, 0)), rec("error", at(18, 2, 0))},
			at(18, 9, 0), 0, longAgo, false, "last run error"},
		{"error before a run in progress", []state.RunRecord{rec("error", at(17, 2, 0)), rec("running", at(18, 2, 0))},
			at(18, 3, 0), 0, longAgo, false, "last run error"},
		{"problems is healthy", []state.RunRecord{rec("problems", at(18, 2, 0))},
			at(18, 9, 0), 0, longAgo, true, ""},
		{"missed 02:00 at 03:00", []state.RunRecord{rec("ok", at(18, 2, 0))},
			at(19, 3, 0), 0, longAgo, false, "scheduled run missed"},
		{"02:00 within 30m grace", []state.RunRecord{rec("ok", at(18, 2, 0))},
			at(19, 2, 29), 0, longAgo, true, ""},
		{"run started today", []state.RunRecord{rec("ok", at(18, 2, 0)), rec("ok", at(19, 2, 0))},
			at(19, 9, 0), 0, longAgo, true, ""},
		{"running 25h with maxRun 0", []state.RunRecord{rec("running", at(18, 2, 0))},
			at(19, 3, 0), 0, longAgo, false, "run overran"},
		{"running 23h with maxRun 0", []state.RunRecord{rec("running", at(18, 2, 0))},
			at(19, 1, 0), 0, longAgo, true, ""},
		{"running past maxRun", []state.RunRecord{rec("running", at(19, 2, 0))},
			at(19, 6, 1), 4 * time.Hour, longAgo, false, "run overran"},
		{"running within maxRun spans a tick", []state.RunRecord{rec("running", at(19, 2, 0))},
			at(20, 2, 45), 48 * time.Hour, longAgo, true, ""},
		{"fresh daemon, no runs", nil, at(19, 1, 10), 0, at(19, 1, 0), true, ""},
		{"daemon up, no runs, missed", nil, at(19, 3, 0), 0, at(18, 12, 0), false, "scheduled run missed"},
		{"no start file, no runs", nil, at(19, 9, 0), 0, time.Time{}, true, ""},
		{"no start file, old run", []state.RunRecord{rec("ok", at(17, 2, 0))},
			at(19, 9, 0), 0, time.Time{}, false, "scheduled run missed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Evaluate(state.State{Runs: tt.runs}, tt.now, sched, tt.maxRun, tt.startedAt)
			if ok != tt.ok || reason != tt.reason {
				t.Errorf("Evaluate = %v %q, want %v %q", ok, reason, tt.ok, tt.reason)
			}
		})
	}
}
