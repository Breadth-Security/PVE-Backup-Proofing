// Package target restores backups as temporary guests on a PVE node and tears
// them down again. It enforces PBP's safety rules (design section 11): only
// VMIDs in the reserved range are ever touched, only guests tagged "pbp" are
// destroyed, and no guest is started until it is sanitized onto the isolated
// bridge.
package target

import (
	"context"
	"errors"
	"fmt"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
)

var (
	// ErrUnsafe: the restored guest cannot be made safe to start.
	ErrUnsafe = errors.New("guest cannot be sanitized safely")
	// ErrNoFreeVMID: every VMID in the reserved range is taken.
	ErrNoFreeVMID = errors.New("no free VMID in range")
	// ErrNotIsolated: the configured bridge has uplinks or addresses.
	ErrNotIsolated = errors.New("bridge is not isolated")
	// ErrForeignInRange: a guest PBP does not own sits in the reserved range.
	ErrForeignInRange = errors.New("guest not owned by PBP in VMID range")
	// ErrVersion: the node runs an unsupported PVE version.
	ErrVersion = errors.New("unsupported PVE version (need 9.0 or later)")
	// ErrOutOfRange: an operation targeted a VMID outside the reserved range.
	ErrOutOfRange = errors.New("VMID outside reserved range")
	// ErrNotOwned: the guest is not tagged "pbp" (or is not the expected kind).
	ErrNotOwned = errors.New("guest not owned by PBP")
	// ErrRestoreNotStarted: the node refused the restore request, so no
	// restore task ran and PBP created no guest at the VMID. Any other
	// Restore error may have left a (possibly untagged) guest behind.
	ErrRestoreNotStarted = errors.New("restore not started")
	// ErrBootFailed: the guest itself did not boot (never reached running,
	// or a container never accepted exec while SSH worked). Only this boot
	// error says something about the backup; any other is a node fault.
	ErrBootFailed = errors.New("guest did not boot")
)

// bootFailure is a boot failure that matches ErrBootFailed and keeps its own
// message.
type bootFailure struct{ msg string }

func (e bootFailure) Error() string        { return e.msg }
func (e bootFailure) Is(target error) bool { return target == ErrBootFailed }

func bootFailed(format string, a ...any) error { return bootFailure{fmt.Sprintf(format, a...)} }

// notStarted is a refused restore request: it matches ErrRestoreNotStarted
// and keeps the node's message unchanged (it becomes the notes reason).
type notStarted struct{ err error }

func (e notStarted) Error() string        { return e.err.Error() }
func (e notStarted) Unwrap() error        { return e.err }
func (e notStarted) Is(target error) bool { return target == ErrRestoreNotStarted }

// OwnerTag marks guests PBP created. A guest in the reserved range is PBP's
// if it carries this tag, or (design section 11.2) if it is the leftover of a
// restore that failed before it could be tagged: the caller's state store
// records its VMID as an in-flight restore, it is stopped, and its name
// (hostname for a container) matches the backup's original config or it is
// still locked by the restore ("lock: create"). Only Destroy applies the
// second rule.
const OwnerTag = "pbp"

// BootInfo describes a guest that reached running.
type BootInfo struct {
	OS probe.OSFamily
	// OSType is a container's PVE ostype ("" for a VM).
	OSType string
	// AgentMissing: a VM whose guest agent is not configured or never answered.
	AgentMissing bool
	// ExecBlocked: the agent answers but refuses to run commands.
	ExecBlocked bool
	Duration    time.Duration
}

// Target is where backups are restored and proofed (design section 5.1).
type Target interface {
	// CheckNode verifies the node's restricted `pbp node` helper before
	// anything else touches the node: it is reachable, runs a compatible
	// version, agrees with the daemon's VMID range and bridge, and refuses
	// an out-of-range exec. Every failure names README "Node setup".
	CheckNode(ctx context.Context) error
	// Prepare verifies the node: PVE version, isolated bridge and restore
	// storage.
	Prepare(ctx context.Context) error
	// CheckRangeClear refuses with ErrForeignInRange, naming the VMIDs, if
	// any guest in the range is not tagged "pbp". Run it after the janitor,
	// which removes the untagged leftovers of recorded restores.
	CheckRangeClear(ctx context.Context) error
	// Owned lists guests in the range tagged "pbp".
	Owned(ctx context.Context) ([]pve.GuestRef, error)
	// StrayTagged lists guests tagged "pbp" whose VMID is outside the
	// reserved range, on any node. PBP never touches them; the run reports
	// them. Read-only.
	StrayTagged(ctx context.Context) ([]pve.GuestRef, error)
	// AllocateVMID returns the lowest VMID in range that does not exist and
	// is not in inUse, or ErrNoFreeVMID.
	AllocateVMID(ctx context.Context, inUse map[int]bool) (int, error)
	// Restore restores b into vmid (not started) and tags it "pbp", keeping
	// origTags (the "tags" of the config stored in the backup). An error
	// matching ErrRestoreNotStarted means no guest was created; any other
	// error may have left one.
	Restore(ctx context.Context, b model.Backup, vmid int, origTags string) error
	// Sanitize makes the restored guest safe to start and verifies it. It
	// returns the pieces it removed, which the proof cannot cover.
	Sanitize(ctx context.Context, kind model.GuestKind, vmid int) ([]model.MissingPiece, error)
	// Start starts a sanitized guest; it refuses one that is not.
	Start(ctx context.Context, kind model.GuestKind, vmid int) error
	// WaitBooted waits until the guest runs and can be looked into. An
	// error matching ErrBootFailed means the guest did not boot; any other
	// error is a node, API or transport fault.
	WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (BootInfo, error)
	// Executor runs commands inside the guest.
	Executor(kind model.GuestKind, vmid int) probe.Executor
	// Destroy stops and removes the guest at vmid if PBP owns it, and
	// confirms it is gone. A guest that does not exist is not an error. It
	// must be in range, of kind and on this node, and tagged "pbp"; or, if
	// rec names the restore the caller's state store records at vmid, the
	// stopped, identity-matching leftover of that restore (see OwnerTag). The
	// zero Recorded allows tagged guests only.
	Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec Recorded) error
}

// Recorded is the restore a caller's state store records as started at a
// VMID. VolID alone decides whether there is a record: an empty VolID (with
// or without a Name) means none, so Destroy allows tagged guests only.
type Recorded struct {
	// VolID is the backup the restore was for.
	VolID string
	// Name is the guest's name (hostname for a container) in VolID's
	// original config, if the caller has it; empty makes Destroy read it.
	Name string
}
