// Package targetfake is a scripted, in-memory target.Target for runner tests.
package targetfake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

var _ target.Target = (*Fake)(nil)

// Fake implements target.Target. Outcomes are scripted per VMID (the
// temporary VMID PBP allocated, not the original guest's). Nil maps and
// missing keys mean success. Seed fields before use and read them after the
// code under test has finished; methods are safe for concurrent use.
//
// Like the real target it enforces the safety contracts: every method that
// names a VMID refuses one outside Range with target.ErrOutOfRange, Start
// refuses a guest without a successful Sanitize, and Destroy refuses a guest
// left untagged by a failed restore (DestroyRecorded removes it, given the
// volid that restore was for). Restore refusals that create no guest (out
// of range, VMID taken) match target.ErrRestoreNotStarted.
type Fake struct {
	mu sync.Mutex

	// Range is the reserved VMID range AllocateVMID allocates from.
	Range [2]int

	// PrepareErr is returned by Prepare.
	PrepareErr error
	// Recorded is the recorded-restore set the last Prepare call was given.
	Recorded map[int]target.Recorded
	// OwnedGuests are leftover guests returned by Owned (e.g. from a crash).
	// Their VMIDs count as existing for AllocateVMID; Destroy removes them.
	OwnedGuests []pve.GuestRef
	// OwnedErr is returned by Owned.
	OwnedErr error

	// StrayGuests are pbp-tagged guests outside the range, returned by
	// StrayTagged.
	StrayGuests []pve.GuestRef
	// StrayErr is returned by StrayTagged.
	StrayErr error

	// RestoreErr fails Restore for a VMID. A failed restore creates no guest
	// unless RestoreLeaves is set for the VMID.
	RestoreErr map[int]error
	// RestoreLeaves makes a failed Restore leave an untagged guest behind,
	// as a failed restore task (or tag write) can.
	RestoreLeaves map[int]bool
	// SanitizeErr fails Sanitize for a VMID.
	SanitizeErr map[int]error
	// Missing is what a successful Sanitize returns for a VMID.
	Missing map[int][]model.MissingPiece
	// StartErr fails Start for a VMID.
	StartErr map[int]error
	// Boot is what WaitBooted returns for a VMID (zero BootInfo if absent).
	Boot map[int]target.BootInfo
	// BootErr fails WaitBooted for a VMID.
	BootErr map[int]error
	// Exec is the executor returned for a VMID; absent → every Exec fails.
	Exec map[int]probe.Executor
	// DestroyErr fails Destroy for a VMID; the guest is then kept.
	DestroyErr map[int]error

	// Live holds guests restored and not yet destroyed, keyed by VMID.
	// Tests read it to check cleanup.
	Live map[int]model.GuestKind
	// Untagged maps Live guests a failed restore left without the tag to
	// the volid that restore was for.
	Untagged map[int]string
	// sanitized marks guests whose last Sanitize succeeded.
	sanitized map[int]bool
	// Calls records each call as "Method arg...", e.g. "Restore 990000 <volid>".
	Calls []string
}

// New returns a Fake allocating from [lo, hi].
func New(lo, hi int) *Fake {
	return &Fake{Range: [2]int{lo, hi}, Live: map[int]model.GuestKind{}}
}

// guard refuses a VMID outside Range. Callers hold f.mu.
func (f *Fake) guard(vmid int) error {
	if vmid < f.Range[0] || vmid > f.Range[1] {
		return fmt.Errorf("%w: %d not in [%d, %d]", target.ErrOutOfRange, vmid, f.Range[0], f.Range[1])
	}
	return nil
}

func (f *Fake) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

// CallLog returns a copy of Calls.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Calls)
}

func (f *Fake) Prepare(ctx context.Context, recorded map[int]target.Recorded) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Prepare")
	f.Recorded = maps.Clone(recorded)
	return f.PrepareErr
}

func (f *Fake) Owned(ctx context.Context) ([]pve.GuestRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Owned")
	if f.OwnedErr != nil {
		return nil, f.OwnedErr
	}
	return slices.Clone(f.OwnedGuests), nil
}

func (f *Fake) StrayTagged(ctx context.Context) ([]pve.GuestRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("StrayTagged")
	if f.StrayErr != nil {
		return nil, f.StrayErr
	}
	return slices.Clone(f.StrayGuests), nil
}

