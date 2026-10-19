package targetfake

import (
	"context"
	"errors"
	"slices"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe/probetest"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

func TestScriptedLifecycle(t *testing.T) {
	ctx := context.Background()
	f := New(990000, 990002)
	f.OwnedGuests = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 990000}}}
	f.SanitizeErr = map[int]error{990002: target.ErrUnsafe}
	f.Boot = map[int]target.BootInfo{990001: {OS: probe.Linux}}
	f.Exec = map[int]probe.Executor{990001: probetest.Scripted{"true": {}}}

	v, err := f.AllocateVMID(ctx, nil)
	if err != nil || v != 990001 {
		t.Fatalf("allocate = %d, %v (990000 is a leftover owned guest)", v, err)
	}
	b := model.Backup{VolID: "vol", Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101}}
	if err := f.Restore(ctx, b, v, nil); err != nil {
		t.Fatal(err)
	}
	if bi, _ := f.WaitBooted(ctx, model.KindQEMU, v, 0); bi.OS != probe.Linux {
		t.Errorf("boot = %+v", bi)
	}
	if r, err := f.Executor(model.KindQEMU, v).Exec(ctx, []string{"true"}); err != nil || r.ExitCode != 0 {
		t.Errorf("exec = %+v, %v", r, err)
	}
	if _, err := f.Executor(model.KindQEMU, 990002).Exec(ctx, []string{"true"}); err == nil {
		t.Error("unscripted executor should fail")
	}
	if _, err := f.Sanitize(ctx, model.KindQEMU, 990002, nil); !errors.Is(err, target.ErrUnsafe) {
		t.Errorf("sanitize err = %v", err)
	}
	if _, err := f.AllocateVMID(ctx, map[int]bool{990002: true}); !errors.Is(err, target.ErrNoFreeVMID) {
		t.Errorf("want ErrNoFreeVMID, got %v", err)
	}
	if err := f.Destroy(ctx, model.KindQEMU, v); err != nil {
		t.Fatal(err)
	}
	if err := f.Destroy(ctx, model.KindQEMU, 990000); err != nil {
		t.Fatal(err)
	}
	if len(f.Live) != 0 || len(f.OwnedGuests) != 0 {
		t.Errorf("live = %v owned = %v", f.Live, f.OwnedGuests)
	}
	want := []string{"AllocateVMID", "Restore 990001 vol", "WaitBooted qemu 990001", "Executor qemu 990001",
		"Executor qemu 990002", "Sanitize qemu 990002", "AllocateVMID", "Destroy qemu 990001", "Destroy qemu 990000"}
	if got := f.CallLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %q", got)
	}
}

func TestZeroValueRestore(t *testing.T) {
	f := &Fake{Range: [2]int{1, 1}}
	if err := f.Restore(context.Background(), model.Backup{}, 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStrayTagged(t *testing.T) {
	f := New(990000, 990002)
	f.StrayGuests = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindLXC, VMID: 210}}}
	got, err := f.StrayTagged(context.Background())
	if err != nil || len(got) != 1 || got[0].Key.VMID != 210 {
		t.Fatalf("got %v, %v", got, err)
	}
	f.StrayErr = errors.New("x")
	if _, err := f.StrayTagged(context.Background()); err == nil {
		t.Error("want error")
	}
}

