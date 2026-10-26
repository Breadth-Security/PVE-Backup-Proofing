// Package targetfake is a scripted, in-memory target.Target for runner tests.
package targetfake

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/target"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/testutil"
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
// left untagged by a failed restore unless given the Recorded volid that
// restore was for. CheckRangeClear refuses while such a guest exists.
// Restore refusals that create no guest (out of range, VMID taken) match
// target.ErrRestoreNotStarted.
type Fake struct {
	mu sync.Mutex

	// Range is the reserved VMID range AllocateVMID allocates from.
	Range [2]int

	// NodeErr is returned by CheckNode.
	NodeErr error
	// PrepareErr is returned by Prepare.
	PrepareErr error
	// RangeErr is returned by CheckRangeClear (before its own check).
	RangeErr error
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
	Live map[int]Guest
	// calls records each call as "Method arg...", e.g. "Restore 990000 <volid>"
	// or "Destroy qemu 990000 [<recorded volid>]".
	calls testutil.CallLog
}

// Guest is a restored guest the fake holds.
type Guest struct {
	Kind model.GuestKind
	// UntaggedFor is the volid of the failed restore that left the guest
	// without the "pbp" tag ("" for a tagged guest).
	UntaggedFor string
	// Sanitized: the guest's last Sanitize succeeded.
	Sanitized bool
}

// New returns a Fake allocating from [lo, hi].
func New(lo, hi int) *Fake {
	return &Fake{Range: [2]int{lo, hi}, Live: map[int]Guest{}}
}

// guard refuses a VMID outside Range. Callers hold f.mu.
func (f *Fake) guard(vmid int) error {
	if vmid < f.Range[0] || vmid > f.Range[1] {
		return fmt.Errorf("%w: %d not in [%d, %d]", target.ErrOutOfRange, vmid, f.Range[0], f.Range[1])
	}
	return nil
}

func (f *Fake) record(format string, args ...any) {
	f.calls.Add(fmt.Sprintf(format, args...))
}

// CallLog returns the calls recorded so far.
func (f *Fake) CallLog() []string { return f.calls.Snapshot() }

func (f *Fake) CheckNode(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CheckNode")
	return f.NodeErr
}

func (f *Fake) Prepare(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Prepare")
	return f.PrepareErr
}

func (f *Fake) CheckRangeClear(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CheckRangeClear")
	if f.RangeErr != nil {
		return f.RangeErr
	}
	var foreign []int
	for v, g := range f.Live {
		if g.UntaggedFor != "" {
			foreign = append(foreign, v)
		}
	}
	if len(foreign) > 0 {
		slices.Sort(foreign)
		return fmt.Errorf("%w: %v", target.ErrForeignInRange, foreign)
	}
	return nil
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

func (f *Fake) Restore(ctx context.Context, b model.Backup, vmid int, origTags string) error {
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
		f.Live = map[int]Guest{}
	}
	g := Guest{Kind: b.Key.Kind}
	if err != nil {
		g.UntaggedFor = b.VolID
	}
	f.Live[vmid] = g
	return err
}

func (f *Fake) Sanitize(ctx context.Context, kind model.GuestKind, vmid int) (target.Sanitized, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Sanitize %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return target.Sanitized{}, err
	}
	g, ok := f.Live[vmid]
	if ok {
		g.Sanitized = false
		f.Live[vmid] = g
	}
	if err := f.SanitizeErr[vmid]; err != nil {
		return target.Sanitized{}, err
	}
	if !ok {
		return target.Sanitized{}, fmt.Errorf("targetfake: sanitize %d: no restored guest", vmid)
	}
	g.Sanitized = true
	f.Live[vmid] = g
	return target.Sanitized{Removed: slices.Clone(f.Missing[vmid])}, nil
}

func (f *Fake) Start(ctx context.Context, kind model.GuestKind, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Start %s %d", kind, vmid)
	if err := f.guard(vmid); err != nil {
		return err
	}
	if !f.Live[vmid].Sanitized {
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

func (f *Fake) Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec target.Recorded) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.VolID == "" {
		f.record("Destroy %s %d", kind, vmid)
	} else {
		f.record("Destroy %s %d %s", kind, vmid, rec.VolID)
	}
	if err := f.guard(vmid); err != nil {
		return err
	}
	if v := f.Live[vmid].UntaggedFor; v != "" && rec.VolID != v {
		return fmt.Errorf("%w: %d has no %q tag and is not the leftover of %q", target.ErrNotOwned, vmid, target.OwnerTag, rec.VolID)
	}
	if err := f.DestroyErr[vmid]; err != nil {
		return err
	}
	delete(f.Live, vmid)
	f.OwnedGuests = slices.DeleteFunc(f.OwnedGuests, func(g pve.GuestRef) bool { return g.Key.VMID == vmid })
	return nil
}
