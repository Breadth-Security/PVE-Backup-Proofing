package target

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

const (
	lo, hi = 990000, 990049
	bridge = "pbpiso0"
	store  = "scratch"
	node   = "pve1"
)

var mutating = []string{"Restore", "SetGuestConfig", "Start", "Stop", "Shutdown", "Destroy"}

func testConfig() config.Config {
	return config.Config{
		Target:  config.Target{Node: node, Bridge: bridge, VMIDRange: [2]int{lo, hi}},
		Restore: config.Restore{Storage: store},
	}
}

// healthyFake is a node that passes Prepare.
func healthyFake() *pvefake.Fake {
	f := pvefake.New()
	f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "bridge"}
	f.Storage[store] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,rootdir,iso"}
	return f
}

// newTarget builds an ExistingNode with fast polling and registers the safety
// invariant: no mutating call may ever target a VMID outside the range.
func newTarget(t *testing.T, api pve.API, fake *pvefake.Fake, ssh sshx.Runner) *ExistingNode {
	t.Helper()
	if ssh == nil {
		ssh = &sshx.Fake{}
	}
	n := NewExistingNode(testConfig(), api, ssh)
	n.statusPoll, n.agentPoll, n.lxcExecPoll, n.execPoll = time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { assertNoMutationOutsideRange(t, fake.CallLog()) })
	return n
}

func assertNoMutationOutsideRange(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		f := strings.Fields(c)
		if !slices.Contains(mutating, f[0]) {
			continue
		}
		// Mutating calls are logged as "Method node kind vmid ...".
		if len(f) < 4 {
			t.Errorf("unparseable mutating call %q", c)
			continue
		}
		v, err := strconv.Atoi(f[3])
		if err != nil || v < lo || v > hi {
			t.Errorf("SAFETY: mutating call outside range: %q", c)
		}
	}
}

func countMutating(calls []string) int {
	n := 0
	for _, c := range calls {
		if slices.Contains(mutating, strings.Fields(c)[0]) {
			n++
		}
	}
	return n
}

func guest(kind model.GuestKind, vmid int, status string, tags ...string) pve.GuestRef {
	return pve.GuestRef{Key: model.GuestKey{Kind: kind, VMID: vmid}, Node: node, Status: status, Tags: tags}
}

func TestPrepare(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(f *pvefake.Fake)
		want  error
		text  string
	}{
		{name: "isolated linux bridge", setup: func(f *pvefake.Fake) {}},
		{name: "isolated linux bridge ports none", setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "bridge", BridgePorts: "none"}
		}},
		{name: "isolated ovs bridge", setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "OVSBridge"}
		}},
		{name: "owned guest in range is fine", setup: func(f *pvefake.Fake) {
			f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990001, "stopped", "pbp"), guest(model.KindQEMU, 100, "running")}
		}},
		{name: "real pve1 vmbr0", want: ErrNotIsolated, setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: "vmbr0", Type: "OVSBridge", OVSPorts: "enp66s0f0 ovsport_host ovsport_dev"}
		}},
		{name: "linux bridge with port", want: ErrNotIsolated, setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "bridge", BridgePorts: "eno1"}
		}},
		{name: "bridge with address", want: ErrNotIsolated, setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "bridge", CIDR: "10.9.9.1/24"}
		}},
		{name: "bridge with ipv6", want: ErrNotIsolated, setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "bridge", Address6: "fd00::1"}
		}},
		{name: "not a bridge", want: ErrNotIsolated, setup: func(f *pvefake.Fake) {
			f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "eth"}
		}},
		{name: "pve 8", want: ErrVersion, setup: func(f *pvefake.Fake) { f.VersionString = "8.4.1" }},
		{name: "foreign guest in range", want: ErrForeignInRange, text: "990003", setup: func(f *pvefake.Fake) {
			f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990003, "stopped")}
		}},
		{name: "storage inactive", text: "storage", setup: func(f *pvefake.Fake) {
			f.Storage[store] = pve.StorageStatus{Enabled: true, Content: "images,rootdir"}
		}},
		{name: "storage disabled", text: "storage", setup: func(f *pvefake.Fake) {
			f.Storage[store] = pve.StorageStatus{Active: true, Content: "images,rootdir"}
		}},
		{name: "storage without rootdir", text: "rootdir", setup: func(f *pvefake.Fake) {
			f.Storage[store] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,iso"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			tc.setup(f)
			err := newTarget(t, f, f, nil).Prepare(ctx)
			wantErr := tc.want != nil || tc.text != ""
			if !wantErr {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Prepare succeeded, want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err %q does not mention %q", err, tc.text)
			}
			if n := countMutating(f.CallLog()); n != 0 {
				t.Errorf("Prepare made %d mutating calls", n)
			}
		})
	}
}

