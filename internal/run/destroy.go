package run

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
)

// scanCause is the cause a destroy that needed the fallback is reported and
// grouped by: PVE's one-line exit status, without the task's UPID and the
// VMID, which differ for every copy; else the error on one line.
func scanCause(err error) string {
	if exit, _, ok := pve.TaskFailure(err); ok && strings.TrimSpace(exit) != "" {
		return strings.TrimSpace(exit)
	}
	return oneLine(err.Error())
}

// scanSkippedWarnings are the run's warnings for test copies removed only
// without PVE's scan for unreferenced disks: one per distinct cause, naming
// the copies' VMIDs (issue #108). PVE scans every storage enabled on the
// node, so the cause is usually one of them that is missing or broken.
// Disks of those VMIDs that the config did not name may be left on the
// restore storage, and PBP's token cannot see them (that needs
// VM.Config.Disk), so the warning says how to look.
func scanSkippedWarnings(d Deps, results []model.GuestResult) []string {
	byCause := map[string][]int{}
	var causes []string
	for _, r := range results {
		c := r.DestroyScanSkipped
		if c == "" {
			continue
		}
		if _, ok := byCause[c]; !ok {
			causes = append(causes, c)
		}
		if !slices.Contains(byCause[c], r.TempVMID) {
			byCause[c] = append(byCause[c], r.TempVMID)
		}
	}
	node, store := d.Cfg.Target.Node, d.Cfg.Restore.Storage
	var out []string
	for _, c := range causes {
		vmids := byCause[c]
		slices.Sort(vmids)
		ids := make([]string, len(vmids))
		for i, v := range vmids {
			ids[i] = strconv.Itoa(v)
		}
		copies, them, those, vmid := "test copy "+ids[0], "it", "that VMID", ids[0]
		if len(ids) > 1 {
			copies, them, those, vmid = "test copies "+strings.Join(ids, ", "), "them", "those VMIDs", "<vmid>"
		}
		out = append(out, fmt.Sprintf("%s removed without PVE's scan for unreferenced disks, because destroying %s failed: %s. "+
			"A storage enabled on node %s is likely missing or broken (check pvesm status); PBP cannot see leftover disks of %s on %s "+
			"(check: pvesm list %s --vmid %s)", copies, them, c, node, those, store, store, vmid))
	}
	return out
}
