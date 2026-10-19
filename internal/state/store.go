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

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// Job is a temporary guest that must be cleaned up if the run crashes.
// Stage is one of protected|restored|started|destroyed.
type Job struct {
	TempVMID      int
	Kind          model.GuestKind
	VolID         string
	Storage       string
	ProtectedOrig *bool
	Stage         string
}

// RunRecord summarises one run. Outcome is one of running|ok|problems|error.
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
	s := &Store{path: path, state: newState()}
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
// fails, neither memory nor the file changes.
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
	if err := s.write(c); err != nil {
		return err
	}
	s.state = c
	return nil
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

func (s *Store) write(st State) (err error) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("state: open %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp) // best-effort cleanup; err is already being returned
		}
	}()
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("state: write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("state: fsync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("state: rename to %s: %w", s.path, err)
	}
	d, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return fmt.Errorf("state: open dir: %w", err)
	}
	defer func() { _ = d.Close() }() // read-only directory handle
	if err = d.Sync(); err != nil {
		return fmt.Errorf("state: fsync dir: %w", err)
	}
	return nil
}
