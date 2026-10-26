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

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
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
		st.InFlight[9001] = Job{TempVMID: 9001, Kind: model.KindLXC, VolID: "v", Storage: "s", ProtectedOrig: &orig, Stage: StageRestored}
		st.LastProven[model.GuestKey{Kind: model.KindLXC, VMID: 101}.String()] = now
		st.Runs = append(st.Runs, RunRecord{ID: "r1", Trigger: "cron", Outcome: OutcomeOK, Started: now, Ended: now, Counts: map[model.Verdict]int{model.Proven: 2}})
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
	if es, _ := os.ReadDir(filepath.Dir(p)); len(es) != 1 {
		t.Fatalf("temp file left behind: %v", es)
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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := s.Update(func(st *State) error { st.InFlight[1] = Job{TempVMID: 1}; return nil }); err != nil {
		t.Fatal(err)
	}
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
	if err := s.Update(func(st *State) error { st.InFlight[1] = Job{TempVMID: 1}; return nil }); err != nil {
		t.Fatal(err)
	}
	// Put a non-empty directory where the state file goes so the rename
	// fails (this holds for root too, which the Docker build runs as).
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "occupied"), 0o700); err != nil {
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
	err := s.Update(func(st *State) error {
		st.InFlight[1] = Job{ProtectedOrig: &orig}
		st.Runs = []RunRecord{{ID: "a", Counts: map[model.Verdict]int{model.Proven: 1}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := s.AppendRun(RunRecord{ID: "a", Outcome: OutcomeRunning, Started: t0}, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(RunRecord{ID: "b", Outcome: OutcomeOK}, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(RunRecord{ID: "a", Outcome: OutcomeProblems, Started: t0, Ended: t0.Add(time.Hour)}, 30); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(p)
	runs := s2.Snapshot().Runs
	if len(runs) != 2 || runs[0].ID != "a" || runs[0].Outcome != OutcomeProblems || runs[1].ID != "b" {
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

func TestOpenCreatesPrivateDir(t *testing.T) {
	p := tmpPath(t)
	if _, err := Open(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", fi.Mode().Perm())
	}
}

// A failed directory fsync after the rename is distinguishable, and memory
// adopts the new state so it matches the file.
func TestDirSyncFailureIsDurabilityError(t *testing.T) {
	p := tmpPath(t)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.syncDir = func(string) error { return errors.New("eio") }

	err = s.Update(func(st *State) error {
		st.LastProven["qemu/1"] = time.Unix(0, 0).UTC()
		return nil
	})
	if !errors.Is(err, ErrDurability) || !strings.Contains(err.Error(), "eio") {
		t.Fatalf("err = %v, want ErrDurability wrapping eio", err)
	}
	if _, ok := s.Snapshot().LastProven["qemu/1"]; !ok {
		t.Error("memory does not match the renamed file")
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "qemu/1") {
		t.Errorf("file not renamed into place: %q, %v", b, err)
	}
}

func TestConstantsMatchStoredValues(t *testing.T) {
	got := []string{StageProtected, StageRestored, StageStarted, StageDestroyed, OutcomeRunning, OutcomeOK, OutcomeProblems, OutcomeError}
	want := []string{"protected", "restored", "started", "destroyed", "running", "ok", "problems", "error"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("constants = %q, want %q (stored in state files; never change)", got, want)
	}
}

func TestSetStage(t *testing.T) {
	s, err := Open(tmpPath(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := false
	job := Job{TempVMID: 9001, Kind: model.KindQEMU, VolID: "v", Storage: "s", ProtectedOrig: &orig, Stage: StageProtected}
	if err := s.Update(func(st *State) error { st.InFlight[9001] = job; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStage(9001, StageRestored, true); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot().InFlight[9001]
	if got.Stage != StageRestored || got.VolID != "v" || got.ProtectedOrig == nil || *got.ProtectedOrig {
		t.Errorf("job = %+v", got)
	}
	// A missing entry is never created: an error if it must exist, else a
	// no-op.
	if err := s.SetStage(9002, StageDestroyed, true); err == nil {
		t.Error("missing entry with mustExist: want error")
	}
	if err := s.SetStage(9002, StageDestroyed, false); err != nil {
		t.Errorf("missing entry without mustExist: %v", err)
	}
	if _, ok := s.Snapshot().InFlight[9002]; ok {
		t.Error("SetStage created an entry")
	}
}
