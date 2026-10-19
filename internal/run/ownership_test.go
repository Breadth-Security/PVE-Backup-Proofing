package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/sanitize"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

// Ownership by state (design section 11.2): a guest in the range is PBP's if
// it is tagged "pbp" or the state store records its VMID as the target of a
// restore PBP started. These tests use the real target on pvefake.

const ownVol = "pbs:backup/vm/101/2026-10-18T22:00:00Z"

// isolatedNode makes the fixture's node pass the real Prepare: an isolated
// bridge (API and live state over SSH) and a usable restore storage.
func (f *janitorFixture) isolatedNode(api pve.API) {
	br := f.d.Cfg.Target.Bridge
	f.api.Ifaces[br] = pve.NetIface{Iface: br, Type: "bridge"}
	f.api.Storage[f.d.Cfg.Restore.Storage] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,rootdir"}
	answers := map[string]sshx.Result{
		"cat " + sshx.Quote("/proc/sys/net/ipv6/conf/"+br+"/disable_ipv6"): {Stdout: "1\n"},
		"ip -j addr show dev " + sshx.Quote(br):                            {Stdout: `[{"ifname":"` + br + `","addr_info":[]}]`},
		"ip -j link show master " + sshx.Quote(br):                         {Stdout: "[]\n"},
	}
	ssh := &sshx.Fake{Handler: func(cmd string, _ []byte) (sshx.Result, error) {
		if r, ok := answers[cmd]; ok {
			return r, nil
		}
		return sshx.Result{ExitCode: 127, Stderr: "unexpected command " + cmd}, nil
	}}
	f.tgt = &destroyLog{Target: target.NewExistingNode(f.d.Cfg, api, ssh)}
	f.d.Target, f.d.API = f.tgt, api
	f.d.Notifier = &fakeNotifier{}
}

// restoreTaskFails fails the restore task after the guest was created, as a
// PBS read error mid-restore does on a real node.
type restoreTaskFails struct{ pve.API }

func (a restoreTaskFails) WaitTask(ctx context.Context, node, upid string) error {
	if strings.Contains(upid, ":restore:") {
		return &pve.TaskError{UPID: upid, ExitStatus: "error", Log: []string{"unable to read archive: bad chunk digest"}}
	}
	return a.API.WaitTask(ctx, node, upid)
}

func (f *janitorFixture) ownBackup() model.Backup {
	b := model.Backup{Storage: "pbs", VolID: ownVol, Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101}}
	f.api.Backups["pbs"] = append(f.api.Backups["pbs"], b)
	f.api.BackupConfigs[ownVol] = "name: db01\nprotection: 1\n"
	return b
}

// A restore that fails after creating the guest leaves it untagged (and
// here protected, from the backup's config). Teardown removes it at once by
// the state record; nothing is left for the next run.
func TestRestoreFailureLeavingGuestIsDestroyedViaState(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(restoreTaskFails{f.api})
	b := f.ownBackup()

	res := proofOne(context.Background(), f.d, b, 990000, sanitize.ParseConfigText(f.api.BackupConfigs[ownVol]))

	if res.Verdict != model.Failed || !strings.HasPrefix(res.Reason, "restore failed: ") || strings.Contains(res.Reason, "cleanup failed") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	if f.exists(990000) || !f.destroyed(990000) {
		t.Error("partial guest not destroyed")
	}
	if len(f.store.Snapshot().InFlight) != 0 {
		t.Errorf("InFlight = %+v", f.store.Snapshot().InFlight)
	}
	if f.protected(ownVol) {
		t.Error("backup left protected")
	}
	assertNoStrayMutation(t, f.api.CallLog())
}

