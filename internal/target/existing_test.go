package target

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx/helperfake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target/bridgetest"
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

// isolatedBridge answers the helper's bridge request as an isolated bridge
// with IPv6 disabled, no addresses and no members; mods change the reply.
func isolatedBridge(mods ...func(*nodeproto.BridgeState)) *helperfake.Fake {
	return bridgetest.Isolated(bridge, mods...)
}

// newTarget builds an ExistingNode with fast polling and registers the safety
// invariant: no mutating call may ever target a VMID outside the range.
func newTarget(t *testing.T, api pve.API, fake *pvefake.Fake, ssh sshx.Runner) *ExistingNode {
	t.Helper()
	if ssh == nil {
		// The node reports the bridge kind the API shows.
		ovs := fake.Ifaces[bridge].Type == "OVSBridge"
		ssh = isolatedBridge(func(st *nodeproto.BridgeState) { st.OVS = ovs })
	}
	n := NewExistingNode(testConfig(), api, sshx.Helper{R: ssh})
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

// leftover seeds an untagged qemu guest at vmid with cfg, and vmVol's
// original config.
func leftover(vmid int, status string, cfg map[string]string) func(*pvefake.Fake) {
	return func(f *pvefake.Fake) {
		f.BackupConfigs[vmVol] = vmConf
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, vmid, status)}
		f.Configs[vmid] = cfg
	}
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
		{name: "guests are not Prepare's concern", setup: func(f *pvefake.Fake) {
			f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990003, "stopped")}
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

