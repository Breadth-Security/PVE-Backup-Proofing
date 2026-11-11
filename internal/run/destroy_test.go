package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
)

// A test copy removed only without PVE's scan for unreferenced disks keeps
// its verdict, and the run carries one warning naming the cause, the node
// and the restore storage (issue #108). One copy: grouping several is
// TestScanSkippedWarningsGroupRealTaskErrors.
func TestRunReportsDestroyWithoutScan(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.tgt.ScanSkip = map[int]error{990000: errors.New("destroy 990000: no such logical volume pve/data")}

	s := f.run(context.Background(), Options{Trigger: "manual"})

	for vmid, r := range byVMID(t, s) {
		if r.Verdict != model.Proven || strings.Contains(r.Reason, "not removed") {
			t.Errorf("VM%d: %s %q", vmid, r.Verdict, r.Reason)
		}
	}
	var hits []string
	for _, e := range s.EnvironmentErrors {
		if strings.Contains(e, "without PVE's scan for unreferenced disks") {
			hits = append(hits, e)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("env = %q, want one grouped warning", s.EnvironmentErrors)
	}
	for _, want := range []string{"test copy 990000", "no such logical volume pve/data", "node pve1", "pvesm status", "local-zfs"} {
		if !strings.Contains(hits[0], want) {
			t.Errorf("warning %q lacks %q", hits[0], want)
		}
	}
	if s.Severity() != "warning" {
		t.Errorf("severity %s", s.Severity())
	}
	if len(f.tgt.Live) != 0 {
		t.Errorf("copies left: %v", f.tgt.Live)
	}
}

func TestScanSkippedWarningsGroupByCause(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	rs := []model.GuestResult{
		{TempVMID: 990001, DestroyScanSkipped: "lvm broke"},
		{TempVMID: 990000, DestroyScanSkipped: "lvm broke"},
		{TempVMID: 990000, DestroyScanSkipped: "lvm broke"}, // a reused VMID counts once
		{TempVMID: 990002, DestroyScanSkipped: "nfs gone"},
		{TempVMID: 990003},
	}
	ws := scanSkippedWarnings(f.d, rs)
	if len(ws) != 2 {
		t.Fatalf("warnings = %q", ws)
	}
	if !strings.HasPrefix(ws[0], "test copies 990000, 990001 removed without PVE's scan for unreferenced disks, because destroying them failed: lvm broke.") ||
		!strings.Contains(ws[0], "leftover disks of those VMIDs on local-zfs (check: pvesm list local-zfs --vmid <vmid>)") {
		t.Errorf("first = %q", ws[0])
	}
	if !strings.HasPrefix(ws[1], "test copy 990002 removed without PVE's scan for unreferenced disks, because destroying it failed: nfs gone.") ||
		!strings.Contains(ws[1], "leftover disks of that VMID") {
		t.Errorf("second = %q", ws[1])
	}
}

// Real PVE causes embed the task's UPID and the VMID, which differ per copy;
// the warning groups by PVE's exit status instead, without them (review of
// #108).
func TestScanSkippedWarningsGroupRealTaskErrors(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	cause := func(vmid int, upid string) error {
		return fmt.Errorf("destroy %d: %w", vmid, &pve.TaskError{UPID: upid, ExitStatus: "no such logical volume pve/data",
			Log: []string{"  WARNING: Cannot find matching striped segment", "TASK ERROR: no such logical volume pve/data"}})
	}
	rs := []model.GuestResult{
		{TempVMID: 990000, DestroyScanSkipped: scanCause(cause(990000, "UPID:pve1:0001:AA:qmdestroy:990000:pbp@pve!proof:"))},
		{TempVMID: 990001, DestroyScanSkipped: scanCause(cause(990001, "UPID:pve1:0002:BB:qmdestroy:990001:pbp@pve!proof:"))},
	}
	ws := scanSkippedWarnings(f.d, rs)
	if len(ws) != 1 {
		t.Fatalf("warnings = %q, want one", ws)
	}
	if !strings.Contains(ws[0], "test copies 990000, 990001") || !strings.Contains(ws[0], "no such logical volume pve/data") ||
		strings.Contains(ws[0], "UPID") || strings.Contains(ws[0], "striped segment") || !strings.Contains(ws[0], "--vmid <vmid>") {
		t.Errorf("warning = %q", ws[0])
	}
}
