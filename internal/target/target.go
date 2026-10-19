// Package target restores backups as temporary guests on a PVE node and tears
// them down again. It enforces PBP's safety rules (design section 11): only
// VMIDs in the reserved range are ever touched, only guests tagged "pbp" are
// destroyed, and no guest is started until it is sanitized onto the isolated
// bridge.
package target

import (
	"context"
	"errors"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
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
)

// OwnerTag marks guests PBP created.
const OwnerTag = "pbp"

// BootInfo describes a guest that reached running.
type BootInfo struct {
	OS probe.OSFamily
	// AgentMissing: a VM whose guest agent is not configured or never answered.
	AgentMissing bool
	// ExecBlocked: the agent answers but refuses to run commands.
	ExecBlocked bool
	Duration    time.Duration
}

// Target is where backups are restored and proofed (design section 5.1).
type Target interface {
	// Prepare verifies the node: PVE version, isolated bridge, restore
	// storage, and that no foreign guest sits in the VMID range.
	Prepare(ctx context.Context) error
	// Owned lists guests in the range tagged "pbp".
	Owned(ctx context.Context) ([]pve.GuestRef, error)
	// AllocateVMID returns the lowest VMID in range that does not exist and
	// is not in inUse, or ErrNoFreeVMID.
	AllocateVMID(ctx context.Context, inUse map[int]bool) (int, error)
	// Restore restores b into vmid (not started) and tags it "pbp".
	Restore(ctx context.Context, b model.Backup, vmid int) error
	// Sanitize makes the restored guest safe to start and verifies it. It
	// returns the pieces the proof cannot cover.
	Sanitize(ctx context.Context, kind model.GuestKind, vmid int, volid string) ([]model.MissingPiece, error)
	// Start starts a sanitized guest; it refuses one that is not.
	Start(ctx context.Context, kind model.GuestKind, vmid int) error
	// WaitBooted waits until the guest runs and can be looked into.
	WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (BootInfo, error)
	// Executor runs commands inside the guest.
	Executor(kind model.GuestKind, vmid int) probe.Executor
	// Destroy stops and removes an owned guest and confirms it is gone. A
	// guest that does not exist is not an error.
	Destroy(ctx context.Context, kind model.GuestKind, vmid int) error
}
