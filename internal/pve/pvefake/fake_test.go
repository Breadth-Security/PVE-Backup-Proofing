package pvefake

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
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
	// onboot=0 is the restore override the real client sends.
	want := map[string]string{"name": "delta1", "cores": "4", "net0": "virtio=BC:24:11:ED:57:35,bridge=vmbr0", "onboot": "0"}
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
	if _, err := f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch"); err != nil {
		t.Fatal(err)
	}
	status := func() string { s, _ := f.GuestStatus(ctx, "pve1", model.KindQEMU, 9100); return s }
	if _, err := f.Start(ctx, "pve1", model.KindQEMU, 9100); err != nil {
		t.Fatal(err)
	}
	if status() != "running" {
		t.Errorf("after Start: %q", status())
	}
	if _, err := f.Shutdown(ctx, "pve1", model.KindQEMU, 9100); err != nil {
		t.Fatal(err)
	}
	if status() != "stopped" {
		t.Errorf("after Shutdown: %q", status())
	}
	if _, err := f.Start(ctx, "pve1", model.KindQEMU, 9100); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stop(ctx, "pve1", model.KindQEMU, 9100); err != nil {
		t.Fatal(err)
	}
	if status() != "stopped" {
		t.Errorf("after Stop: %q", status())
	}
	if err := f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 9100, map[string]string{"onboot": "0"}, []string{"cores"}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch"); err != nil {
		t.Fatal(err)
	}
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
			if _, err := f.Restore(ctx, "pve1", model.KindQEMU, vmid, volid, "scratch"); err != nil {
				t.Error(err)
			}
			if _, err := f.Start(ctx, "pve1", model.KindQEMU, vmid); err != nil {
				t.Error(err)
			}
			if _, err := f.Stop(ctx, "pve1", model.KindQEMU, vmid); err != nil {
				t.Error(err)
			}
			if _, err := f.Destroy(ctx, "pve1", model.KindQEMU, vmid); err != nil {
				t.Error(err)
			}
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

// Like PVE, a protected guest cannot be destroyed.
func TestDestroyRefusesProtected(t *testing.T) {
	f := New()
	f.GuestList = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 990000}, Status: "stopped"}}
	f.Configs[990000] = map[string]string{"protection": "1"}
	if _, err := f.Destroy(context.Background(), "pve1", model.KindQEMU, 990000); err == nil {
		t.Fatal("destroyed a protected guest")
	}
	if len(f.GuestList) != 1 {
		t.Fatal("guest removed")
	}
}

// Like the real API, restoring a volid that was never seeded fails and creates nothing.
func TestRestoreUnseededVolid(t *testing.T) {
	f := New()
	if _, err := f.Restore(context.Background(), "pve1", model.KindQEMU, 9100, volid, "scratch"); err == nil {
		t.Fatal("Restore of an unseeded volid succeeded")
	}
	if len(f.GuestList) != 0 || len(f.Configs) != 0 {
		t.Errorf("failed Restore left state: %+v %v", f.GuestList, f.Configs)
	}
}

func TestSetGuestConfigRecordsArguments(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	if _, err := f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch"); err != nil {
		t.Fatal(err)
	}
	f.ResetCalls()
	if err := f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 9100, map[string]string{"b": "2", "a": "1"}, []string{"cores"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"SetGuestConfig pve1 qemu 9100 set=map[a:1 b:2] del=[cores]"}
	if got := f.CallLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// A guest seeded in GuestList without a Configs entry has an empty config, not "not found".
func TestSeededGuestWithoutConfig(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.GuestList = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 100}, Status: "stopped"}}
	cfg, err := f.GuestConfig(ctx, "pve1", model.KindQEMU, 100)
	if err != nil || len(cfg) != 0 {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if err := f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 100, map[string]string{"tags": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.GuestConfig(ctx, "pve1", model.KindQEMU, 100); cfg["tags"] != "x" {
		t.Errorf("cfg = %v", cfg)
	}
	if _, err := f.GuestConfig(ctx, "pve1", model.KindQEMU, 101); err == nil {
		t.Error("unlisted guest should not be found")
	}
	if err := f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 101, nil, nil); err == nil {
		t.Error("SetGuestConfig on an unlisted guest should fail")
	}
}

func TestOriginalConfig(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	if got, err := f.OriginalConfig(ctx, "pve1", volid); err != nil || got != f.BackupConfigs[volid] {
		t.Errorf("got %q %v", got, err)
	}
	if _, err := f.OriginalConfig(ctx, "pve1", "nope"); err == nil {
		t.Error("unseeded volid should fail")
	}
}

func TestAgentMethods(t *testing.T) {
	ctx := context.Background()
	f := New()
	if err := f.AgentPing(ctx, "pve1", 100); err != nil {
		t.Error(err)
	}
	if o, err := f.AgentOSInfo(ctx, "pve1", 100); err != nil || o.ID != "debian" {
		t.Errorf("os = %+v %v", o, err)
	}
	pid, err := f.AgentExec(ctx, "pve1", 100, []string{"echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := f.AgentExecStatus(ctx, "pve1", 100, pid); err != nil || !st.Exited {
		t.Errorf("status = %+v %v", st, err)
	}
	want := []string{"AgentPing pve1 100", "AgentOSInfo pve1 100", "AgentExec pve1 100 echo hi", "AgentExecStatus pve1 100 1"}
	if got := f.CallLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}
}

