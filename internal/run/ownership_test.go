package run

import (
	"context"
	"errors"
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
			f.guest(model.KindQEMU, 990001, false)
			f.api.GuestList[0].Status = "stopped"
			f.backup("pbs:v1", true)
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

// Without a record of a started restore, an untagged guest in the range is
// foreign: Prepare refuses to run and nothing destroys it.
func TestUntaggedGuestWithoutRecordIsForeign(t *testing.T) {
	cases := map[string]*state.Job{
		"no entry":           nil,
		"stage protected":    {TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageProtected},
		"stage destroyed":    {TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageDestroyed},
		"other VMID entered": {TempVMID: 990002, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageRestored},
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.isolatedNode(f.api)
			f.guest(model.KindQEMU, 990001, false)
			f.api.GuestList[0].Status = "stopped"
			f.backup("pbs:v1", true)
			if job != nil {
				f.inFlight(*job)
			}

			err := Preflight(context.Background(), f.d)
			if !errors.Is(err, target.ErrForeignInRange) || !strings.Contains(err.Error(), "990001") {
				t.Errorf("Preflight err = %v, want ErrForeignInRange naming 990001", err)
			}
			// The janitor on its own (Preflight stops before it) still
			// leaves the guest alone.
			_ = Janitor(context.Background(), f.d)
			if !f.exists(990001) || f.destroyed(990001) {
				t.Error("SAFETY: untagged guest without a restore record destroyed")
			}
		})
	}
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
	if got := f.tgt.Recorded; len(got) != 2 || !got[990001] || !got[990002] {
		t.Errorf("recorded = %v, want 990001 and 990002", got)
	}
}
