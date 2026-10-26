package pve

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

type contentEntry struct {
	VolID     string   `json:"volid"`
	VMID      flexInt  `json:"vmid"`
	Subtype   string   `json:"subtype"`
	CTime     flexInt  `json:"ctime"`
	Size      flexInt  `json:"size"`
	Notes     string   `json:"notes"`
	Protected flexBool `json:"protected"`
	Encrypted flexBool `json:"encrypted"`
}

// ListBackups returns all guest (qemu and lxc) backup volumes on a storage.
// For PBS storages each Notes holds only the first line of the backup's
// notes (the PBS "comment"); VolumeNotes reads them in full.
func (c *Client) ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error) {
	var entries []contentEntry
	path := "/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(storage) + "/content"
	if err := c.do(ctx, http.MethodGet, path, url.Values{"content": {"backup"}}, nil, &entries); err != nil {
		return nil, err
	}
	out := make([]model.Backup, 0, len(entries))
	for _, e := range entries {
		// Host backups ("pbs-host") have no guest kind or VMID.
		if k := model.GuestKind(e.Subtype); (k != model.KindQEMU && k != model.KindLXC) || e.VMID <= 0 {
			continue
		}
		out = append(out, model.Backup{
			Storage:   storage,
			VolID:     e.VolID,
			Notes:     e.Notes,
			Key:       model.GuestKey{Kind: model.GuestKind(e.Subtype), VMID: int(e.VMID)},
			Time:      time.Unix(int64(e.CTime), 0).UTC(),
			Size:      int64(e.Size),
			Protected: bool(e.Protected),
			Encrypted: bool(e.Encrypted),
		})
	}
	return out, nil
}

// VolumeNotes returns a backup volume's full notes.
func (c *Client) VolumeNotes(ctx context.Context, node, storage, volid string) (string, error) {
	var r struct {
		Notes string `json:"notes"`
	}
	if err := c.do(ctx, http.MethodGet, volumePath(node, storage, volid), nil, nil, &r); err != nil {
		return "", err
	}
	return r.Notes, nil
}

// VolumeProtected reads a backup volume's protected flag now (a listing may
// be hours old).
func (c *Client) VolumeProtected(ctx context.Context, node, storage, volid string) (bool, error) {
	var r struct {
		Protected flexBool `json:"protected"`
	}
	if err := c.do(ctx, http.MethodGet, volumePath(node, storage, volid), nil, nil, &r); err != nil {
		return false, err
	}
	return bool(r.Protected), nil
}

func volumePath(node, storage, volid string) string {
	return "/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(storage) + "/content/" + url.PathEscape(volid)
}

// SetVolumeAttrs updates notes and/or protected on a backup volume; nil fields are left untouched.
func (c *Client) SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error {
	form := url.Values{}
	if notes != nil {
		form.Set("notes", *notes)
	}
	if protected != nil {
		if *protected {
			form.Set("protected", "1")
		} else {
			form.Set("protected", "0")
		}
	}
	return c.do(ctx, http.MethodPut, volumePath(node, storage, volid), nil, form, nil)
}
