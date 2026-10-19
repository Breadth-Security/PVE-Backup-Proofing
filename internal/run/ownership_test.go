package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target/bridgetest"
)

// Ownership by state (design section 11.2): a guest in the range is PBP's if
// it is tagged "pbp" or the state store records its VMID as the target of a
// restore PBP started. These tests use the real target on pvefake.

const ownVol = "pbs:backup/vm/101/2026-10-18T22:00:00Z"

// isolatedNode makes the fixture's node pass the real Prepare: an isolated
// bridge (API and the helper's live state) and a usable restore storage.
func (f *janitorFixture) isolatedNode(api pve.API) {
	br := f.d.Cfg.Target.Bridge
	f.api.Ifaces[br] = pve.NetIface{Iface: br, Type: "bridge"}
	f.api.Storage[f.d.Cfg.Restore.Storage] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,rootdir"}
	f.tgt = &destroyLog{Target: target.NewExistingNode(f.d.Cfg, api, bridgetest.Isolated(br).Helper())}
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
	assertNoStrayMutation(t, f.api.CallLog(), f.d.Cfg.Target.VMIDRange, nil)
}

// If the node refuses the restore request, no guest of PBP's exists: a guest
// at the VMID (here one created meanwhile by someone else) is never touched.
func TestRestoreRefusedNeverDestroys(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(f.api)
	b := f.ownBackup()
	f.stoppedGuest(990000, "x")
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
// restore: the janitor destroys it, and the range check then passes.
func TestPreflightCleansUntaggedRecordedGuest(t *testing.T) {
	for _, stage := range []string{state.StageRestored, state.StageStarted} {
		t.Run(stage, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.leftover(990001, "x")
			f.inFlight(jobAt(990001, stage))

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
	f.stoppedGuest(vmid, name)
	f.backup("pbs:v1", true)
	f.api.BackupConfigs["pbs:v1"] = "name: x\n"
}

// jobAt is an InFlight qemu job at vmid for backup pbs:v1, which PBP
// protected (its original flag was false).
func jobAt(vmid int, stage string) state.Job {
	return state.Job{TempVMID: vmid, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(false), Stage: stage}
}

// assertRefusedAndKept: Preflight fails with want naming vmid, the guest
// there is untouched, and so it stays when the janitor runs on its own.
func (f *janitorFixture) assertRefusedAndKept(t *testing.T, vmid int, want error) {
	t.Helper()
	err := Preflight(context.Background(), f.d)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), fmt.Sprint(vmid)) {
		t.Errorf("Preflight err = %v, want %v naming %d", err, want, vmid)
	}
	_ = Janitor(context.Background(), f.d)
	if !f.exists(vmid) || f.destroyed(vmid) {
		t.Errorf("SAFETY: untagged guest %d destroyed", vmid)
	}
}

// Without a record of a started restore, an untagged guest in the range is
// foreign even if it is stopped and named as the backup: the janitor leaves
// it, the range check refuses to run, and nothing destroys it.
func TestUntaggedGuestWithoutRecordIsForeign(t *testing.T) {
	cases := map[string]*state.Job{
		"no entry":           nil,
		"stage destroyed":    ptrJob(jobAt(990001, state.StageDestroyed)),
		"other VMID entered": ptrJob(jobAt(990002, state.StageRestored)),
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.leftover(990001, "x")
			if job != nil {
				f.inFlight(*job)
			}
			f.assertRefusedAndKept(t, 990001, target.ErrForeignInRange)
		})
	}
}

// An entry that does not make the untagged guest PBP's (no restore started,
// or the guest is named otherwise) has the janitor refuse to destroy it:
// the entry is kept and the run aborts.
func TestJanitorRefusesGuestTheRecordDoesNotCover(t *testing.T) {
	cases := map[string]struct {
		stage, name string
	}{
		"stage protected":      {state.StageProtected, "x"},
		"recorded, other name": {state.StageRestored, "someone-elses"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.leftover(990001, tc.name)
			f.inFlight(jobAt(990001, tc.stage))
			f.assertRefusedAndKept(t, 990001, target.ErrNotOwned)
			if _, ok := f.store.Snapshot().InFlight[990001]; !ok {
				t.Error("refused entry removed")
			}
		})
	}
}

func ptrJob(j state.Job) *state.Job { return &j }