// exists reports whether vmid is live or a not-yet-destroyed owned guest.
// Callers hold f.mu.
func (f *Fake) exists(vmid int) bool {
	if _, ok := f.Live[vmid]; ok {
		return true
	}
	return slices.ContainsFunc(f.OwnedGuests, func(g pve.GuestRef) bool { return g.Key.VMID == vmid })
}

func (f *Fake) AllocateVMID(ctx context.Context, inUse map[int]bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AllocateVMID")
	for v := f.Range[0]; v <= f.Range[1]; v++ {
		if !f.exists(v) && !inUse[v] {
			return v, nil
		}
	}
	return 0, target.ErrNoFreeVMID
}

func (f *Fake) Restore(ctx context.Context, b model.Backup, vmid int, orig map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Restore %d %s", vmid, b.VolID)
	if err := f.guard(vmid); err != nil {
		return fmt.Errorf("%w: %w", target.ErrRestoreNotStarted, err)
	}
	if f.exists(vmid) {
		return fmt.Errorf("%w: vmid %d already exists", target.ErrRestoreNotStarted, vmid)
	}
	err := f.RestoreErr[vmid]
	if err != nil && !f.RestoreLeaves[vmid] {
		return err
	}
	if f.Live == nil {
		f.Live = map[int]model.GuestKind{}
	}
	f.Live[vmid] = b.Key.Kind
	if err != nil {
		if f.Untagged == nil {
			f.Untagged = map[int]string{}
		}
		f.Untagged[vmid] = b.VolID
	}
	return err
}

func (f *Fake) Sanitize(ctx context.Context, kind model.GuestKind, vmid int, orig map[string]string) ([]model.MissingPiece, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Sanitize %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return nil, err
	}
	delete(f.sanitized, vmid)
	if err := f.SanitizeErr[vmid]; err != nil {
		return nil, err
	}
	if _, ok := f.Live[vmid]; !ok {
		return nil, fmt.Errorf("targetfake: sanitize %d: no restored guest", vmid)
	}
	if f.sanitized == nil {
		f.sanitized = map[int]bool{}
	}
	f.sanitized[vmid] = true
	return slices.Clone(f.Missing[vmid]), nil
}

func (f *Fake) Start(ctx context.Context, kind model.GuestKind, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Start %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return err
	}
	if !f.sanitized[vmid] {
		return fmt.Errorf("targetfake: refusing to start %d: not sanitized", vmid)
	}
	return f.StartErr[vmid]
}

func (f *Fake) WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (target.BootInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WaitBooted %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return target.BootInfo{}, err
	}
	if err := f.BootErr[vmid]; err != nil {
		return target.BootInfo{}, err
	}
	return f.Boot[vmid], nil
}

func (f *Fake) Executor(kind model.GuestKind, vmid int) probe.Executor {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Executor %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return refused{err}
	}
	if e, ok := f.Exec[vmid]; ok {
		return e
	}
	return noExec{vmid}
}

type noExec struct{ vmid int }

func (e noExec) Exec(context.Context, []string) (probe.ExecResult, error) {
	return probe.ExecResult{}, fmt.Errorf("targetfake: no executor scripted for %d", e.vmid)
}

type refused struct{ err error }

func (r refused) Exec(context.Context, []string) (probe.ExecResult, error) {
	return probe.ExecResult{}, r.err
}

func (f *Fake) Destroy(ctx context.Context, kind model.GuestKind, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Destroy %s %d", kind, vmid)
	return f.destroy(vmid, "")
}

func (f *Fake) DestroyRecorded(ctx context.Context, kind model.GuestKind, vmid int, volid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DestroyRecorded %s %d %s", kind, vmid, volid)
	return f.destroy(vmid, volid)
}

// destroy removes vmid; an untagged guest only when volid is the one its
// failed restore was for. Callers hold f.mu.
func (f *Fake) destroy(vmid int, volid string) error {
	if err := f.guard(vmid); err != nil {
		return err
	}
	if v, untagged := f.Untagged[vmid]; untagged && (volid == "" || volid != v) {
		return fmt.Errorf("%w: %d has no %q tag and is not the leftover of %q", target.ErrNotOwned, vmid, target.OwnerTag, volid)
	}
	if err := f.DestroyErr[vmid]; err != nil {
		return err
	}
	delete(f.Live, vmid)
	delete(f.Untagged, vmid)
	delete(f.sanitized, vmid)
	f.OwnedGuests = slices.DeleteFunc(f.OwnedGuests, func(g pve.GuestRef) bool { return g.Key.VMID == vmid })
	return nil
}
