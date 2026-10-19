package sanitize

import (
	"reflect"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

func TestMissingFromOriginalQEMU(t *testing.T) {
	got := MissingFromOriginal(model.KindQEMU, map[string]string{
		"scsi0": "local-zfs:vm-1-disk-0,size=32G",
		"scsi1": "local-zfs:vm-1-disk-1,backup=0,size=100G",
		"ide2":  "none,media=cdrom",
		"ide3":  "local:iso/x.iso,media=cdrom,backup=0",
	})
	want := []model.MissingPiece{{Kind: "excluded-disk", Key: "scsi1", Detail: "backup=0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestMissingFromOriginalLXC(t *testing.T) {
	got := MissingFromOriginal(model.KindLXC, map[string]string{
		"mp0": "local-zfs:subvol-1-disk-1,mp=/srv/pgdata",
		"mp1": "local-zfs:subvol-1-disk-2,mp=/ok,backup=1",
		"mp2": "/mnt/media,mp=/media",
	})
	want := []model.MissingPiece{{Kind: "excluded-mountpoint", Key: "mp0", Path: "/srv/pgdata", Detail: "backup not enabled"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestMissingFromOriginalNone(t *testing.T) {
	if got := MissingFromOriginal(model.KindLXC, map[string]string{"mp0": "v:s,mp=/a,backup=1"}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestMissingFromOriginalBackupSpellings(t *testing.T) {
	for v, excluded := range map[string]bool{
		"0": true, "no": true, "off": true, "false": true, "NO": true, "Off": true, "FALSE": true,
		"1": false, "yes": false, "on": false, "true": false, "Yes": false, "ON": false,
		"maybe": true, // unrecognised: reported as excluded
	} {
		qemu := MissingFromOriginal(model.KindQEMU, map[string]string{"scsi1": "s:vm-1-disk-1,backup=" + v})
		if got := len(qemu) == 1; got != excluded {
			t.Errorf("qemu backup=%s: excluded=%v, want %v", v, got, excluded)
		}
		lxc := MissingFromOriginal(model.KindLXC, map[string]string{"mp0": "s:subvol-1-disk-1,mp=/a,backup=" + v})
		if got := len(lxc) == 1; got != excluded {
			t.Errorf("lxc backup=%s: excluded=%v, want %v", v, got, excluded)
		}
	}
}

func TestGuestName(t *testing.T) {
	cfg := map[string]string{"name": "vm-name", "hostname": "ct-host"}
	if got := GuestName(model.KindQEMU, cfg); got != "vm-name" {
		t.Errorf("qemu: %q", got)
	}
	if got := GuestName(model.KindLXC, cfg); got != "ct-host" {
		t.Errorf("lxc: %q", got)
	}
	if got := GuestName(model.KindLXC, map[string]string{"name": "x"}); got != "" {
		t.Errorf("lxc without hostname: %q", got)
	}
}