// The API shows configuration only. Prepare also checks the bridge's live
// state on the node, as the helper's bridge request reports it: IPv6
// disabled (no link-local address a guest could use to reach the node, no
// SLAAC from a restored router), no addresses at all, and no members but
// proof-guest interfaces. These are the cases the per-command SSH checks had,
// with the same verdicts, plus the helper-only ones.
func TestPrepareLiveIsolation(t *testing.T) {
	ctx := context.Background()
	members := func(names ...string) func(*nodeproto.BridgeState) {
		return func(st *nodeproto.BridgeState) { st.Members = append([]string{}, names...) }
	}
	addrs := func(js string) func(*nodeproto.BridgeState) {
		return func(st *nodeproto.BridgeState) { st.Addrs = json.RawMessage(js) }
	}
	fail := func(reason string) func(nodeproto.Request) sshx.Result {
		return func(req nodeproto.Request) sshx.Result { return helperfake.Fail(req, reason) }
	}
	tests := []struct {
		name     string
		ovs      bool // the API and the node report an OVS bridge
		mod      func(*nodeproto.BridgeState)
		reply    func(nodeproto.Request) sshx.Result // replaces the helper's reply
		sshErr   error
		wantErr  bool
		isolated bool // the error is ErrNotIsolated
		timeout  bool // the error is a timeout
		text     string
	}{
		{name: "isolated, no members"},
		{name: "proof guest members", mod: members("tap990000i0", "veth990001i0", "fwpr990002p0", "fwln990002i0")},
		{name: "empty member output", mod: members()},
		{name: "ipv6 enabled", mod: func(st *nodeproto.BridgeState) { st.DisableIPv6 = "0" },
			wantErr: true, isolated: true, text: "IPv6"},
		{name: "ipv6 state unreadable", reply: fail("bridge pbpiso0: read disable_ipv6: open /proc/sys/net/ipv6/conf/pbpiso0/disable_ipv6: No such file or directory"),
			wantErr: true, isolated: true, text: "cannot be verified"},
		{name: "link-local address", mod: addrs(`[{"ifname":"` + bridge + `","addr_info":[{"family":"inet6","local":"fe80::1","prefixlen":64,"scope":"link"}]}]`),
			wantErr: true, isolated: true, text: "fe80::1"},
		{name: "ipv4 address", mod: addrs(`[{"ifname":"` + bridge + `","addr_info":[{"family":"inet","local":"10.9.9.1","prefixlen":24}]}]`),
			wantErr: true, isolated: true, text: "10.9.9.1"},
		{name: "physical uplink", mod: members("tap990000i0", "eno1"),
			wantErr: true, isolated: true, text: "eno1"},
		{name: "bond uplink", mod: members("bond0"),
			wantErr: true, isolated: true, text: "bond0"},
		{name: "vlan uplink", mod: members("eno1.20"),
			wantErr: true, isolated: true, text: "eno1.20"},
		{name: "production guest plugged in", mod: members("tap100i0"),
			wantErr: true, isolated: true, text: "tap100i0"},
		{name: "unparseable member list", reply: fail("bridge pbpiso0: parse ip link output: invalid character 'g' looking for beginning of value"),
			wantErr: true, isolated: true},
		{name: "ip fails", reply: fail("bridge pbpiso0: /usr/bin/ip -j addr show dev pbpiso0 exited 1: Device does not exist"),
			wantErr: true, isolated: true, text: "cannot be verified"},
		{name: "ssh transport error", sshErr: errors.New("ssh: connection lost"), wantErr: true, text: "connection lost"},
		{name: "ovs isolated", ovs: true},
		// On OVS, PVE plugs a NIC with firewall=1 in as the internal port
		// fwln<vmid>o<n> (PVE::Network compute_fwbr_names); without the
		// firewall it is the plain tap.
		{name: "ovs proof guest ports", ovs: true, mod: members("tap990000i0", "fwln990001o0", "fwln990002o1")},
		{name: "ovs production guest firewall port", ovs: true, mod: members("fwln100o0"),
			wantErr: true, isolated: true, text: "fwln100o0"},
		{name: "ovs firewall port out of range", ovs: true, mod: members("fwln990050o0"),
			wantErr: true, isolated: true, text: "fwln990050o0"},
		{name: "ovs internal port", ovs: true, mod: members("tap990000i0", "mgmt0"),
			wantErr: true, isolated: true, text: "mgmt0"},

		// Helper-only cases.
		{name: "unparseable address list", mod: addrs(`{"addr_info":[]}`),
			wantErr: true, isolated: true, text: "cannot be verified"},
		{name: "helper reports another bridge", mod: func(st *nodeproto.BridgeState) { st.Name = "vmbr0" },
			wantErr: true, isolated: true, text: "vmbr0"},
		{name: "api says ovs, node says linux", ovs: true, mod: func(st *nodeproto.BridgeState) { st.OVS = false },
			wantErr: true, isolated: true, text: "OVS"},
		{name: "api says linux, node says ovs", mod: func(st *nodeproto.BridgeState) { st.OVS = true },
			wantErr: true, isolated: true, text: "OVS"},
		{name: "helper refuses", reply: func(req nodeproto.Request) sshx.Result { return helperfake.Refuse(req, "bad-request", "nope") },
			wantErr: true, isolated: true, text: "nope"},
		{name: "unusable helper reply", reply: func(nodeproto.Request) sshx.Result { return sshx.Result{Stdout: `{"name":"pbpiso0"}`} },
			wantErr: true, isolated: true, text: "cannot be verified"},
		{name: "helper timeout", reply: helperfake.Timeout,
			wantErr: true, timeout: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			if tc.ovs {
				f.Ifaces[bridge] = pve.NetIface{Iface: bridge, Type: "OVSBridge"}
			}
			var mods []func(*nodeproto.BridgeState)
			if tc.ovs {
				mods = append(mods, func(st *nodeproto.BridgeState) { st.OVS = true })
			}
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			ssh := isolatedBridge(mods...)
			if tc.reply != nil {
				ssh.Handler = func(req nodeproto.Request, _ []byte) (sshx.Result, error) { return tc.reply(req), nil }
			}
			if tc.sshErr != nil {
				ssh.Handler = func(nodeproto.Request, []byte) (sshx.Result, error) { return sshx.Result{}, tc.sshErr }
			}
			err := newTarget(t, f, f, ssh).Prepare(ctx)
			if ops := ssh.Ops(); !slices.Equal(ops, []string{"bridge"}) {
				t.Errorf("helper requests = %q, want one bridge request", ops)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Prepare succeeded, want error")
			}
			if errors.Is(err, ErrNotIsolated) != tc.isolated {
				t.Errorf("err = %v, ErrNotIsolated = %v, want %v", err, errors.Is(err, ErrNotIsolated), tc.isolated)
			}
			if errors.Is(err, context.DeadlineExceeded) != tc.timeout {
				t.Errorf("err = %v, timeout = %v, want %v", err, errors.Is(err, context.DeadlineExceeded), tc.timeout)
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

// Any untagged guest in the range is foreign, whatever its state or name:
// the untagged leftovers of recorded restores are the janitor's to remove
// (Destroy with a Recorded) before this check runs.
func TestCheckRangeClear(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		guests []pve.GuestRef
		text   string // "" = clear
	}{
		{name: "empty"},
		{name: "tagged in range and untagged outside", guests: []pve.GuestRef{
			guest(model.KindQEMU, 990001, "stopped", "pbp"), guest(model.KindLXC, 990002, "running", "web", "pbp"),
			guest(model.KindQEMU, 100, "running"), guest(model.KindQEMU, 990050, "stopped")}},
		{name: "untagged stopped guest", text: "qemu/990003", guests: []pve.GuestRef{guest(model.KindQEMU, 990003, "stopped")}},
		{name: "every untagged guest named", text: "qemu/990000, lxc/990049", guests: []pve.GuestRef{
			guest(model.KindQEMU, 990000, "running"), guest(model.KindQEMU, 990001, "stopped", "pbp"),
			guest(model.KindLXC, 990049, "stopped", "web")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			f.GuestList = tc.guests
			err := newTarget(t, f, f, nil).CheckRangeClear(ctx)
			if tc.text == "" {
				if err != nil {
					t.Fatalf("CheckRangeClear: %v", err)
				}
			} else if !errors.Is(err, ErrForeignInRange) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want ErrForeignInRange naming %s", err, tc.text)
			}
			if n := countMutating(f.CallLog()); n != 0 {
				t.Errorf("CheckRangeClear made %d mutating calls", n)
			}
		})
	}
	f := healthyFake()
	f.Fail["Guests"] = errors.New("boom")
	if err := newTarget(t, f, f, nil).CheckRangeClear(ctx); err == nil || errors.Is(err, ErrForeignInRange) {
		t.Errorf("list failure: err = %v", err)
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
			n := NewExistingNode(cfg, f, sshx.Helper{R: &helperfake.Fake{}})
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

// origTags is the tags of the backup's original config, as the run passes
// them to Restore.
func origTags(f *pvefake.Fake, volid string) string {
	return sanitize.ParseConfigText(f.BackupConfigs[volid])["tags"]
}

func vmBackup() model.Backup {
	return model.Backup{Storage: "PBS", VolID: vmVol, Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101},
		Time: time.Date(2026, 7, 26, 6, 0, 3, 0, time.UTC)}
}

func TestRestoreTagsOwnershipBeforeSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	cfg := f.Configs[990000]
	if cfg["tags"] != "pbp" {
		t.Errorf("tags = %q, want pbp", cfg["tags"])
	}
	if cfg["name"] != "pbp-101-202607260600" {
		t.Errorf("name = %q", cfg["name"])
	}
	// The backup's onboot=1 is cleared in the first write, before Sanitize,
	// so a copy left by a failed sanitize never autostarts (code review C1).
	if cfg["onboot"] != "0" {
		t.Errorf("onboot = %q, want 0", cfg["onboot"])
	}
	calls := f.CallLog()
	want := []string{
		"Restore pve1 qemu 990000 " + vmVol + " " + store,
		"WaitTask pve1 UPID:pve1:fake:restore:990000:",
		"SetGuestConfig pve1 qemu 990000 set=map[name:pbp-101-202607260600 onboot:0 protection:0 tags:pbp] del=[]", // tags come from the backup's config: no read first
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q", calls)
	}
	// A later sanitize failure still leaves a guest Destroy accepts.
	f.Fail["SetGuestConfig"] = errors.New("boom")
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); err == nil {
		t.Fatal("Sanitize should fail")
	}
	delete(f.Fail, "SetGuestConfig")
	if err := n.Destroy(ctx, model.KindQEMU, 990000, Recorded{}); err != nil {
		t.Fatalf("Destroy after failed sanitize: %v", err)
	}
	if len(f.GuestList) != 0 {
		t.Errorf("guest not destroyed: %+v", f.GuestList)
	}
}

// A backup of a protected guest restores protected. Restore clears it in the
// same call that adds the tag, so a failed sanitize still leaves a guest
// Destroy can remove.
func TestRestoreClearsProtection(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf + "protection: 1\n"
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	if got := f.Configs[990000]["protection"]; got != "0" {
		t.Errorf("protection after restore = %q", got)
	}
	want := "SetGuestConfig pve1 qemu 990000 set=map[name:pbp-101-202607260600 onboot:0 protection:0 tags:pbp] del=[]"
	if got := f.CallLog(); slices.Index(got, want) != len(got)-1 {
		t.Errorf("want one SetGuestConfig as the last call, got %q", got)
	}
	f.Fail["SetGuestConfig"] = errors.New("boom")
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); err == nil {
		t.Fatal("Sanitize should fail")
	}
	delete(f.Fail, "SetGuestConfig")
	if err := n.Destroy(ctx, model.KindQEMU, 990000, Recorded{}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

// protectionKeepingAPI simulates a node where clearing protection did not
// take effect.
type protectionKeepingAPI struct{ *pvefake.Fake }

func (a protectionKeepingAPI) SetGuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error {
	kept := map[string]string{}
	for k, v := range set {
		if k != "protection" {
			kept[k] = v
		}
	}
	return a.Fake.SetGuestConfig(ctx, node, kind, vmid, kept, del)
}

func TestSanitizeVerifiesProtectionCleared(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf + "protection: 1\n"
	n := newTarget(t, protectionKeepingAPI{f}, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000)
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "protection") {
		t.Fatalf("err = %v, want protection verification failure", err)
	}
}

func TestRestoreFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("out of range", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 101, origTags(f, vmVol))
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
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 990000, origTags(f, vmVol))
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
		if errors.Is(err, ErrRestoreNotStarted) {
			t.Error("a failed restore task may have left a guest; it must not say none was created")
		}
	})
	t.Run("refused request created no guest", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		f.Fail["Restore"] = &pve.APIError{Status: 500, Path: "/nodes/pve1/qemu", Message: "unable to create VM 990000 - VM 990000 already exists"}
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 990000, origTags(f, vmVol))
		if !errors.Is(err, ErrRestoreNotStarted) {
			t.Fatalf("err = %v, want ErrRestoreNotStarted", err)
		}
		if err.Error() != "restore "+vmVol+" to 990000: pve api /nodes/pve1/qemu: 500: unable to create VM 990000 - VM 990000 already exists" {
			t.Errorf("message changed: %q", err)
		}
		if ClassifyRestoreError(err) != model.Error {
			t.Errorf("classified %q", ClassifyRestoreError(err))
		}
	})
	// A transport failure may hide an accepted request: a guest may exist.
	t.Run("lost request may have created a guest", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		f.Fail["Restore"] = errors.New("read tcp: connection reset by peer")
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 990000, origTags(f, vmVol))
		if err == nil || errors.Is(err, ErrRestoreNotStarted) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("out of range is not started", func(t *testing.T) {
		f := healthyFake()
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 100, "")
		if !errors.Is(err, ErrRestoreNotStarted) || !errors.Is(err, ErrOutOfRange) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("failed tag write leaves a guest", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[vmVol] = vmConf
		f.Fail["SetGuestConfig"] = errors.New("403 permission denied")
		err := newTarget(t, f, f, nil).Restore(ctx, vmBackup(), 990000, origTags(f, vmVol))
		if err == nil || errors.Is(err, ErrRestoreNotStarted) {
			t.Errorf("err = %v", err)
		}
	})
}

