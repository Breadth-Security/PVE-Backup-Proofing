package sanitize

import (
	"reflect"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

func TestRestoreMountsNoBindMount(t *testing.T) {
	for _, cfg := range []map[string]string{
		{},
		{"rootfs": "local-zfs:subvol-1-disk-0,size=8G"},
		{"rootfs": "local-zfs:subvol-1-disk-0,size=8G", "mp0": "local-zfs:subvol-1-disk-1,mp=/data,backup=1,size=4G"},
	} {
		mounts, bind, err := RestoreMounts(cfg, "pool")
		if mounts != nil || bind != nil || err != nil {
			t.Errorf("%v: mounts %v, bind %v, err %v; want today's restore (all nil)", cfg, mounts, bind, err)
		}
	}
}

// A container with bind mounts (issue #129): rootfs and every volume mount
// point are passed as new allocations on the restore storage, with their
// options; bind mounts are left out and reported.
func TestRestoreMountsMix(t *testing.T) {
	cfg := map[string]string{
		"hostname": "jellyfin",
		"rootfs":   "local-zfs:subvol-129-disk-0,size=8G",
		"mp0":      "/nfs/music,mp=/nfs/music",
		"mp1":      "/nfs/media,mp=/nfs/media,ro=1",
		"mp2":      "local-zfs:subvol-129-disk-1,mp=/data,acl=1,backup=1,mountoptions=noatime;nodev,quota=1,replicate=0,ro=1,size=512M",
		"mp3":      "local-zfs:subvol-129-disk-2,mp=/scratch,size=8.5G",
		"mp4":      "volume=local-zfs:subvol-129-disk-3,mp=/vol,shared=1,size=1T",
	}
	mounts, bind, err := RestoreMounts(cfg, "pool_qemu")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"rootfs": "pool_qemu:8",
		"mp2":    "pool_qemu:0.5,mp=/data,acl=1,backup=1,mountoptions=noatime;nodev,quota=1,replicate=0,ro=1",
		"mp3":    "pool_qemu:8.5,mp=/scratch",
		"mp4":    "pool_qemu:1024,mp=/vol",
	}
	if !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts\n%v\nwant\n%v", mounts, want)
	}
	wantBind := []model.MissingPiece{
		{Kind: "bind-mount", Key: "mp0", Path: "/nfs/music"},
		{Kind: "bind-mount", Key: "mp1", Path: "/nfs/media"},
	}
	if !reflect.DeepEqual(bind, wantBind) {
		t.Errorf("bind %v, want %v", bind, wantBind)
	}
}

// A device mount point (a host block device) is a host path too: only
// root@pam may restore it, so it is left out like a bind mount.
func TestRestoreMountsDeviceAndBackupOff(t *testing.T) {
	cfg := map[string]string{
		"rootfs": "local-lvm:vm-5-disk-0,size=4G",
		"mp0":    "/dev/sdb1,mp=/mnt/disk",
		"mp1":    "local-lvm:vm-5-disk-1,mp=/cache,backup=0,size=2G",
	}
	mounts, bind, err := RestoreMounts(cfg, "s")
	if err != nil {
		t.Fatal(err)
	}
	// backup=0: still created (empty), so the copy keeps the layout.
	if want := map[string]string{"rootfs": "s:4", "mp1": "s:2,mp=/cache,backup=0"}; !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts %v, want %v", mounts, want)
	}
	if want := []model.MissingPiece{{Kind: "bind-mount", Key: "mp0", Path: "/mnt/disk"}}; !reflect.DeepEqual(bind, want) {
		t.Errorf("bind %v, want %v", bind, want)
	}
}

func TestRestoreMountsErrors(t *testing.T) {
	bindOnly := "/x,mp=/x"
	for _, tc := range []struct {
		name string
		cfg  map[string]string
		want string
	}{
		{"no rootfs", map[string]string{"mp0": bindOnly}, "no rootfs"},
		{"rootfs without size", map[string]string{"rootfs": "s:vm-1-disk-0", "mp0": bindOnly}, "rootfs has no size"},
		{"mount point without size", map[string]string{"rootfs": "s:d,size=1G", "mp0": bindOnly, "mp1": "s:d1,mp=/a"}, "mp1 has no size"},
		{"bad size", map[string]string{"rootfs": "s:d,size=1X", "mp0": bindOnly}, `rootfs: size "1X"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := RestoreMounts(tc.cfg, "s")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Sizes become GiB as a plain decimal, rounded up to a whole MiB: PVE
// allocates int(GiB*1024) MiB, so a volume is never smaller than the data
// it held (comment 1066 on issue #129).
func TestGiBSize(t *testing.T) {
	for in, want := range map[string]string{
		"8G": "8", "512M": "0.5", "8.5G": "8.5", "1T": "1024", "1.5T": "1536",
		"1048576": "0.0009765625", "1": "0.0009765625", "1025K": "0.001953125",
		"100M": "0.09765625", "0.1G": "0.1005859375",
		// size 0 is an unlimited subvolume (dir, ZFS, btrfs): PVE's own
		// restore passes STORAGE:0 for it, and so does PBP.
		"0": "0", "0T": "0", "0.0G": "0",
	} {
		got, err := gibSize(in)
		if err != nil || got != want {
			t.Errorf("gibSize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "1X", "-1G", "1e3", "1.G"} {
		if got, err := gibSize(bad); err == nil {
			t.Errorf("gibSize(%q) = %q, want an error", bad, got)
		}
	}
}

func TestRestoreMountsUnlimitedSubvol(t *testing.T) {
	mounts, _, err := RestoreMounts(map[string]string{"rootfs": "local:101/subvol-101-disk-0.subvol,size=0T", "mp0": "/x,mp=/x"}, "s")
	if err != nil || mounts["rootfs"] != "s:0" {
		t.Errorf("mounts %v, err %v; want rootfs s:0", mounts, err)
	}
}

func TestBindMounts(t *testing.T) {
	got := BindMounts(map[string]string{"mp10": "/b,mp=/b", "mp2": "volume=/a,mp=/a", "mp1": "s:v,mp=/v,size=1G", "rootfs": "s:r"})
	want := []model.MissingPiece{{Kind: "bind-mount", Key: "mp2", Path: "/a"}, {Kind: "bind-mount", Key: "mp10", Path: "/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
