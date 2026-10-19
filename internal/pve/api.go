package pve

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// API is the subset of the Proxmox VE REST API that PBP uses.
type API interface {
	Version(ctx context.Context) (string, error)
	ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error)
	VolumeNotes(ctx context.Context, node, storage, volid string) (string, error)
	SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error
	StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error)
	NodeMemory(ctx context.Context, node string) (NodeMemory, error)
	NetIface(ctx context.Context, node, iface string) (NetIface, error)
	WaitTask(ctx context.Context, node, upid string) error

	Guests(ctx context.Context) ([]GuestRef, error)
	Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (upid string, err error)
	GuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int) (map[string]string, error)
	SetGuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error
	OriginalConfig(ctx context.Context, node, volid string) (string, error)
	Start(ctx context.Context, node string, kind model.GuestKind, vmid int) (upid string, err error)
	Shutdown(ctx context.Context, node string, kind model.GuestKind, vmid int) (upid string, err error)
	Stop(ctx context.Context, node string, kind model.GuestKind, vmid int) (upid string, err error)
	Destroy(ctx context.Context, node string, kind model.GuestKind, vmid int) (upid string, err error)
	GuestStatus(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error)
	AgentPing(ctx context.Context, node string, vmid int) error
	AgentOSInfo(ctx context.Context, node string, vmid int) (OSInfo, error)
	AgentExec(ctx context.Context, node string, vmid int, argv []string) (pid int, err error)
	AgentExecStatus(ctx context.Context, node string, vmid, pid int) (AgentExecStatus, error)
}

var _ API = (*Client)(nil)