// The owner tag is added to the restored guest's tags, never replacing them:
// removing a registered tag needs Sys.Modify on "/" (pve1: a 403 for
// "development"), which the token must not have.
func TestRestoreKeepsOriginalTags(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf + "tags: development;web\n"
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	if got := f.Configs[990000]["tags"]; got != "development;web;pbp" {
		t.Errorf("tags after restore = %q", got)
	}
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); err != nil {
		t.Fatal(err)
	}
	if got := f.Configs[990000]["tags"]; got != "development;web;pbp" {
		t.Errorf("tags after sanitize = %q", got)
	}
}

func TestSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	san, err := n.Sanitize(ctx, model.KindQEMU, 990000)
	missing := san.Removed
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
	// Only what Sanitize removed: pieces the backup excludes (scsi1,
	// backup=0) are the caller's to add from the original config.
	want := []model.MissingPiece{{Kind: "passthrough", Key: "hostpci0"}}
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
	if err := n.Restore(ctx, b, 990001, origTags(f, ctVol)); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Configs[990001]["hostname"]; !ok || f.Configs[990001]["hostname"] != "web" {
		t.Errorf("LXC hostname must not be renamed: %q", f.Configs[990001]["hostname"])
	}
	san, err := n.Sanitize(ctx, model.KindLXC, 990001)
	missing := san.Removed
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
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000)
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
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000)
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

