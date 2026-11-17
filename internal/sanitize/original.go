package sanitize

import (
	"regexp"
	"sort"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

var reDisk = regexp.MustCompile(`^(scsi|virtio|sata|ide)\d+$`)

// MissingFromOriginal reports pieces the backup excludes, judged from the
// original (live) config: qemu disks with backup=0, storage-backed LXC
// mount points without backup=1, LXC bind (or device) mount points,
// which the copy is restored without (RestoreMounts, issue #129), and the
// devices of LXC raw lines PVE drops on restore when all are device-only
// (issue #141). Sanitize
// would report a bind mount still in the restored config too; the run
// drops such duplicates.
//
// An unrecognised backup= value counts as excluded for both kinds: reporting
// a piece that was in fact backed up is safer than claiming it was.
func MissingFromOriginal(kind model.GuestKind, original map[string]string) []model.MissingPiece {
	keys := make([]string, 0, len(original))
	for k := range original {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []model.MissingPiece
	for _, k := range keys {
		v := original[k]
		switch {
		case kind == model.KindQEMU && reDisk.MatchString(k):
			if m, _ := optValue(v, "media"); m == "cdrom" {
				continue
			}
			if !backupEnabled(v, true) {
				out = append(out, model.MissingPiece{Kind: "excluded-disk", Key: k, Detail: "backup=0"})
			}
		case kind == model.KindLXC && k == "lxc":
			// PVE drops raw lines when PBP's token restores; device-only
			// ones are proofed without their devices (issue #141).
			out = append(out, droppedDevices(original)...)
		case kind == model.KindLXC && reMP.MatchString(k):
			if hostPathMount(v) {
				// PBP restores the copy without it (RestoreMounts, issue
				// #129), so the restored config never shows it.
				path, _ := optValue(v, "mp")
				out = append(out, model.MissingPiece{Kind: "bind-mount", Key: k, Path: path})
				continue
			}
			if !backupEnabled(v, false) {
				path, _ := optValue(v, "mp")
				out = append(out, model.MissingPiece{Kind: "excluded-mountpoint", Key: k, Path: path, Detail: "backup not enabled"})
			}
		}
	}
	return out
}

// GuestName is a guest's name in its config: "name" for a VM, "hostname"
// for a container.
func GuestName(kind model.GuestKind, cfg map[string]string) string {
	if kind == model.KindLXC {
		return cfg["hostname"]
	}
	return cfg["name"]
}
