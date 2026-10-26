package probe

import (
	"context"
	"path"
	"regexp"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// unitProps is the subset of `systemctl show` properties tracing needs.
type unitProps struct {
	Type, What, Where string
	RequiresMountsFor []string
	Requires, BindsTo []string
	ok                bool // properties were read successfully
	isMount           bool
}

var (
	networkFSTypes = map[string]bool{"nfs": true, "nfs4": true, "cifs": true, "smb3": true}
	networkSource  = regexp.MustCompile(`^([^/]+:/|//)`)
	sourcePrefixes = []struct{ tag, dir string }{
		{"UUID=", "/dev/disk/by-uuid/"},
		{"LABEL=", "/dev/disk/by-label/"},
		{"PARTUUID=", "/dev/disk/by-partuuid/"},
	}
)

// traceMissing splits failed units into those explained by a piece missing
// from the backup (covered) and those that are not (untraced). It is
// conservative: a unit whose properties cannot be read, or whose failure
// cannot be tied to a missing piece, stays untraced. Both results keep the
// order of failed.
func traceMissing(ctx context.Context, ex Executor, failed []string, missing []model.MissingPiece) (covered []model.NotCovered, untraced []string) {
	props := make(map[string]unitProps, len(failed))
	for _, u := range failed {
		props[u] = showUnit(ctx, ex, u)
	}

	pieceOf := map[string]model.MissingPiece{} // covered unit -> piece
	var mountWheres []string                   // Where of covered mount units, parallel to mountPieces
	var mountPieces []model.MissingPiece

	for _, u := range failed {
		p := props[u]
		if !p.ok || !p.isMount {
			continue
		}
		if piece, ok := traceMount(ctx, ex, p, missing); ok {
			pieceOf[u] = piece
			mountWheres = append(mountWheres, p.Where)
			mountPieces = append(mountPieces, piece)
		}
	}

	for changed := true; changed; {
		changed = false
		for _, u := range failed {
			p := props[u]
			if _, done := pieceOf[u]; done || !p.ok {
				continue
			}
			if piece, ok := traceDependent(p, pieceOf, mountWheres, mountPieces, missing); ok {
				pieceOf[u] = piece
				if p.isMount && p.Where != "" {
					mountWheres = append(mountWheres, p.Where)
					mountPieces = append(mountPieces, piece)
				}
				changed = true
			}
		}
	}

	for _, u := range failed {
		if piece, ok := pieceOf[u]; ok {
			covered = append(covered, model.NotCovered{Unit: u, Piece: piece})
		} else {
			untraced = append(untraced, u)
		}
	}
	return covered, untraced
}

// showUnit reads the unit's properties; ok is false on any exec error or
// non-zero exit.
func showUnit(ctx context.Context, ex Executor, unit string) unitProps {
	res, err := ex.Exec(ctx, []string{"systemctl", "show",
		"-p", "Type", "-p", "What", "-p", "Where", "-p", "RequiresMountsFor",
		"-p", "Requires", "-p", "BindsTo", "--", unit})
	if err != nil || res.ExitCode != 0 {
		return unitProps{}
	}
	p := unitProps{ok: true, isMount: strings.HasSuffix(unit, ".mount")}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !found {
			continue
		}
		switch k {
		case "Type":
			p.Type = strings.TrimSpace(v)
		case "What":
			p.What = strings.TrimSpace(v)
		case "Where":
			p.Where = strings.TrimSpace(v)
		case "RequiresMountsFor":
			p.RequiresMountsFor = strings.Fields(v)
		case "Requires":
			p.Requires = strings.Fields(v)
		case "BindsTo":
			p.BindsTo = strings.Fields(v)
		}
	}
	return p
}

