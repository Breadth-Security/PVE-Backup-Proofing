package sanitize

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// RestoreMounts returns the mount points to pass to a container's restore
// on storage, for a container whose backed-up config (orig) has bind
// mounts (issue #129). PVE lets only root@pam restore a bind mount, and
// refuses PBP's token; given rootfs and mpN explicitly, it uses exactly
// those instead and never reads the backup's, so the copy is restored
// without its bind mounts. Every volume mount point is passed (rootfs is
// mandatory then), as a new allocation of its size on storage, with its
// options except size and shared; each bind or device mount point is left
// out and returned in bind. vzdump never backs those up, so nothing the
// backup holds is lost.
//
// A container without bind mounts gets nil, nil, nil: it is restored as
// before, from the backup's own mount points.
func RestoreMounts(orig map[string]string, storage string) (mounts map[string]string, bind []model.MissingPiece, err error) {
	var volumeKeys []string
	for _, k := range mountKeys(orig) {
		v := orig[k]
		if hostPathMount(v) {
			path, _ := optValue(v, "mp")
			bind = append(bind, model.MissingPiece{Kind: "bind-mount", Key: k, Path: path})
			continue
		}
		volumeKeys = append(volumeKeys, k)
	}
	if len(bind) == 0 {
		return nil, nil, nil
	}
	if orig["rootfs"] == "" {
		return nil, nil, errors.New("the backed-up config has no rootfs")
	}
	mounts = map[string]string{}
	for _, k := range append([]string{"rootfs"}, volumeKeys...) {
		v, err := newAllocation(k, orig[k], storage)
		if err != nil {
			return nil, nil, err
		}
		mounts[k] = v
	}
	return mounts, bind, nil
}

// mountKeys returns orig's mpN keys in numeric order.
func mountKeys(orig map[string]string) []string {
	var keys []string
	for k := range orig {
		if reMP.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i][2:])
		b, _ := strconv.Atoi(keys[j][2:])
		return a < b
	})
	return keys
}

// hostPathMount reports whether a container mount point is a host path (a
// bind mount, or a device such as /dev/sdb1) rather than a storage volume.
func hostPathMount(v string) bool {
	vol, _ := splitMount(v)
	return strings.HasPrefix(vol, "/")
}

// splitMount splits a mount point value into its volume (the first field,
// or volume=) and its other options, in order.
func splitMount(v string) (vol string, opts []string) {
	for i, f := range strings.Split(v, ",") {
		if i == 0 && !strings.Contains(f, "=") {
			vol = f
			continue
		}
		if x, ok := strings.CutPrefix(f, "volume="); ok {
			vol = x
			continue
		}
		opts = append(opts, f)
	}
	return vol, opts
}

// newAllocation rewrites mount point k (value v) as a new volume of the
// same size on storage, keeping its options but size (read-only in PVE's
// schema) and shared (for host paths only).
func newAllocation(k, v, storage string) (string, error) {
	_, opts := splitMount(v)
	size, ok := optValue(v, "size")
	if !ok {
		return "", fmt.Errorf("%s has no size in the backed-up config", k)
	}
	gib, err := gibSize(size)
	if err != nil {
		return "", fmt.Errorf("%s: %w", k, err)
	}
	out := []string{storage + ":" + gib}
	for _, o := range opts {
		name, _, _ := strings.Cut(o, "=")
		if name == "size" || name == "shared" {
			continue
		}
		out = append(out, o)
	}
	return strings.Join(out, ","), nil
}

var reSize = regexp.MustCompile(`^(\d+(?:\.\d+)?)([KMGT]?)$`)

// gibSize converts a PVE disk size (bytes, or K/M/G/T, binary) to the GiB
// a new allocation takes (STORAGE:GiB): a plain decimal, rounded up to a
// whole MiB, since PVE allocates int(GiB*1024) MiB.
func gibSize(size string) (string, error) {
	m := reSize.FindStringSubmatch(size)
	if m == nil {
		return "", fmt.Errorf("size %q is not a PVE size", size)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return "", fmt.Errorf("size %q: %w", size, err)
	}
	exp := map[string]float64{"": 0, "K": 1, "M": 2, "G": 3, "T": 4}[m[2]]
	mib := math.Ceil(n * math.Pow(1024, exp) / (1 << 20))
	if mib < 1 {
		return "", fmt.Errorf("size %q is not a PVE size", size)
	}
	return strconv.FormatFloat(mib/1024, 'f', -1, 64), nil
}