func TestOwned(t *testing.T) {
	f := healthyFake()
	f.GuestList = []pve.GuestRef{
		guest(model.KindQEMU, 990000, "stopped", "pbp"),
		guest(model.KindLXC, 990002, "running", "web", "pbp"),
		guest(model.KindQEMU, 990003, "stopped"),
		guest(model.KindQEMU, 100, "running", "pbp"), // tagged but outside range
	}
	got, err := newTarget(t, f, f, nil).Owned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, g := range got {
		ids = append(ids, g.Key.VMID)
	}
	if !slices.Equal(ids, []int{990000, 990002}) {
		t.Errorf("owned = %v", ids)
	}
}

func TestAllocateVMID(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		rng      [2]int
		existing []int
		inUse    []int
		want     int
		wantErr  bool
	}{
		{name: "exhausted", rng: [2]int{990000, 990001}, existing: []int{990000}, inUse: []int{990001}, wantErr: true},
		{name: "lowest free", rng: [2]int{990000, 990005}, existing: []int{990000, 990002}, inUse: []int{990001}, want: 990003},
		{name: "first", rng: [2]int{990000, 990005}, want: 990000},
		{name: "last", rng: [2]int{990000, 990002}, existing: []int{990000, 990001}, want: 990002},
		{name: "guests outside range ignored", rng: [2]int{990000, 990000}, existing: []int{989999, 990001}, want: 990000},
		{name: "single slot taken", rng: [2]int{990000, 990000}, existing: []int{990000}, wantErr: true},
		{name: "inUse outside range ignored", rng: [2]int{990000, 990001}, inUse: []int{989999, 990002}, want: 990000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			for _, v := range tc.existing {
				f.GuestList = append(f.GuestList, guest(model.KindQEMU, v, "stopped", "pbp"))
			}
			inUse := map[int]bool{}
			for _, v := range tc.inUse {
				inUse[v] = true
			}
			cfg := testConfig()
			cfg.Target.VMIDRange = tc.rng
			n := NewExistingNode(cfg, f, &sshx.Fake{})
			t.Cleanup(func() { assertNoMutationOutsideRange(t, f.CallLog()) })
			got, err := n.AllocateVMID(ctx, inUse)
			if tc.wantErr {
				if !errors.Is(err, ErrNoFreeVMID) {
					t.Fatalf("got %d, %v; want ErrNoFreeVMID", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
			if got < tc.rng[0] || got > tc.rng[1] {
				t.Errorf("SAFETY: %d outside range %v", got, tc.rng)
			}
		})
	}
}

const (
	vmVol  = "PBS:backup/vm/101/2026-07-26T06:00:03Z"
	ctVol  = "PBS:backup/ct/201/2026-07-26T06:10:00Z"
	vmConf = "name: delta1\nmemory: 2048\nagent: 1\nhostpci0: 0000:01:00.0\nscsi0: local-lvm:vm-101-disk-0,size=32G\nscsi1: tank:vm-101-disk-1,backup=0,size=1T\nnet0: virtio=BC:24:11:ED:57:35,bridge=vmbr0,tag=2\nonboot: 1\n"
	ctConf = "hostname: web\nostype: debian\nrootfs: local-lvm:subvol-201-disk-0,size=8G\nmp0: /srv/share,mp=/mnt/share\nnet0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp\n"
)

func vmBackup() model.Backup {
	return model.Backup{Storage: "PBS", VolID: vmVol, Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101},
		Time: time.Date(2026, 7, 26, 6, 0, 3, 0, time.UTC)}
}

func TestRestoreTagsOwnershipBeforeSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000); err != nil {
		t.Fatal(err)
	}
	cfg := f.Configs[990000]
	if cfg["tags"] != "pbp" {
		t.Errorf("tags = %q, want pbp", cfg["tags"])
	}
	if cfg["name"] != "pbp-101-202607260600" {
		t.Errorf("name = %q", cfg["name"])
	}
	calls := f.CallLog()
	want := []string{
		"Restore pve1 qemu 990000 " + vmVol + " " + store,
		"WaitTask pve1 UPID:pve1:fake:restore:990000:",
		"SetGuestConfig pve1 qemu 990000",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q", calls)
	}
	// A later sanitize failure still leaves a guest Destroy accepts.
	f.Fail["SetGuestConfig"] = errors.New("boom")
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000, vmVol); err == nil {
		t.Fatal("Sanitize should fail")
	}
	delete(f.Fail, "SetGuestConfig")
	if err := n.Destroy(ctx, model.KindQEMU, 990000); err != nil {
		t.Fatalf("Destroy after failed sanitize: %v", err)
	}
	if len(f.GuestList) != 0 {
		t.Errorf("guest not destroyed: %+v", f.GuestList)
	}
}

func TestRestoreFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("out of range", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 101)
		if !errors.Is(err, ErrOutOfRange) {
			t.Errorf("err = %v", err)
		}
		if len(f.CallLog()) != 0 {
			t.Errorf("calls = %q", f.CallLog())
		}
	})
	t.Run("task failure is returned, not tagged", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		f.Fail["WaitTask"] = &pve.TaskError{UPID: "x", ExitStatus: "error", Log: []string{"decryption failed: missing key"}}
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 990000)
		var te *pve.TaskError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v, want TaskError", err)
		}
		if ClassifyRestoreError(err) != model.Error {
			t.Errorf("classified %q", ClassifyRestoreError(err))
		}
		for _, c := range f.CallLog() {
			if strings.HasPrefix(c, "SetGuestConfig") {
				t.Errorf("tagged after failed restore: %q", c)
			}
		}
	})
}

func TestSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000); err != nil {
		t.Fatal(err)
	}
	missing, err := n.Sanitize(ctx, model.KindQEMU, 990000, vmVol)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.Configs[990000]
	if _, ok := cfg["hostpci0"]; ok {
		t.Error("hostpci0 still present")
	}
	if cfg["net0"] != "virtio=BC:24:11:ED:57:35,bridge="+bridge {
		t.Errorf("net0 = %q", cfg["net0"])
	}
	if cfg["onboot"] != "0" || cfg["tags"] != "pbp" {
		t.Errorf("onboot=%q tags=%q", cfg["onboot"], cfg["tags"])
	}
	want := []model.MissingPiece{
		{Kind: "passthrough", Key: "hostpci0"},
		{Kind: "excluded-disk", Key: "scsi1", Detail: "backup=0"},
	}
	if !slices.Equal(missing, want) {
		t.Errorf("missing = %+v", missing)
	}
}

func TestSanitizeLXCBindMount(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[ctVol] = ctConf
	n := newTarget(t, f, f, nil)
	b := model.Backup{VolID: ctVol, Key: model.GuestKey{Kind: model.KindLXC, VMID: 201}}
	if err := n.Restore(ctx, b, 990001); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Configs[990001]["hostname"]; !ok || f.Configs[990001]["hostname"] != "web" {
		t.Errorf("LXC hostname must not be renamed: %q", f.Configs[990001]["hostname"])
	}
	missing, err := n.Sanitize(ctx, model.KindLXC, 990001, ctVol)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Configs[990001]["mp0"]; ok {
		t.Error("bind mount mp0 still present")
	}
	if len(missing) != 1 || missing[0].Kind != "bind-mount" || missing[0].Path != "/mnt/share" {
		t.Errorf("missing = %+v", missing)
	}
}

func TestSanitizeUnsafe(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf + "args: -device foo\n"
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000, vmVol)
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "args") {
		t.Fatalf("err = %v", err)
	}
	// The guest must not be startable.
	if err := n.Start(ctx, model.KindQEMU, 990000); err == nil {
		t.Error("Start of unsanitized guest succeeded")
	}
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, "Start ") {
			t.Errorf("Start reached the API: %q", c)
		}
	}
}

// bridgeIgnoringAPI simulates a node that silently drops NIC changes.
type bridgeIgnoringAPI struct{ *pvefake.Fake }

func (a bridgeIgnoringAPI) SetGuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error {
	kept := map[string]string{}
	for k, v := range set {
		if !strings.HasPrefix(k, "net") {
			kept[k] = v
		}
	}
	return a.Fake.SetGuestConfig(ctx, node, kind, vmid, kept, del)
}

func TestSanitizeVerifiesBridge(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, bridgeIgnoringAPI{f}, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000, vmVol)
	if err == nil || !strings.Contains(err.Error(), "net0") {
		t.Fatalf("err = %v, want net0 verification failure", err)
	}
	if err := n.Start(ctx, model.KindQEMU, 990000); err == nil {
		t.Error("Start of guest on production bridge succeeded")
	}
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, "Start ") {
			t.Errorf("Start reached the API: %q", c)
		}
	}
}

func TestStartAfterSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000, vmVol); err != nil {
		t.Fatal(err)
	}
	if err := n.Start(ctx, model.KindQEMU, 990000); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.GuestStatus(ctx, node, model.KindQEMU, 990000); s != "running" {
		t.Errorf("status = %q", s)
	}
	if err := n.Start(ctx, model.KindQEMU, 100); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("Start 100: %v", err)
	}
}