// traceMount decides whether a failed mount unit is explained by a missing piece.
func traceMount(ctx context.Context, ex Executor, p unitProps, missing []model.MissingPiece) (model.MissingPiece, bool) {
	if p.Where == "" {
		return model.MissingPiece{}, false
	}
	if piece, ok := pieceForPath(p.Where, missing); ok {
		return piece, true
	}
	if networkFSTypes[p.Type] || networkSource.MatchString(p.What) {
		return model.MissingPiece{Kind: "network-storage", Path: p.Where, Detail: p.What}, true
	}
	var disks []model.MissingPiece
	for _, m := range missing {
		// EFI and TPM state hold no filesystem: they cannot explain a mount
		// (orphan-disk review).
		if m.Kind == "excluded-disk" && !strings.HasPrefix(m.Key, "efidisk") && !strings.HasPrefix(m.Key, "tpmstate") {
			disks = append(disks, m)
		}
	}
	if len(disks) == 0 {
		return model.MissingPiece{}, false
	}
	dev := resolveSource(p.What)
	if dev == "" {
		return model.MissingPiece{}, false
	}
	// `test -e` exits 1 when the path is absent; any other outcome (present,
	// usage error, command missing, exec failure) is not proof of absence.
	res, err := ex.Exec(ctx, []string{"test", "-e", dev})
	if err != nil || res.ExitCode != 1 {
		return model.MissingPiece{}, false
	}
	piece := model.MissingPiece{Kind: "excluded-disk", Path: p.Where, Detail: disks[0].Detail}
	keys := make([]string, len(disks))
	for i, d := range disks {
		keys[i] = d.Key
		if d.Detail != piece.Detail {
			piece.Detail = ""
		}
	}
	piece.Key = strings.Join(keys, "/")
	return piece, true
}

// traceDependent decides whether a failed unit is explained by a covered
// mount or piece path it requires, or a covered unit it Requires/BindsTo.
func traceDependent(p unitProps, pieceOf map[string]model.MissingPiece, mountWheres []string, mountPieces []model.MissingPiece, missing []model.MissingPiece) (model.MissingPiece, bool) {
	for _, rp := range p.RequiresMountsFor {
		for i, w := range mountWheres {
			if pathWithin(rp, w) {
				return mountPieces[i], true
			}
		}
		if piece, ok := pieceForPath(rp, missing); ok {
			return piece, true
		}
	}
	for _, deps := range [][]string{p.Requires, p.BindsTo} {
		for _, d := range deps {
			if piece, ok := pieceOf[d]; ok {
				return piece, true
			}
		}
	}
	return model.MissingPiece{}, false
}

// pieceForPath returns the first missing guest-path piece (bind mount or
// excluded mount point) that p is equal to or under.
func pieceForPath(p string, missing []model.MissingPiece) (model.MissingPiece, bool) {
	for _, m := range missing {
		if (m.Kind == "bind-mount" || m.Kind == "excluded-mountpoint") && m.Path != "" && pathWithin(p, m.Path) {
			return m, true
		}
	}
	return model.MissingPiece{}, false
}

// pathWithin reports whether p equals base or lies under it, comparing whole
// path segments (/media2 is not under /media). Both must be absolute, and a
// base of / never matches.
func pathWithin(p, base string) bool {
	if !strings.HasPrefix(p, "/") || !strings.HasPrefix(base, "/") {
		return false
	}
	p, base = path.Clean(p), path.Clean(base)
	if base == "/" {
		return false // the root cannot be missing; treating it as such would cover everything
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

// resolveSource turns a mount source into a device path under /dev/,
// resolving UUID=/LABEL=/PARTUUID= tags. It returns "" for anything else
// (loop-mounted image files and other paths are not disks).
func resolveSource(what string) string {
	for _, sp := range sourcePrefixes {
		if v, ok := strings.CutPrefix(what, sp.tag); ok && v != "" && !strings.Contains(v, "/") {
			return sp.dir + v
		}
	}
	if p := path.Clean(what); strings.HasPrefix(p, "/dev/") {
		return p
	}
	return ""
}