// If the node refuses the restore request, no guest of PBP's exists: a guest
// at the VMID (here one created meanwhile by someone else) is never touched.
func TestRestoreRefusedNeverDestroys(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(f.api)
	b := f.ownBackup()
	f.guest(model.KindQEMU, 990000, false)
	f.api.GuestList[0].Status = "stopped"
	f.api.Fail["Restore"] = &pve.APIError{Status: 500, Path: "/nodes/pve1/qemu", Message: "VM 990000 already exists"}

	res := proofOne(context.Background(), f.d, b, 990000, sanitize.ParseConfigText(f.api.BackupConfigs[ownVol]))

	if res.Verdict != model.Error || strings.Contains(res.Reason, "cleanup failed") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	if !f.exists(990000) || f.destroyed(990000) || len(f.tgt.calls) != 0 {
		t.Errorf("foreign guest touched (destroy calls %v)", f.tgt.calls)
	}
	if len(f.store.Snapshot().InFlight) != 0 {
		t.Errorf("InFlight = %+v", f.store.Snapshot().InFlight)
	}
	if f.protected(ownVol) {
		t.Error("backup left protected")
	}
}

// A crashed run left an untagged guest at a VMID recorded as a started
// restore: Prepare accepts it and the janitor destroys it.
func TestPreflightCleansUntaggedRecordedGuest(t *testing.T) {
	for _, stage := range []string{state.StageRestored, state.StageStarted} {
		t.Run(stage, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.leftover(990001, "x")
			f.inFlight(state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(false), Stage: stage})

			if err := Preflight(context.Background(), f.d); err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			if f.exists(990001) || !f.destroyed(990001) {
				t.Error("recorded untagged guest not destroyed")
			}
			if f.protected("pbs:v1") || len(f.store.Snapshot().InFlight) != 0 {
				t.Errorf("job not finished: protected=%v InFlight=%+v", f.protected("pbs:v1"), f.store.Snapshot().InFlight)
			}
		})
	}
}

// leftover adds a stopped untagged qemu guest at vmid named name, and backup
// pbs:v1 (protected) whose original config is named "x".
func (f *janitorFixture) leftover(vmid int, name string) {
	f.guest(model.KindQEMU, vmid, false)
	g := &f.api.GuestList[len(f.api.GuestList)-1]
	g.Status = "stopped"
	f.api.Configs[vmid]["name"] = name
	f.backup("pbs:v1", true)
	f.api.BackupConfigs["pbs:v1"] = "name: x\n"
}

// assertForeignAndKept: Preflight refuses with ErrForeignInRange naming vmid,
// and the janitor on its own (Preflight stops before it) leaves it alone.
func (f *janitorFixture) assertForeignAndKept(t *testing.T, vmid int) {
	t.Helper()
	err := Preflight(context.Background(), f.d)
	if !errors.Is(err, target.ErrForeignInRange) || !strings.Contains(err.Error(), fmt.Sprint(vmid)) {
		t.Errorf("Preflight err = %v, want ErrForeignInRange naming %d", err, vmid)
	}
	_ = Janitor(context.Background(), f.d)
	if !f.exists(vmid) || f.destroyed(vmid) {
		t.Errorf("SAFETY: untagged guest %d destroyed", vmid)
	}
}

// Without a record of a started restore, an untagged guest in the range is
// foreign even if it is stopped and named as the backup: Prepare refuses to
// run and nothing destroys it. With a record, a guest named otherwise is
// foreign too.
func TestUntaggedGuestWithoutRecordIsForeign(t *testing.T) {
	cases := map[string]struct {
		job  *state.Job
		name string
	}{
		"no entry":             {nil, "x"},
		"stage protected":      {&state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageProtected}, "x"},
		"stage destroyed":      {&state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageDestroyed}, "x"},
		"other VMID entered":   {&state.Job{TempVMID: 990002, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageRestored}, "x"},
		"recorded, other name": {&state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageRestored}, "someone-elses"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.leftover(990001, tc.name)
			if tc.job != nil {
				f.inFlight(*tc.job)
			}
			f.assertForeignAndKept(t, 990001)
		})
	}
}

