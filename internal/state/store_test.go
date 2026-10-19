package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

func tmpPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "sub", "state.json")
}

func TestRoundTripThroughReopen(t *testing.T) {
	p := tmpPath(t)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	orig := true
	now := time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)
	err = s.Update(func(st *State) error {
		st.InFlight[9001] = Job{TempVMID: 9001, Kind: model.KindLXC, VolID: "v", Storage: "s", ProtectedOrig: &orig, Stage: "restored"}
		st.LastProven[model.GuestKey{Kind: model.KindLXC, VMID: 101}.String()] = now
		st.Runs = append(st.Runs, RunRecord{ID: "r1", Trigger: "cron", Outcome: "ok", Started: now, Ended: now, Counts: map[model.Verdict]int{model.Proven: 2}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Snapshot(), s2.Snapshot()) {
		t.Fatalf("mismatch:\n%+v\n%+v", s.Snapshot(), s2.Snapshot())
	}
	got := s2.Snapshot()
	if got.InFlight[9001].ProtectedOrig == nil || !*got.InFlight[9001].ProtectedOrig {
		t.Fatal("ProtectedOrig lost")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind")
	}
}

func TestOpenMissingGivesEmptyState(t *testing.T) {
	s, err := Open(tmpPath(t))
	if err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot()
	if st.InFlight == nil || st.LastProven == nil || len(st.Runs) != 0 {
		t.Fatalf("bad empty state %+v", st)
	}
}

func TestOpenEmptyFileIsEmptyState(t *testing.T) {
	p := tmpPath(t)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, nil, 0o600)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().InFlight) != 0 {
		t.Fatal("expected empty")
	}
}

func TestOpenCorruptFails(t *testing.T) {
	p := tmpPath(t)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("{not json"), 0o600)
	_, err := Open(p)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("want error naming path, got %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "{not json" {
		t.Fatal("corrupt file was modified")
	}
}

func TestUpdateErrorLeavesStateUnchanged(t *testing.T) {
	p := tmpPath(t)
	s, _ := Open(p)
	s.Update(func(st *State) error { st.InFlight[1] = Job{TempVMID: 1}; return nil })
	before, _ := os.ReadFile(p)
	boom := errors.New("boom")
	err := s.Update(func(st *State) error { st.InFlight[2] = Job{TempVMID: 2}; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("file changed")
	}
	if _, ok := s.Snapshot().InFlight[2]; ok {
		t.Fatal("memory changed")
	}
}

func TestUpdateWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	p := tmpPath(t)
	s, _ := Open(p)
	s.Update(func(st *State) error { st.InFlight[1] = Job{TempVMID: 1}; return nil })
	// Block the tmp path with a directory so the write fails.
	if err := os.Mkdir(p+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(st *State) error { st.InFlight[2] = Job{TempVMID: 2}; return nil })
	if err == nil {
		t.Fatal("expected write error")
	}
	if _, ok := s.Snapshot().InFlight[2]; ok {
		t.Fatal("memory changed despite failed write")
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	s, _ := Open(tmpPath(t))
	orig := true
	s.Update(func(st *State) error {
		st.InFlight[1] = Job{ProtectedOrig: &orig}
		st.Runs = []RunRecord{{ID: "a", Counts: map[model.Verdict]int{model.Proven: 1}}}
		return nil
	})
	snap := s.Snapshot()
	*snap.InFlight[1].ProtectedOrig = false
	snap.Runs[0].Counts[model.Proven] = 99
	snap.Runs[0].ID = "zzz"
	snap.InFlight[7] = Job{}
	snap.LastProven["x"] = time.Now()
	again := s.Snapshot()
	if !*again.InFlight[1].ProtectedOrig || again.Runs[0].Counts[model.Proven] != 1 || again.Runs[0].ID != "a" || len(again.InFlight) != 1 || len(again.LastProven) != 0 {
		t.Fatalf("snapshot aliased: %+v", again)
	}
}

func TestAppendRunRetainsLast30Of35(t *testing.T) {
	s, _ := Open(tmpPath(t))
	for i := 0; i < 35; i++ {
		if err := s.AppendRun(RunRecord{ID: fmt.Sprintf("r%02d", i)}, 30); err != nil {
			t.Fatal(err)
		}
	}
	runs := s.Snapshot().Runs
	if len(runs) != 30 || runs[0].ID != "r05" || runs[29].ID != "r34" {
		t.Fatalf("got %d, first %s last %s", len(runs), runs[0].ID, runs[len(runs)-1].ID)
	}
}

func TestAppendRunReplacesRunningRecord(t *testing.T) {
	p := tmpPath(t)
	s, _ := Open(p)
	t0 := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s.AppendRun(RunRecord{ID: "a", Outcome: "running", Started: t0}, 30)
	s.AppendRun(RunRecord{ID: "b", Outcome: "ok"}, 30)
	s.AppendRun(RunRecord{ID: "a", Outcome: "problems", Started: t0, Ended: t0.Add(time.Hour)}, 30)
	s2, _ := Open(p)
	runs := s2.Snapshot().Runs
	if len(runs) != 2 || runs[0].ID != "a" || runs[0].Outcome != "problems" || runs[1].ID != "b" {
		t.Fatalf("got %+v", runs)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s, _ := Open(tmpPath(t))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Update(func(st *State) error { st.InFlight[i] = Job{TempVMID: i}; return nil }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(s.Snapshot().InFlight); n != 20 {
		t.Fatalf("got %d", n)
	}
}