// A NIC still carrying an MTU fails verification even on the right bridge.
func TestSanitizeVerifiesMTUCleared(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = strings.Replace(vmConf, "bridge=vmbr0,tag=2", "bridge="+bridge+",mtu=1500", 1)
	n := newTarget(t, bridgeIgnoringAPI{f}, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	_, err := n.Sanitize(ctx, model.KindQEMU, 990000)
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "net0") {
		t.Fatalf("err = %v, want net0 verification failure", err)
	}
}

func TestStartAfterSanitize(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); err != nil {
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
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 100, Recorded{})
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
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, Recorded{})
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
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, Recorded{})
		if !errors.Is(err, ErrNotOwned) || countMutating(f.CallLog()) != 0 {
			t.Errorf("err = %v calls = %q", err, f.CallLog())
		}
	})
	t.Run("absent is a no-op", func(t *testing.T) {
		f := healthyFake()
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990004, Recorded{}); err != nil {
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
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990005, Recorded{}); err != nil {
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
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindLXC, 990006, Recorded{}); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.CallLog() {
			if strings.HasPrefix(c, "Stop ") || strings.HasPrefix(c, "Shutdown ") {
				t.Errorf("unexpected %q", c)
			}
		}
	})
	t.Run("protected owned guest is unprotected then destroyed", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990008, "running", "pbp")}
		f.Configs[990008] = map[string]string{"tags": "pbp", "protection": "1"}
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990008, Recorded{}); err != nil {
			t.Fatal(err)
		}
		var muts []string
		for _, c := range f.CallLog() {
			if slices.Contains(mutating, strings.Fields(c)[0]) {
				muts = append(muts, c)
			}
		}
		want := []string{"SetGuestConfig pve1 qemu 990008 set=map[protection:0] del=[]", "Stop pve1 qemu 990008", "Destroy pve1 qemu 990008"}
		if !slices.Equal(muts, want) {
			t.Errorf("mutating calls = %q", muts)
		}
		if len(f.GuestList) != 0 {
			t.Error("guest not destroyed")
		}
	})
	t.Run("protected untagged guest is left alone", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990009, "stopped")}
		f.Configs[990009] = map[string]string{"protection": "1"}
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990009, Recorded{})
		if !errors.Is(err, ErrNotOwned) || countMutating(f.CallLog()) != 0 || f.Configs[990009]["protection"] != "1" {
			t.Errorf("err = %v calls = %q", err, f.CallLog())
		}
	})
	t.Run("still present after destroy is an error", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990007, "stopped", "pbp")}
		f.Configs[990007] = map[string]string{"tags": "pbp"}
		api := undeadAPI{f}
		if err := newTarget(t, api, f, nil).Destroy(ctx, model.KindQEMU, 990007, Recorded{}); err == nil {
			t.Error("Destroy reported success while guest still exists")
		}
	})
}