// Fail applies to every method, not just Start.
func TestFailOnOtherMethods(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	f := seeded()
	f.GuestList = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 100}, Status: "stopped"}}
	calls := map[string]func() error{
		"Version":         func() error { _, err := f.Version(ctx); return err },
		"ListBackups":     func() error { _, err := f.ListBackups(ctx, "pve1", "PBS"); return err },
		"VolumeNotes":     func() error { _, err := f.VolumeNotes(ctx, "pve1", "PBS", volid); return err },
		"SetVolumeAttrs":  func() error { return f.SetVolumeAttrs(ctx, "pve1", "PBS", volid, nil, nil) },
		"StorageStatus":   func() error { _, err := f.StorageStatus(ctx, "pve1", "scratch"); return err },
		"NodeMemory":      func() error { _, err := f.NodeMemory(ctx, "pve1"); return err },
		"NetIface":        func() error { _, err := f.NetIface(ctx, "pve1", "vmbr0"); return err },
		"WaitTask":        func() error { return f.WaitTask(ctx, "pve1", "u") },
		"Guests":          func() error { _, err := f.Guests(ctx); return err },
		"Restore":         func() error { _, err := f.Restore(ctx, "pve1", model.KindQEMU, 9100, volid, "scratch"); return err },
		"GuestConfig":     func() error { _, err := f.GuestConfig(ctx, "pve1", model.KindQEMU, 100); return err },
		"SetGuestConfig":  func() error { return f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 100, nil, nil) },
		"OriginalConfig":  func() error { _, err := f.OriginalConfig(ctx, "pve1", volid); return err },
		"Start":           func() error { _, err := f.Start(ctx, "pve1", model.KindQEMU, 100); return err },
		"Shutdown":        func() error { _, err := f.Shutdown(ctx, "pve1", model.KindQEMU, 100); return err },
		"Stop":            func() error { _, err := f.Stop(ctx, "pve1", model.KindQEMU, 100); return err },
		"Destroy":         func() error { _, err := f.Destroy(ctx, "pve1", model.KindQEMU, 100); return err },
		"GuestStatus":     func() error { _, err := f.GuestStatus(ctx, "pve1", model.KindQEMU, 100); return err },
		"AgentPing":       func() error { return f.AgentPing(ctx, "pve1", 100) },
		"AgentOSInfo":     func() error { _, err := f.AgentOSInfo(ctx, "pve1", 100); return err },
		"AgentExec":       func() error { _, err := f.AgentExec(ctx, "pve1", 100, nil); return err },
		"AgentExecStatus": func() error { _, err := f.AgentExecStatus(ctx, "pve1", 100, 1); return err },
	}
	for name, call := range calls {
		f.Fail[name] = boom
		if err := call(); err != boom {
			t.Errorf("%s: err = %v, want injected failure", name, err)
		}
		delete(f.Fail, name)
	}
	if len(f.GuestList) != 1 {
		t.Errorf("failed calls changed state: %+v", f.GuestList)
	}
}

// SetVolumeAttrs logs the guest the backup belongs to, "unknown" for an
// unseeded volid, which fails like on PVE.
func TestSetVolumeAttrsLogsGuest(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.Backups["PBS"] = []model.Backup{{Storage: "PBS", VolID: volid, Key: model.GuestKey{Kind: model.KindQEMU, VMID: 100}}}
	if err := f.SetVolumeAttrs(ctx, "pve1", "PBS", volid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.SetVolumeAttrs(ctx, "pve1", "PBS", "PBS:nope", nil, nil); err == nil {
		t.Fatal("unknown volid accepted")
	}
	want := []string{"SetVolumeAttrs pve1 PBS " + volid + " guest=qemu/100", "SetVolumeAttrs pve1 PBS PBS:nope guest=unknown"}
	if got := f.CallLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}
}

// Like PVE: a running guest cannot be destroyed, calls naming another node
// fail, the listing shows the config's tags and name, and FailOn can fail
// one particular call (code review I20).
func TestBehavesLikePVE(t *testing.T) {
	ctx := context.Background()
	f := seeded()
	if _, err := f.Restore(ctx, "pve1", model.KindQEMU, 9000, volid, "scratch"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetGuestConfig(ctx, "pve1", model.KindQEMU, 9000, map[string]string{"tags": "pbp;web", "name": "pbp-9000"}, nil); err != nil {
		t.Fatal(err)
	}
	g, _ := f.Guests(ctx)
	if i := slices.IndexFunc(g, func(r pve.GuestRef) bool { return r.Key.VMID == 9000 }); i < 0 ||
		!slices.Equal(g[i].Tags, []string{"pbp", "web"}) || g[i].Name != "pbp-9000" {
		t.Errorf("listing: %+v", g)
	}
	if _, err := f.Start(ctx, "pve1", model.KindQEMU, 9000); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Destroy(ctx, "pve1", model.KindQEMU, 9000); err == nil {
		t.Error("destroyed a running guest")
	}
	if _, err := f.Stop(ctx, "pve2", model.KindQEMU, 9000); err == nil {
		t.Error("call on another node accepted")
	}
	f.Node = "pve2"
	if _, err := f.Stop(ctx, "pve2", model.KindQEMU, 9000); err != nil {
		t.Errorf("configured node refused: %v", err)
	}
	boom := errors.New("boom")
	n := 0
	f.FailOn = func(method string, args []string) error {
		if method == "SetGuestConfig" && args[2] == "9000" {
			if n++; n == 2 {
				return boom
			}
		}
		return nil
	}
	if err := f.SetGuestConfig(ctx, "pve2", model.KindQEMU, 9000, map[string]string{"a": "1"}, nil); err != nil {
		t.Errorf("first call: %v", err)
	}
	if err := f.SetGuestConfig(ctx, "pve2", model.KindQEMU, 9000, map[string]string{"a": "2"}, nil); err != boom {
		t.Errorf("second call: %v, want boom", err)
	}
}
