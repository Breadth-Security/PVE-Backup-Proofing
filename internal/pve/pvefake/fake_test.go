package pvefake

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
)

var _ pve.API = (*Fake)(nil)

const volid = "PBS:backup/vm/100/2026-07-26T06:00:03Z"

func seeded() *Fake {
	f := New()
	f.BackupConfigs[volid] = "#a note\nname: delta1\ncores: 4\nnet0: virtio=BC:24:11:ED:57:35,bridge=vmbr0\n\n[PENDING]\ncores: 8\n[snap1]\nname: other\n"
	return f
}

func TestRestoreThenGuestConfig(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	upid, err := f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch")
	if err != nil || upid == "" {
		t.Fatalf("upid=%q err=%v", upid, err)
	}
	cfg, err := f.GuestConfig(ctx, "pve1", model.KindQEMU, 9100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"name": "delta1", "cores": "4", "net0": "virtio=BC:24:11:ED:57:35,bridge=vmbr0"}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg = %v", cfg)
	}
	g, _ := f.Guests(ctx)
	if len(g) != 1 || g[0].Node != "pve1" || g[0].Key.VMID != 9100 || g[0].Name != "delta1" {
		t.Errorf("guests = %+v", g)
	}
	if err := f.WaitTask(ctx, "pve1", upid); err != nil {
		t.Error(err)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch")
	status := func() string { s, _ := f.GuestStatus(ctx, "pve1", model.KindQEMU, 9100); return s }
	f.Start(ctx, "pve1", model.KindQEMU, 9100)
	if status() != "running" {
		t.Errorf("after Start: %q", status())
	}
	f.Shutdown(ctx, "pve1", model.KindQEMU, 9100)
	if status() != "stopped" {
		t.Errorf("after Shutdown: %q", status())
	}
	f.Start(ctx, "pve1", model.KindQEMU, 9100)
	f.Stop(ctx, "pve1", model.KindQEMU, 9100)
	if status() != "stopped" {
		t.Errorf("after Stop: %q", status())
	}
	f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 9100, map[string]string{"onboot": "0"}, []string{"cores"})
	cfg, _ := f.GuestConfig(ctx, "pve1", model.KindQEMU, 9100)
	if cfg["onboot"] != "0" || cfg["cores"] != "" {
		t.Errorf("cfg = %v", cfg)
	}
	if _, err := f.Destroy(ctx, "pve1", model.KindQEMU, 9100); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.Guests(ctx); len(g) != 0 {
		t.Errorf("guests after Destroy = %+v", g)
	}
	if _, err := f.GuestStatus(ctx, "pve1", model.KindQEMU, 9100); err == nil {
		t.Error("status of destroyed guest should fail")
	}
}

func TestFailInjectionAndCalls(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	boom := errors.New("boom")
	f.Fail["Start"] = boom
	f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch")
	if _, err := f.Start(ctx, "pve1", model.KindQEMU, 9100); err != boom {
		t.Errorf("Start err = %v", err)
	}
	want := []string{"Restore pve1 qemu 9100 " + volid + " scratch", "Start pve1 qemu 9100"}
	if got := f.CallLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}
}

func TestSeededReads(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.Backups["PBS"] = []model.Backup{{Storage: "PBS", VolID: volid}}
	f.Storage["scratch"] = pve.StorageStatus{Active: true, Avail: 5}
	f.Memory = pve.NodeMemory{Total: 10, Available: 9}
	f.Ifaces["vmbr0"] = pve.NetIface{Iface: "vmbr0"}
	f.VersionString = "9.9.9"
	if b, _ := f.ListBackups(ctx, "pve1", "PBS"); len(b) != 1 {
		t.Error(b)
	}
	if s, _ := f.StorageStatus(ctx, "pve1", "scratch"); s.Avail != 5 {
		t.Error(s)
	}
	if m, _ := f.NodeMemory(ctx, "pve1"); m.Available != 9 {
		t.Error(m)
	}
	if n, _ := f.NetIface(ctx, "pve1", "vmbr0"); n.Iface != "vmbr0" {
		t.Error(n)
	}
	if v, _ := f.Version(ctx); v != "9.9.9" {
		t.Error(v)
	}
	if _, err := f.StorageStatus(ctx, "pve1", "nope"); err == nil {
		t.Error("unknown storage should fail")
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(vmid int) {
			defer wg.Done()
			f.Restore(ctx, "pve1", model.KindQEMU, vmid, volid, "scratch")
			f.Start(ctx, "pve1", model.KindQEMU, vmid)
			f.Destroy(ctx, "pve1", model.KindQEMU, vmid)
		}(9000 + i)
	}
	wg.Wait()
	if g, _ := f.Guests(ctx); len(g) != 0 {
		t.Errorf("leftover guests: %+v", g)
	}
}

// Like PBS, the listing shows only the first line of the notes; VolumeNotes
// returns them in full.
func TestNotesFirstLineInListing(t *testing.T) {
	f := New()
	f.Backups["pbs"] = []model.Backup{{Storage: "pbs", VolID: "pbs:backup/ct/1/x", Notes: "first\nsecond"}}
	bs, _ := f.ListBackups(context.Background(), "pve1", "pbs")
	if bs[0].Notes != "first" {
		t.Errorf("listed notes %q", bs[0].Notes)
	}
	if n, err := f.VolumeNotes(context.Background(), "pve1", "pbs", "pbs:backup/ct/1/x"); err != nil || n != "first\nsecond" {
		t.Errorf("VolumeNotes %q %v", n, err)
	}
	if f.Backups["pbs"][0].Notes != "first\nsecond" {
		t.Error("listing modified the seeded notes")
	}
}