// The fake enforces the target's safety contracts, so runner tests cannot
// pass by doing what the real target refuses.
func TestContracts(t *testing.T) {
	ctx := context.Background()
	b := model.Backup{VolID: "vol", Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101}}

	t.Run("out of range refused", func(t *testing.T) {
		f := New(990000, 990002)
		const out = 101
		checks := map[string]error{
			"Restore":         f.Restore(ctx, b, out, nil),
			"Start":           f.Start(ctx, model.KindQEMU, out),
			"Destroy":         f.Destroy(ctx, model.KindQEMU, out),
			"DestroyRecorded": f.DestroyRecorded(ctx, model.KindQEMU, out, "vol"),
		}
		_, checks["Sanitize"] = f.Sanitize(ctx, model.KindQEMU, out, nil)
		_, checks["WaitBooted"] = f.WaitBooted(ctx, model.KindQEMU, out, 0)
		_, checks["Executor"] = f.Executor(model.KindQEMU, out).Exec(ctx, []string{"true"})
		for m, err := range checks {
			if !errors.Is(err, target.ErrOutOfRange) {
				t.Errorf("%s(%d) = %v, want ErrOutOfRange", m, out, err)
			}
		}
		if !errors.Is(checks["Restore"], target.ErrRestoreNotStarted) {
			t.Errorf("out-of-range Restore = %v, want ErrRestoreNotStarted", checks["Restore"])
		}
		if len(f.Live) != 0 {
			t.Errorf("live = %v", f.Live)
		}
	})

	t.Run("start needs a successful sanitize", func(t *testing.T) {
		f := New(990000, 990002)
		if err := f.Restore(ctx, b, 990000, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.Start(ctx, model.KindQEMU, 990000); err == nil {
			t.Error("Start before Sanitize succeeded")
		}
		f.SanitizeErr = map[int]error{990000: target.ErrUnsafe}
		if _, err := f.Sanitize(ctx, model.KindQEMU, 990000, nil); err == nil {
			t.Fatal("want sanitize error")
		}
		if err := f.Start(ctx, model.KindQEMU, 990000); err == nil {
			t.Error("Start after failed Sanitize succeeded")
		}
		f.SanitizeErr = nil
		if _, err := f.Sanitize(ctx, model.KindQEMU, 990000, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.Start(ctx, model.KindQEMU, 990000); err != nil {
			t.Errorf("Start after Sanitize: %v", err)
		}
	})

	t.Run("untagged leftover needs DestroyRecorded", func(t *testing.T) {
		f := New(990000, 990002)
		f.RestoreErr = map[int]error{990000: errors.New("task failed")}
		f.RestoreLeaves = map[int]bool{990000: true}
		if err := f.Restore(ctx, b, 990000, nil); err == nil {
			t.Fatal("want restore error")
		}
		if _, ok := f.Live[990000]; !ok {
			t.Fatal("no guest left behind")
		}
		if err := f.Destroy(ctx, model.KindQEMU, 990000); !errors.Is(err, target.ErrNotOwned) {
			t.Errorf("Destroy = %v, want ErrNotOwned", err)
		}
		if err := f.DestroyRecorded(ctx, model.KindQEMU, 990000, "other-vol"); !errors.Is(err, target.ErrNotOwned) {
			t.Errorf("DestroyRecorded for another backup = %v, want ErrNotOwned", err)
		}
		if err := f.DestroyRecorded(ctx, model.KindQEMU, 990000, "vol"); err != nil {
			t.Errorf("DestroyRecorded = %v", err)
		}
		if len(f.Live) != 0 {
			t.Errorf("live = %v", f.Live)
		}
	})

	t.Run("taken VMID is not started", func(t *testing.T) {
		f := New(990000, 990002)
		f.OwnedGuests = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 990000}}}
		if err := f.Restore(ctx, b, 990000, nil); !errors.Is(err, target.ErrRestoreNotStarted) {
			t.Errorf("Restore onto existing guest = %v, want ErrRestoreNotStarted", err)
		}
	})

	t.Run("prepare records the recorded set", func(t *testing.T) {
		f := New(990000, 990002)
		if err := f.Prepare(ctx, map[int]target.Recorded{990001: {Kind: model.KindQEMU, VolID: "vol"}}); err != nil {
			t.Fatal(err)
		}
		if f.Recorded[990001].VolID != "vol" || len(f.Recorded) != 1 {
			t.Errorf("recorded = %v", f.Recorded)
		}
	})
}
