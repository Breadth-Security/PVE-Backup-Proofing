package sanitize

import (
	"regexp"
	"sort"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

var reDisk = regexp.MustCompile(`^(scsi|virtio|sata|ide)\d+$`)

// MissingFromOriginal reports pieces the backup excludes, judged from the
// original (live) config: qemu disks with backup=0, and storage-backed LXC
// mount points without backup=1. Bind mounts are not reported here; Sanitize
// reports them via Removed, so the two sets stay disjoint.
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
			if b, ok := optBool(v, "backup"); ok && !b {
				out = append(out, model.MissingPiece{Kind: "excluded-disk", Key: k, Detail: "backup=0"})
			}
		case kind == model.KindLXC && reMP.MatchString(k):
			if strings.HasPrefix(firstField(v), "/") {
				continue
			}
			if b, _ := optBool(v, "backup"); !b {
				path, _ := optValue(v, "mp")
				out = append(out, model.MissingPiece{Kind: "excluded-mountpoint", Key: k, Path: path, Detail: "backup not enabled"})
			}
		}
	}
	return out
}
