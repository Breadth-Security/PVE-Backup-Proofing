// Package discovery selects which backups PBP should prove and in what order.
package discovery

import (
	"sort"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/notes"
)

// NewestPerGuest returns the newest backup of each guest across all storages.
// On equal Time the earlier entry in the input wins. Results appear in the
// order each guest is first seen in the input. The input is not modified.
func NewestPerGuest(all []model.Backup) []model.Backup {
	idx := make(map[model.GuestKey]int, len(all))
	out := make([]model.Backup, 0, len(all))
	for _, b := range all {
		i, seen := idx[b.Key]
		if !seen {
			idx[b.Key] = len(out)
			out = append(out, b)
		} else if b.Time.After(out[i].Time) {
			out[i] = b
		}
	}
	return out
}

// OutsideRange drops backups whose VMID lies in [lo, hi], PBP's reserved
// range: those are backups of PBP's own temporary guests. It returns the
// kept backups in input order and each dropped guest once, in first-seen
// order. The input is not modified.
func OutsideRange(bs []model.Backup, lo, hi int) (kept []model.Backup, skipped []model.GuestKey) {
	seen := map[model.GuestKey]bool{}
	for _, b := range bs {
		if b.Key.VMID < lo || b.Key.VMID > hi {
			kept = append(kept, b)
			continue
		}
		if !seen[b.Key] {
			seen[b.Key] = true
			skipped = append(skipped, b.Key)
		}
	}
	return kept, skipped
}

// Order returns a copy of bs with never-proven guests first (by VMID, then
// kind), followed by proven guests with the oldest lastProven first (ties by
// VMID, then kind). The input is not modified.
func Order(bs []model.Backup, lastProven map[model.GuestKey]time.Time) []model.Backup {
	out := append([]model.Backup(nil), bs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		ta, okA := lastProven[a]
		tb, okB := lastProven[b]
		if okA != okB {
			return !okA
		}
		if okA && !ta.Equal(tb) {
			return ta.Before(tb)
		}
		if a.VMID != b.VMID {
			return a.VMID < b.VMID
		}
		return a.Kind < b.Kind
	})
	return out
}

// AlreadyProven reports whether b already carries a PROVEN note and need not
// be proven again; reproof forces re-proving.
func AlreadyProven(b model.Backup, reproof bool) bool {
	return !reproof && notes.HasProven(b.Notes)
}