// Destroy with a Recorded (the state store records an in-flight restore at
// the VMID) also removes an untagged guest (a restore that failed before
// tagging), but only a stopped one (PBP starts nothing it has not tagged)
// whose name/hostname matches the backup's original config or that is still
// locked by the restore. Range, kind and node checks still apply.
func TestDestroyRecorded(t *testing.T) {
	rec := Recorded{VolID: vmVol}
	ctx := context.Background()
	destroyed := func(t *testing.T, f *pvefake.Fake, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(f.GuestList) != 0 {
			t.Error("guest not destroyed")
		}
	}
	refused := func(t *testing.T, f *pvefake.Fake, err error, text string) {
		t.Helper()
		if !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), text) {
			t.Errorf("err = %v, want ErrNotOwned mentioning %q", err, text)
		}
		if countMutating(f.CallLog()) != 0 || len(f.GuestList) != 1 {
			t.Errorf("SAFETY: guest touched: calls = %q", f.CallLog())
		}
	}
	t.Run("untagged stopped guest matching name destroyed", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"name": "delta1", "protection": "1"})(f)
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec))
	})
	t.Run("untagged container matching hostname destroyed", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[ctVol] = ctConf
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990003, "stopped")}
		f.Configs[990003] = map[string]string{"hostname": "web"}
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindLXC, 990003, Recorded{VolID: ctVol}))
	})
	t.Run("untagged guest locked by restore destroyed", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"lock": "create"})(f)
		delete(f.BackupConfigs, vmVol) // the lock alone suffices
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec))
	})
	t.Run("untagged guest with another name refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"name": "someone-elses"})(f)
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "does not match")
	})
	t.Run("untagged guest without a name refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{})(f)
		f.BackupConfigs[vmVol] = "memory: 512\n" // the backup has no name either, and no NIC
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "does not match")
	})
	// A backup without a name: the restore kept its NICs' MAC addresses, so
	// the same set identifies the leftover (code review 1.6b); without that
	// the janitor could never clear it.
	t.Run("nameless leftover with the backup's MACs destroyed", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{
			"net0": "virtio=BC:24:11:ED:57:35,bridge=vmbr0", "net1": "e1000=bc:24:11:00:00:02,bridge=vmbr1"})(f)
		f.BackupConfigs[vmVol] = "memory: 512\nnet0: virtio=BC:24:11:ED:57:35,bridge=vmbr0,tag=2\nnet1: e1000=BC:24:11:00:00:02,bridge=vmbr1\n"
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec))
	})
	t.Run("nameless leftover with other MACs refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"net0": "virtio=BC:24:11:AA:AA:AA,bridge=vmbr0"})(f)
		f.BackupConfigs[vmVol] = "memory: 512\nnet0: virtio=BC:24:11:ED:57:35,bridge=vmbr0\n"
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "MAC")
	})
	t.Run("unreadable original config refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"name": "delta1"})(f)
		f.Fail["OriginalConfig"] = errors.New("pbs unreachable")
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "cannot be read")
	})
	t.Run("untagged running guest refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "running", map[string]string{"name": "delta1"})(f)
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "running")
	})
	t.Run("tagged running guest destroyed", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990004, "running", "pbp")}
		f.Configs[990004] = map[string]string{"tags": "pbp"}
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindLXC, 990004, Recorded{VolID: ctVol}))
	})
	t.Run("outside range refused without any call", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 100, "stopped")}
		f.Configs[100] = map[string]string{}
		err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 100, rec)
		if !errors.Is(err, ErrOutOfRange) || len(f.CallLog()) != 0 {
			t.Errorf("err = %v calls = %q", err, f.CallLog())
		}
	})
	t.Run("kind mismatch refused", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990003, "stopped")}
		f.Configs[990003] = map[string]string{"lock": "create"}
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "expected qemu")
	})
	t.Run("other node refused", func(t *testing.T) {
		f := healthyFake()
		g := guest(model.KindQEMU, 990003, "stopped")
		g.Node = "pve2"
		f.GuestList = []pve.GuestRef{g}
		f.Configs[990003] = map[string]string{"lock": "create"}
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, rec), "pve2")
	})
	t.Run("absent is a no-op", func(t *testing.T) {
		f := healthyFake()
		if err := newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990004, rec); err != nil || countMutating(f.CallLog()) != 0 {
			t.Errorf("err = %v calls = %q", err, f.CallLog())
		}
	})
	// A caller that has the backup's original config passes the name, and
	// Destroy does not read the config again.
	t.Run("caller's name matching destroyed without reading the backup", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"name": "delta1"})(f)
		f.Fail["OriginalConfig"] = errors.New("must not be read")
		destroyed(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, Recorded{VolID: vmVol, Name: "delta1"}))
		if slices.ContainsFunc(f.CallLog(), func(c string) bool { return strings.HasPrefix(c, "OriginalConfig") }) {
			t.Errorf("original config read: %q", f.CallLog())
		}
	})
	t.Run("caller's name not matching refused", func(t *testing.T) {
		f := healthyFake()
		leftover(990003, "stopped", map[string]string{"name": "delta1"})(f)
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindQEMU, 990003, Recorded{VolID: vmVol, Name: "web"}), "does not match")
	})
	t.Run("container compared by hostname, not name", func(t *testing.T) {
		f := healthyFake()
		f.BackupConfigs[ctVol] = ctConf
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990003, "stopped")}
		f.Configs[990003] = map[string]string{"name": "web", "hostname": "other"}
		refused(t, f, newTarget(t, f, f, nil).Destroy(ctx, model.KindLXC, 990003, Recorded{VolID: ctVol}), "does not match")
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
		{errors.New("pbs: error trying to connect: tcp connect error"), model.Error},
		{errors.New("dial tcp 10.0.0.5:8007: No route to host"), model.Error},
		{errors.New("network is unreachable"), model.Error},
		{errors.New("read: connection Reset by peer"), model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"write: Broken pipe"}}, model.Error},
		{errors.New("storage 'pbs-main' is not online"), model.Error},
		// Only known backup damage is FAILED; anything else is the node's
		// or PBP's (ERROR), never blamed on the backup (code review I6).
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"Insufficient free space in volume group"}}, model.Error},
		{errors.New("Cannot allocate memory"), model.Error},
		{errors.New("pve api /nodes/pve1/qemu: 502: Bad Gateway"), model.Error},
		{errors.New("pve api /nodes/pve1/qemu: 503: Service Unavailable"), model.Error},
		{errors.New("pve api /nodes/pve1/qemu: 400: Parameter verification failed"), model.Error},
		{errors.New("unexpected EOF"), model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "mount point type bind is only allowed for root@pam"}, model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "unable to restore"}, model.Error},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"Detected chunk with wrong digest."}}, model.Failed},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{`restore failed: reading file "/mnt/datastore/main/.chunks/3f2a/3f2a9c" failed: No such file or directory (os error 2)`}}, model.Failed},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{"unable to load chunk '3f2a9c' - fetch failed"}}, model.Failed},
		{&pve.TaskError{UPID: "u", ExitStatus: "error", Log: []string{`reading file "/mnt/datastore/main/.chunks/3f2a/3f2a9c" failed: Permission denied (os error 13)`}}, model.Error},
		{errors.New("checksum mismatch in index file"), model.Failed},
		{errors.New("failed to decode blob"), model.Failed},
		{errors.New("unable to parse archive header: invalid magic"), model.Failed},
	}
	for _, tc := range tests {
		if got := ClassifyRestoreError(tc.err); got != tc.want {
			t.Errorf("ClassifyRestoreError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestStrayTagged(t *testing.T) {
	f := healthyFake()
	f.GuestList = []pve.GuestRef{
		guest(model.KindQEMU, 990000, "stopped", "pbp"),     // in range: owned, not stray
		guest(model.KindQEMU, 105, "running", "web", "pbp"), // stray
		guest(model.KindLXC, 210, "running", "pbp"),         // stray
		guest(model.KindQEMU, 106, "running", "web"),        // untagged, outside: fine
		guest(model.KindQEMU, 990050, "stopped", "pbp"),     // just above range: stray
	}
	other := guest(model.KindLXC, 990007, "stopped", "pbp") // in range, other node: stray (1.7)
	other.Node = "pve2"
	f.GuestList = append(f.GuestList, other)
	owned, err := newTarget(t, f, f, nil).Owned(context.Background())
	if err != nil || len(owned) != 1 || owned[0].Key.VMID != 990000 {
		t.Errorf("owned = %v (%v), want only 990000: a guest on another node is not the janitor's", owned, err)
	}
	got, err := newTarget(t, f, f, nil).StrayTagged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, g := range got {
		ids = append(ids, g.Key.VMID)
	}
	if !slices.Equal(ids, []int{105, 210, 990050, 990007}) {
		t.Errorf("stray = %v", ids)
	}
	if n := countMutating(f.CallLog()); n != 0 {
		t.Errorf("StrayTagged made %d mutating calls", n)
	}
	f.Fail["Guests"] = errors.New("boom")
	if _, err := newTarget(t, f, f, nil).StrayTagged(context.Background()); err == nil {
		t.Error("want error")
	}
}

func TestSanitizeReportsChanges(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = "name: delta1\nmemory: 2048\nagent: 1\nscsi0: local-lvm:vm-101-disk-0,size=32G\nnet0: virtio=AA:BB,bridge=vmbr0,tag=2\nonboot: 1\nusb0: host=1-1\n"
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	got, err := n.Sanitize(ctx, model.KindQEMU, 990000)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"net0: virtio=AA:BB,bridge=vmbr0,tag=2 \u2192 virtio=AA:BB,bridge=" + bridge,
		// onboot is already 0: the restore overrode it (code review C1).
		"usb0 removed (host=1-1)",
	}
	if !slices.Equal(got.Changes, want) {
		t.Errorf("Changes = %q, want %q", got.Changes, want)
	}
}

// When the config cannot be re-read after sanitizing, neither Sanitize nor
// Start lets the guest run: PBP cannot show it is safe to start (code
// review I20).
func TestSanitizeReReadFails(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	f.BackupConfigs[vmVol] = vmConf
	n := newTarget(t, f, f, nil)
	if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	sanitized := false
	f.FailOn = func(method string, args []string) error {
		switch {
		case method == "SetGuestConfig" && strings.Contains(strings.Join(args, " "), "net0"):
			sanitized = true
		case method == "GuestConfig" && sanitized:
			return boom
		}
		return nil
	}
	if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); !errors.Is(err, boom) || !strings.Contains(err.Error(), "re-read config") {
		t.Fatalf("Sanitize: %v, want the re-read error", err)
	}
	if err := n.Start(ctx, model.KindQEMU, 990000); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("Start: %v, want refusal", err)
	}
	if s, _ := f.GuestStatus(ctx, node, model.KindQEMU, 990000); s == "running" {
		t.Error("guest started")
	}
}

