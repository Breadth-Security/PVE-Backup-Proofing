package run

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
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
	if r := f.lastRun(t); r.Outcome != "problems" {
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
	if r := f.lastRun(t); r.Outcome != "problems" {
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

// preparedTarget is the production ExistingNode with Prepare (SSH bridge
// checks) stubbed out, so StrayTagged, Owned and Destroy are the real ones.
type preparedTarget struct{ target.Target }

func (preparedTarget) Prepare(context.Context) error { return nil }

// With the real target and a pvefake node holding pbp-tagged guests outside
// the range, a full run never issues a call that names them.
func TestRunNeverTouchesStrayGuestsOnRealTarget(t *testing.T) {
	f := newJanitorFixture(t)
	f.d.Target = preparedTarget{f.tgt}
	f.d.Notifier = &fakeNotifier{}
	f.d.Cfg.Sources.Storages = []string{"pbs"}
	f.d.Cfg.Notify.When = "always"
	for _, g := range strayGuests() {
		f.guest(g.Key.Kind, g.Key.VMID, true)
		f.api.GuestList[len(f.api.GuestList)-1].Name = g.Name
	}

	s := Run(context.Background(), f.d, Options{Trigger: "manual"})

	if !slices.Equal(s.EnvironmentErrors, []string{wantStrayVM, wantStrayCT}) {
		t.Errorf("EnvironmentErrors = %q (RunError %q)", s.EnvironmentErrors, s.RunError)
	}
	if s.Severity() != "warning" {
		t.Errorf("severity = %s", s.Severity())
	}
	assertNoStrayMutation(t, f.api.CallLog())
	if !f.exists(105) || !f.exists(210) {
		t.Error("stray guest removed")
	}
}

func TestJanitorLeavesStrayTaggedGuestsAlone(t *testing.T) {
	f := newJanitorFixture(t)
	for _, g := range strayGuests() {
		f.guest(g.Key.Kind, g.Key.VMID, true)
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
	assertNoStrayMutation(t, f.api.CallLog())
}

// assertNoStrayMutation fails if any mutating API call (logged as
// "Method node kind vmid ...") names a VMID outside the PBP range.
func assertNoStrayMutation(t *testing.T, calls []string) {
	t.Helper()
	mutating := []string{"Restore", "SetGuestConfig", "Start", "Stop", "Shutdown", "Destroy", "SetVolumeAttrs"}
	for _, c := range calls {
		fs := strings.Fields(c)
		if !slices.Contains(mutating, fs[0]) {
			continue
		}
		for _, a := range fs[1:] {
			if v, err := strconv.Atoi(a); err == nil && (v == 105 || v == 210) {
				t.Errorf("SAFETY: mutating call names a stray guest: %q", c)
			}
		}
	}
}
