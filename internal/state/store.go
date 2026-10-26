// Package state is the crash-safe JSON store for in-flight jobs, run history
// and last-proven times.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/fsutil"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// Job stages, in order. Each is recorded before the step it names.
const (
	// StageProtected: the backup is (being) protected; no guest exists yet.
	StageProtected = "protected"
	// StageRestored: a restore into TempVMID may have started, so a guest
	// there may be PBP's even before it carries the "pbp" tag.
	StageRestored = "restored"
	// StageStarted: the guest may have been started.
	StageStarted = "started"
	// StageDestroyed: the guest was confirmed gone.
	StageDestroyed = "destroyed"
)

// Run outcomes.
const (
	OutcomeRunning  = "running"
	OutcomeOK       = "ok"
	OutcomeProblems = "problems"
	OutcomeError    = "error"
)

// ErrDurability: the new state file was renamed into place (and the store
// holds the new state) but the directory could not be synced, so the rename
// may not survive a power loss.
var ErrDurability = errors.New("state: not durable")

// syncDir fsyncs a directory.
func syncDir(dir string) error {
	// #nosec G304 -- dir is the state file's own directory, not user input
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only directory handle
	return d.Sync()
}

// Job is a temporary guest that must be cleaned up if the run crashes.
// Stage is one of the Stage constants.
type Job struct {
	TempVMID      int
	Kind          model.GuestKind
	VolID         string
	Storage       string
	ProtectedOrig *bool
	Stage         string
}

// MayHaveGuest reports whether a restore PBP started may have created a guest
// at TempVMID, so that a guest there is PBP's even without the "pbp" tag
// (design section 11.2: ownership confirmed by state).
func (j Job) MayHaveGuest() bool {
	return j.Stage == StageRestored || j.Stage == StageStarted
}

// RunRecord summarises one run. Outcome is one of the Outcome constants.
type RunRecord struct {
	ID, Trigger, Outcome string
	Started, Ended       time.Time
	Counts               map[model.Verdict]int
}

// State is the persisted document. LastProven is keyed by GuestKey.String().
type State struct {
	InFlight   map[int]Job
	LastProven map[string]time.Time
	Runs       []RunRecord
}

// Store persists State atomically. Safe for concurrent use within a process.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
	// syncDir fsyncs the state file's directory; a field so tests can make
	// it fail.
	syncDir func(dir string) error
}

func newState() State {
	return State{InFlight: map[int]Job{}, LastProven: map[string]time.Time{}}
}

// Open loads the state at path, creating the parent directory if needed. A
// missing or empty file yields empty state; corrupt content is an error.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: create dir for %s: %w", path, err)
	}
	s := &Store{path: path, state: newState(), syncDir: syncDir}
	// #nosec G304 -- path is PBP's own state file under the fixed state directory, not user input
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	case len(b) == 0:
		return s, nil
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state: corrupt state file %s: %w", path, err)
	}
	if st.InFlight == nil {
		st.InFlight = map[int]Job{}
	}
	if st.LastProven == nil {
		st.LastProven = map[string]time.Time{}
	}
	s.state = st
	return s, nil
}

func (st State) clone() State {
	c := State{
		InFlight:   make(map[int]Job, len(st.InFlight)),
		LastProven: make(map[string]time.Time, len(st.LastProven)),
	}
	for k, j := range st.InFlight {
		if j.ProtectedOrig != nil {
			v := *j.ProtectedOrig
			j.ProtectedOrig = &v
		}
		c.InFlight[k] = j
	}
	for k, v := range st.LastProven {
		c.LastProven[k] = v
	}
	if st.Runs != nil {
		c.Runs = make([]RunRecord, len(st.Runs))
		for i, r := range st.Runs {
			if r.Counts != nil {
				m := make(map[model.Verdict]int, len(r.Counts))
				for k, v := range r.Counts {
					m[k] = v
				}
				r.Counts = m
			}
			c.Runs[i] = r
		}
	}
	return c
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.clone()
}

// Update runs fn on a copy of the state and persists it. If fn or the write
// fails, neither memory nor the file changes. Once the new file is in place
// memory holds the new state too, even if the directory sync then fails
// (ErrDurability).
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.state.clone()
	if err := fn(&c); err != nil {
		return err
	}
	if c.InFlight == nil {
		c.InFlight = map[int]Job{}
	}
	if c.LastProven == nil {
		c.LastProven = map[string]time.Time{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path, b, 0o600); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	s.state = c
	if err := s.syncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("%w: fsync dir: %w", ErrDurability, err)
	}
	return nil
}

// SetStage records stage for the InFlight job at vmid. It never creates an
// entry: a missing one is an error if mustExist, else nothing is written.
func (s *Store) SetStage(vmid int, stage string, mustExist bool) error {
	return s.Update(func(st *State) error {
		job, ok := st.InFlight[vmid]
		switch {
		case ok:
			job.Stage = stage
			st.InFlight[vmid] = job
		case mustExist:
			return fmt.Errorf("state: no in-flight job at %d", vmid)
		}
		return nil
	})
}

// AppendRun records r (replacing any record with the same ID in place) and
// keeps only the newest retain records.
func (s *Store) AppendRun(r RunRecord, retain int) error {
	return s.Update(func(st *State) error {
		replaced := false
		for i := range st.Runs {
			if st.Runs[i].ID == r.ID {
				st.Runs[i] = r
				replaced = true
				break
			}
		}
		if !replaced {
			st.Runs = append(st.Runs, r)
		}
		if retain >= 0 && len(st.Runs) > retain {
			st.Runs = append([]RunRecord(nil), st.Runs[len(st.Runs)-retain:]...)
		}
		return nil
	})
}