// The copy may only boot from its own volumes: a disk naming another
// VMID's volume (a production disk) or a host path refuses the start, a
// CD-ROM does not (code review 1.9).
func TestStartRefusesForeignDisks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		key, val string
		ok       bool
	}{
		{"scsi0", "local-lvm:vm-101-disk-0,size=32G", false},
		{"virtio1", "/dev/disk/by-id/ata-PROD", false},
		{"efidisk0", "local-lvm:vm-990001-disk-0,efitype=4m", false},
		{"ide2", "nfs:iso/win.iso,media=cdrom", true},
		{"ide3", "none,media=cdrom", true},
		{"scsi2", "local-lvm:base-101-disk-0/vm-990000-disk-3,size=8G", true},
		{"sata0", "local:990000/vm-990000-disk-4.qcow2,size=8G", true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			f := healthyFake()
			f.BackupConfigs[vmVol] = vmConf
			n := newTarget(t, f, f, nil)
			if err := n.Restore(ctx, vmBackup(), 990000, origTags(f, vmVol)); err != nil {
				t.Fatal(err)
			}
			if _, err := n.Sanitize(ctx, model.KindQEMU, 990000); err != nil {
				t.Fatal(err)
			}
			f.Configs[990000][tc.key] = tc.val
			err := n.Start(ctx, model.KindQEMU, 990000)
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok && (!errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), tc.key)) {
				t.Errorf("err = %v, want ErrUnsafe naming %s", err, tc.key)
			}
		})
	}
}

func TestForeignDiskContainer(t *testing.T) {
	for v, want := range map[string]bool{
		"local-zfs:subvol-990000-disk-0,size=8G": true,
		"local-zfs:subvol-201-disk-0,size=8G":    false,
		"/srv/share,mp=/mnt/share":               false,
	} {
		if got := foreignDisk("mp0", v, 990000) == ""; got != want {
			t.Errorf("%s: own = %v, want %v", v, got, want)
		}
	}
	if foreignDisk("net0", "/whatever", 990000) != "" || foreignDisk("unused0", "local:vm-1-disk-0", 990000) != "" {
		t.Error("non-disk keys judged")
	}
}
