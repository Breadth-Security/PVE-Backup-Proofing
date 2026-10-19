package run

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
)

const (
	wantStrayVM = "VM 105 (web) is tagged pbp but outside the PBP range 990000–990009; not touched"
	wantStrayCT = "CT 210 is tagged pbp but outside the PBP range 990000–990009; not touched"
)

func strayGuests() []pve.GuestRef {
	return []pve.GuestRef{
		{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 105}, Node: "pve1", Name: "web", Status: "running", Tags: []string{"pbp"}},
		{Key: model.GuestKey{Kind: model.KindLXC, VMID: 210}, Node: "pve1", Status: "running", Tags: []string{"pbp"}},
	}
}

// strayBackup seeds a backup of a stray guest, which PBP must never write to.
func (f *janitorFixture) strayBackup(k model.GuestKey) {
	f.api.Backups["pbs"] = append(f.api.Backups["pbs"], model.Backup{Storage: "pbs", VolID: "pbs:backup/stray/" + k.String(), Key: k})
}

func TestRunReportsStrayTaggedGuests(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.tgt.StrayGuests = strayGuests()
	f.add("pbs", model.KindQEMU, 101, 1024, "")

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	wantResult(t, byVMID(t, s)[101], model.Proven, "")
	if !slices.Equal(s.EnvironmentErrors, []string{wantStrayVM, wantStrayCT}) {
		t.Errorf("EnvironmentErrors = %q", s.EnvironmentErrors)
	}
	if s.RunError != "" || s.Severity() != "warning" {
		t.Errorf("run error %q, severity %s", s.RunError, s.Severity())
	}
	if r := f.lastRun(t); r.Outcome != state.OutcomeProblems {
		t.Errorf("outcome = %q, want problems", r.Outcome)
	}
	if sent := f.notif.sends(); len(sent) != 1 || len(sent[0].EnvironmentErrors) != 2 {
		t.Errorf("sent = %+v", sent)
	}
	for _, w := range []string{wantStrayVM, wantStrayCT} {
		if !strings.Contains(f.logBuf.String(), "level=WARN msg=\""+w+"\"") {
			t.Errorf("no WARN log for %q in:\n%s", w, f.logBuf)
		}
	}
	for _, c := range f.tgt.CallLog() {
		if strings.Contains(c, " 105") || strings.Contains(c, " 210") {
			t.Errorf("target call touched a stray guest: %q", c)
		}
	}
}

func TestRunStrayCheckFailureIsReportedAndRunContinues(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.tgt.StrayErr = errors.New("api down")
	f.add("pbs", model.KindQEMU, 101, 1024, "")

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	wantResult(t, byVMID(t, s)[101], model.Proven, "")
	want := []string{"could not check for stray pbp-tagged guests: api down"}
	if !slices.Equal(s.EnvironmentErrors, want) || s.RunError != "" {
		t.Errorf("EnvironmentErrors = %q, RunError = %q", s.EnvironmentErrors, s.RunError)
	}
	if r := f.lastRun(t); r.Outcome != state.OutcomeProblems {
		t.Errorf("outcome = %q", r.Outcome)
	}
}

func TestRunNoStrayCheckWhenPreflightFails(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.tgt.PrepareErr = errors.New("nope")
	f.tgt.StrayGuests = strayGuests()
	s := f.run(context.Background(), Options{Trigger: "schedule"})
	if len(s.EnvironmentErrors) != 0 || s.Severity() != "error" {
		t.Errorf("env errors %q, severity %s", s.EnvironmentErrors, s.Severity())
	}
}

// With the real target (Prepare included) and a pvefake node holding
// pbp-tagged guests outside the range, a full run that proofs a backup never
// issues a call that names them.
func TestRunNeverTouchesStrayGuestsOnRealTarget(t *testing.T) {
	f := newJanitorFixture(t)
	f.isolatedNode(f.api)
	f.d.Cfg.Sources.Storages = []string{"pbs"}
	f.d.Cfg.Notify.When = "always"
	f.api.Storage["local-zfs"] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,rootdir", Avail: 1 << 40, Total: 2 << 40}
	f.api.Memory = pve.NodeMemory{Total: 64 << 30, Available: 32 << 30}
	f.ownBackup() // qemu 101, no guest agent: proofed to BOOTED
	for _, g := range strayGuests() {
		f.guest(g.Key.Kind, g.Key.VMID, true)
		f.api.GuestList[len(f.api.GuestList)-1].Name = g.Name
	}

	s := Run(context.Background(), f.d, Options{Trigger: "manual"})

	if len(s.Results) != 1 || s.Results[0].Verdict != model.Booted || s.Results[0].TempVMID != 990000 {
		t.Fatalf("results = %+v (RunError %q)", s.Results, s.RunError)
	}
	if !f.destroyed(990000) || f.exists(990000) {
		t.Error("proof guest not torn down")
	}
	if !slices.Equal(s.EnvironmentErrors, []string{wantStrayVM, wantStrayCT}) {
		t.Errorf("EnvironmentErrors = %q (RunError %q)", s.EnvironmentErrors, s.RunError)
	}
	if s.Severity() != "warning" {
		t.Errorf("severity = %s", s.Severity())
	}
	assertNoStrayMutation(t, f.api.CallLog(), f.d.Cfg.Target.VMIDRange, nil)
	if !f.exists(105) || !f.exists(210) {
		t.Error("stray guest removed")
	}
}

func TestJanitorLeavesStrayTaggedGuestsAlone(t *testing.T) {
	f := newJanitorFixture(t)
	for _, g := range strayGuests() {
		f.guest(g.Key.Kind, g.Key.VMID, true)
		f.strayBackup(g.Key)
	}
	f.guest(model.KindQEMU, 990001, true) // an owned leftover is still destroyed

	if err := Janitor(context.Background(), f.d); err != nil {
		t.Fatal(err)
	}
	if f.exists(990001) {
		t.Error("owned leftover not destroyed")
	}
	if !f.exists(105) || !f.exists(210) || f.destroyed(105) || f.destroyed(210) {
		t.Error("stray guest destroyed")
	}
	assertNoStrayMutation(t, f.api.CallLog(), f.d.Cfg.Target.VMIDRange, strayGuests())
}

// assertNoStrayMutation fails if any call logged by pvefake mutates a stray
// guest or writes a stray guest's backup: a guest mutation ("Method node kind
// vmid ...") naming a VMID outside vmidRange, or a SetVolumeAttrs whose
// "guest=kind/vmid" is one of strays. (Backups of other guests are legitimately
// written while proofing, so callers that proof pass no strays.)
func assertNoStrayMutation(t *testing.T, calls []string, vmidRange [2]int, strays []pve.GuestRef) {
	t.Helper()
	guestMutations := []string{"Restore", "SetGuestConfig", "Start", "Stop", "Shutdown", "Destroy"}
	for _, c := range calls {
		fs := strings.Fields(c)
		switch {
		case fs[0] == "SetVolumeAttrs":
			for _, g := range strays {
				if slices.Contains(fs, "guest="+g.Key.String()) {
					t.Errorf("SAFETY: a stray guest's backup was written: %q", c)
				}
			}
		case slices.Contains(guestMutations, fs[0]):
			if len(fs) < 4 {
				t.Errorf("unparseable mutating call %q", c)
			} else if v, err := strconv.Atoi(fs[3]); err != nil || v < vmidRange[0] || v > vmidRange[1] {
				t.Errorf("SAFETY: mutating call names a guest outside the range: %q", c)
			}
		}
	}
}