// The janitor records "destroyed" as soon as the guest is gone, so a failed
// unprotect never leaves an empty VMID recorded as an in-flight restore: a
// guest placed there later (even one named as the backup) is foreign.
func TestJanitorRecordsDestroyedBeforeUnprotect(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(f.api)
	f.leftover(990001, "x")
	f.inFlight(jobAt(990001, state.StageRestored))
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

	// An operator restores something into the now-empty VMID by hand; PBS
	// is back, so the janitor finishes the entry and the range check runs.
	f.api.ResetCalls() // forget the legitimate destroy above
	delete(f.api.Fail, "SetVolumeAttrs")
	f.stoppedGuest(990001, "x")
	f.assertRefusedAndKept(t, 990001, target.ErrForeignInRange)
}

// storeBreaksOnRestore refuses the restore request and makes the state store
// unwritable first, so the job cannot record that no restore started.
type storeBreaksOnRestore struct {
	pve.API
	dir string
}

func (a storeBreaksOnRestore) Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (string, error) {
	// Remove the directory rather than chmod it: the Docker build runs the
	// tests as root, which ignores permission bits, but cannot write into a
	// directory that no longer exists.
	if err := os.RemoveAll(a.dir); err != nil {
		return "", err
	}
	return "", &pve.APIError{Status: 500, Path: "/nodes/pve1/qemu", Message: "VM 990000 already exists"}
}

// A refused restore whose "not started" record cannot be written leaves a
// stale "restored" entry. That entry still never makes a foreign guest at
// the VMID PBP's: it is named otherwise, so the janitor refuses it.
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
	f.stoppedGuest(990000, "x") // the foreign guest that made the restore fail

	res := proofOne(context.Background(), f.d, b, 990000, sanitize.ParseConfigText(f.api.BackupConfigs[ownVol]))

	if res.Verdict != model.Error || len(f.tgt.calls) != 0 {
		t.Errorf("result %s %q, destroy calls %v", res.Verdict, res.Reason, f.tgt.calls)
	}
	if err := f.store.Update(func(*state.State) error { return nil }); err == nil {
		t.Error("the state store is still writable; the test needs it broken")
	}
	if j, ok := f.store.Snapshot().InFlight[990000]; !ok || j.Stage != state.StageRestored {
		t.Fatalf("InFlight = %+v, %v; the test needs the stale record", j, ok)
	}
	f.assertRefusedAndKept(t, 990000, target.ErrNotOwned)
}

// Preflight runs Prepare, the templates, the janitor and then the range
// check, stopping at the first failure.
func TestPreflightOrder(t *testing.T) {
	ctx := context.Background()
	t.Run("all pass", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		if err := Preflight(ctx, f.d); err != nil {
			t.Fatal(err)
		}
		if got, want := f.tgt.CallLog(), []string{"Prepare", "Owned", "CheckRangeClear"}; !slices.Equal(got, want) || f.notif.checked != 1 {
			t.Errorf("calls %q (want %q), templates checked %d", got, want, f.notif.checked)
		}
	})
	t.Run("prepare fails", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		f.tgt.PrepareErr = errors.New("stop here")
		if err := Preflight(ctx, f.d); err == nil || !strings.HasPrefix(err.Error(), "target: ") {
			t.Errorf("err = %v", err)
		}
		if got := f.tgt.CallLog(); !slices.Equal(got, []string{"Prepare"}) || f.notif.checked != 0 {
			t.Errorf("calls %q, templates checked %d", got, f.notif.checked)
		}
	})
	t.Run("templates fail", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		f.notif.checkErr = errors.New("stop here")
		if err := Preflight(ctx, f.d); err == nil {
			t.Error("want error")
		}
		if got := f.tgt.CallLog(); !slices.Equal(got, []string{"Prepare"}) {
			t.Errorf("calls %q: janitor ran", got)
		}
	})
	t.Run("janitor fails", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		f.tgt.OwnedErr = errors.New("stop here")
		if err := Preflight(ctx, f.d); err == nil || !strings.HasPrefix(err.Error(), "janitor: ") {
			t.Errorf("err = %v", err)
		}
		if slices.Contains(f.tgt.CallLog(), "CheckRangeClear") {
			t.Error("range checked after a failed janitor")
		}
	})
	t.Run("range not clear", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		f.tgt.RangeErr = fmt.Errorf("%w: qemu/990003", target.ErrForeignInRange)
		if err := Preflight(ctx, f.d); !errors.Is(err, target.ErrForeignInRange) || !strings.HasPrefix(err.Error(), "target: ") {
			t.Errorf("err = %v", err)
		}
	})
}