func TestDestroy(t *testing.T) {
	ctx := context.Background()
	t.Run("outside range refused without any call", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 100, "running", "pbp")}
		f.Configs[100] = map[string]string{"tags": "pbp"}
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 100)
		if !errors.Is(err, ErrOutOfRange) {
			t.Errorf("err = %v", err)
		}
		if len(f.CallLog()) != 0 {
			t.Errorf("calls = %q", f.CallLog())
		}
	})
	t.Run("untagged in range refused", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990003, "running")}
		f.Configs[990003] = map[string]string{"name": "someone-elses"}
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003)
		if !errors.Is(err, ErrNotOwned) {
			t.Errorf("err = %v", err)
		}
		if n := countMutating(f.CallLog()); n != 0 {
			t.Errorf("mutating calls: %q", f.CallLog())
		}
		if len(f.GuestList) != 1 {
			t.Error("guest destroyed")
		}
	})
	t.Run("kind mismatch refused", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990003, "stopped", "pbp")}
		f.Configs[990003] = map[string]string{"tags": "pbp"}
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003)
		if err == nil || countMutating(f.CallLog()) != 0 {
			t.Errorf("err = %v calls = %q", err, f.CallLog())
		}
	})
	t.Run("absent is a no-op", func(t *testing.T) {
		f := healthyFake()
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990004); err != nil {
			t.Errorf("err = %v", err)
		}
		if n := countMutating(f.CallLog()); n != 0 {
			t.Errorf("mutating calls: %q", f.CallLog())
		}
	})
	t.Run("running owned guest hard-stopped then destroyed", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990005, "running", "pbp")}
		f.Configs[990005] = map[string]string{"tags": "pbp;web"}
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990005); err != nil {
			t.Fatal(err)
		}
		var muts []string
		for _, c := range f.CallLog() {
			if slices.Contains(mutating, strings.Fields(c)[0]) {
				muts = append(muts, c)
			}
		}
		want := []string{"Stop pve1 qemu 990005", "Destroy pve1 qemu 990005"}
		if !slices.Equal(muts, want) {
			t.Errorf("mutating calls = %q", muts)
		}
		calls := f.CallLog()
		if last := calls[len(calls)-1]; last != "Guests" {
			t.Errorf("last call %q, want Guests (absence check)", last)
		}
	})
	t.Run("stopped owned guest destroyed without stop", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990006, "stopped", "pbp")}
		f.Configs[990006] = map[string]string{"tags": "pbp"}
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindLXC, 990006); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.CallLog() {
			if strings.HasPrefix(c, "Stop ") || strings.HasPrefix(c, "Shutdown ") {
				t.Errorf("unexpected %q", c)
			}
		}
	})
	t.Run("still present after destroy is an error", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990007, "stopped", "pbp")}
		f.Configs[990007] = map[string]string{"tags": "pbp"}
		api := undeadAPI{f}
		if err := newTarget(t, api, f, nil).Destroy(ctx, model.KindQEMU, 990007); err == nil {
			t.Error("Destroy reported success while guest still exists")
		}
	})
}

// undeadAPI accepts Destroy but never removes the guest.
type undeadAPI struct{ *pvefake.Fake }

func (undeadAPI) Destroy(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return "UPID:undead", nil
}

func TestClassifyRestoreError(t *testing.T) {
	tests := []struct {
		err  error
		want model.Verdict
	}{
		{nil, ""},
		{errors.New("decryption failed: missing key"), model.Error},
		{errors.New("chunk 3f… corrupted"), model.Failed},
		{errors.New("chunk 3f0a5001c4 corrupted"), model.Failed},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"restore image", "Wrong Encryption Key"}}, model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"unable to read archive: bad chunk digest"}}, model.Failed},
		{errors.New("Permission check failed"), model.Error},
		{errors.New("write failed: No space left on device"), model.Error},
		{errors.New("command timed out"), model.Error},
		{errors.New("got timeout"), model.Error},
		{errors.New("connection refused"), model.Error},
		{errors.New("pve: HTTP 401: no ticket"), model.Error},
		{errors.New("pve: HTTP 403 forbidden"), model.Error},
		{errors.New("pve: 500 internal"), model.Error},
		{context.DeadlineExceeded, model.Error},
		{ErrOutOfRange, model.Error},
		{errors.New("unable to create VM 990000: config file already exists"), model.Error},
		{errors.New("can't lock file '/var/lock/qemu-server/lock-990000.conf' - got timeout"), model.Error},
		{errors.New("CAN'T LOCK storage"), model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"VM 990000 Is Locked (create)"}}, model.Error},
	}
	for _, tc := range tests {
		if got := ClassifyRestoreError(tc.err); got != tc.want {
			t.Errorf("ClassifyRestoreError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
