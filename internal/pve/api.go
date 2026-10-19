package pve

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// API is the subset of the Proxmox VE REST API that PBP uses.
type API interface {
	Version(ctx context.Context) (string, error)
	ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error)
	SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error
	StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error)
	NodeMemory(ctx context.Context, node string) (NodeMemory, error)
	NetIface(ctx context.Context, node, iface string) (NetIface, error)
	WaitTask(ctx context.Context, node, upid string) error
}

var _ API = (*Client)(nil)
