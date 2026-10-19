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
	if err := f.Restore(ctx, b, v); err != nil {
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
	if _, err := f.Sanitize(ctx, model.KindQEMU, 990002, "vol"); !errors.Is(err, target.ErrUnsafe) {
		t.Errorf("sanitize err = %v", err)
	}
	if _, err := f.AllocateVMID(ctx, map[int]bool{990002: true}); !errors.Is(err, target.ErrNoFreeVMID) {
		t.Errorf("want ErrNoFreeVMID, got %v", err)
	}
	f.Destroy(ctx, model.KindQEMU, v)
	f.Destroy(ctx, model.KindQEMU, 990000)
	if len(f.Live) != 0 || len(f.OwnedGuests) != 0 {
		t.Errorf("live = %v owned = %v", f.Live, f.OwnedGuests)
	}
	want := []string{"AllocateVMID", "Restore 990001 vol", "WaitBooted qemu 990001", "Executor qemu 990001",
		"Executor qemu 990002", "Sanitize qemu 990002 vol", "AllocateVMID", "Destroy qemu 990001", "Destroy qemu 990000"}
	if got := f.CallLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %q", got)
	}
}

func TestZeroValueRestore(t *testing.T) {
	f := &Fake{Range: [2]int{1, 1}}
	if err := f.Restore(context.Background(), model.Backup{}, 1); err != nil {
		t.Fatal(err)
	}
}