// The janitor records "destroyed" as soon as the guest is gone, so a failed
// unprotect never leaves an empty VMID recorded as an in-flight restore: a
// guest placed there later (even one named as the backup) is foreign.
func TestJanitorRecordsDestroyedBeforeUnprotect(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(f.api)
	f.leftover(990001, "x")
	f.inFlight(state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageRestored})
	f.api.Fail["SetVolumeAttrs"] = errors.New("pbs unreachable")

	if err := Janitor(context.Background(), f.d); err == nil {
		t.Fatal("want unprotect error")
	}
	if f.exists(990001) {
		t.Fatal("leftover not destroyed")
	}
	if j, ok := f.store.Snapshot().InFlight[990001]; !ok || j.Stage != state.StageDestroyed {
		t.Fatalf("InFlight = %+v, %v; want stage destroyed kept for the unprotect retry", j, ok)
	}

	// An operator restores something into the now-empty VMID by hand.
	f.api.ResetCalls() // forget the legitimate destroy above
	f.guest(model.KindQEMU, 990001, false)
	f.api.GuestList[len(f.api.GuestList)-1].Status = "stopped"
	f.api.Configs[990001]["name"] = "x"
	f.assertForeignAndKept(t, 990001)
}

// storeBreaksOnRestore refuses the restore request and makes the state store
// unwritable first, so the job cannot record that no restore started.
type storeBreaksOnRestore struct {
	pve.API
	dir string
}

func (a storeBreaksOnRestore) Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (string, error) {
	// Removing the directory breaks the store's writes even for root, which
	// ignores permission bits (the Docker build runs the tests as root).
	if err := os.RemoveAll(a.dir); err != nil {
		return "", err
	}
	return "", &pve.APIError{Status: 500, Path: "/nodes/pve1/qemu", Message: "VM 990000 already exists"}
}

// A refused restore whose "not started" record cannot be written leaves a
// stale "restored" entry. That entry still never makes a foreign guest at
// the VMID PBP's: it is named otherwise, so Prepare and the janitor refuse.
func TestRefusedRestoreWithStaleRecordFailsSafe(t *testing.T) {
	f := newJanitorFixture(t)
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.d.Store = store, store
	f.isolatedNode(storeBreaksOnRestore{API: f.api, dir: dir})
	b := f.ownBackup()
	f.guest(model.KindQEMU, 990000, false) // the foreign guest that made the restore fail
	f.api.GuestList[len(f.api.GuestList)-1].Status = "stopped"

	res := proofOne(context.Background(), f.d, b, 990000, sanitize.ParseConfigText(f.api.BackupConfigs[ownVol]))

	if res.Verdict != model.Error || len(f.tgt.calls) != 0 {
		t.Errorf("result %s %q, destroy calls %v", res.Verdict, res.Reason, f.tgt.calls)
	}
	if j, ok := f.store.Snapshot().InFlight[990000]; !ok || j.Stage != state.StageRestored {
		t.Fatalf("InFlight = %+v, %v; the test needs the stale record", j, ok)
	}
	f.assertForeignAndKept(t, 990000)
}

// Preflight hands Prepare exactly the VMIDs whose restore may have started.
func TestPreflightPassesRecordedVMIDs(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	err := f.store.Update(func(st *state.State) error {
		for v, stage := range map[int]string{990000: state.StageProtected, 990001: state.StageRestored, 990002: state.StageStarted, 990003: state.StageDestroyed} {
			st.InFlight[v] = state.Job{TempVMID: v, Kind: model.KindQEMU, Stage: stage, ProtectedOrig: ptr(true)}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tgt.PrepareErr = errors.New("stop here")
	_ = Preflight(context.Background(), f.d)
	want := map[int]target.Recorded{990001: {Kind: model.KindQEMU}, 990002: {Kind: model.KindQEMU}}
	if got := f.tgt.Recorded; !maps.Equal(got, want) {
		t.Errorf("recorded = %v, want %v", got, want)
	}
}
