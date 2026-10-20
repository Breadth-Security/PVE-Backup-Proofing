package nodesetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
)

// checkRig fakes pvesh, sshd and the node helper, and records the probes in
// the order they run.
type checkRig struct {
	version   string
	nodes     string
	iface     string // JSON of the bridge; "" means the bridge does not exist
	guests    string
	storages  map[string]bool // storage -> status call succeeds
	sshd      string
	bridge    nodeproto.BridgeState
	bridgeErr error
	calls     []string
	root      string // has sudo, visudo and /etc/sudoers.d
}

// sudoRoot is a temp root with sudo installed.
func sudoRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "etc/sudoers.d"), 0o750)
	mustWrite(t, filepath.Join(root, "usr/bin/sudo"), "", 0o755)
	mustWrite(t, filepath.Join(root, "usr/sbin/visudo"), "", 0o755)
	return root
}

type rigSystem struct {
	node.System
	r *checkRig
}

func (s rigSystem) BridgeState(_ context.Context, b string) (nodeproto.BridgeState, error) {
	s.r.calls = append(s.r.calls, "bridgestate")
	return s.r.bridge, s.r.bridgeErr
}

func newRig(t *testing.T) *checkRig {
	return &checkRig{
		root:     sudoRoot(t),
		version:  `{"version":"9.0.3"}`,
		nodes:    `[{"node":"pve1"},{"node":"pve2"}]`,
		iface:    `{"iface":"vmbr99","type":"bridge","bridge_ports":"none"}`,
		guests:   `[{"type":"qemu","vmid":990001,"node":"pve1","name":"a","status":"running","tags":"pbp"},{"type":"qemu","vmid":100,"node":"pve1","name":"prod","status":"running"}]`,
		storages: map[string]bool{"local-zfs": true},
		sshd:     "port 22\nusepam yes\npermitrootlogin no\n",
		bridge:   nodeproto.BridgeState{Name: "vmbr99", DisableIPv6: "1", Addrs: []byte(`[]`), Members: []string{"tap990001i0"}},
	}
}

func (r *checkRig) env() (Env, *pve.Client) {
	pvesh := func(_ context.Context, _ string, args []string) ([]byte, error) {
		path := args[1]
		r.calls = append(r.calls, path)
		switch {
		case path == "/version":
			return []byte(r.version), nil
		case path == "/nodes":
			return []byte(r.nodes), nil
		case strings.HasPrefix(path, "/nodes/pve1/network/"):
			if r.iface == "" {
				return nil, errors.New("exit status 2: 400 Parameter verification failed.\niface: interface does not exist")
			}
			return []byte(r.iface), nil
		case path == "/cluster/resources":
			return []byte(r.guests), nil
		case strings.HasSuffix(path, "/status") && strings.Contains(path, "/storage/"):
			st := strings.Split(path, "/")[4]
			if r.storages[st] {
				return []byte(`{"active":1,"enabled":1}`), nil
			}
			return nil, errors.New("storage '" + st + "' does not exist")
		}
		return nil, errors.New("unexpected path " + path)
	}
	env := Env{Root: r.root, Run: func(_ context.Context, name string, args []string, _ []byte) (string, string, int, error) {
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		return r.sshd, "", 0, nil
	}}
	return env, pve.NewLocal("/usr/bin/pvesh", pvesh)
}

func (r *checkRig) check(a nodeproto.SetupArgs) (string, error) {
	hostname = func() (string, error) { return "pve1.example.lan", nil }
	env, api := r.env()
	return Check(context.Background(), env, api, rigSystem{r: r}, a, "0.3.0")
}

func checkArgs() nodeproto.SetupArgs {
	a := testArgs()
	a.DaemonVersion = "0.3.5"
	a.Storages = []string{"local-zfs"}
	return a
}

func TestCheckPasses(t *testing.T) {
	r := newRig(t)
	n, err := r.check(checkArgs())
	if err != nil || n != "pve1" {
		t.Fatalf("Check = %q, %v", n, err)
	}
}

func TestCheckFailures(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*checkRig, *nodeproto.SetupArgs)
		want string
		last string // the last probe that may have run
	}{
		{"old pve", func(r *checkRig, _ *nodeproto.SetupArgs) { r.version = `{"version":"8.4.1"}` }, "Proxmox VE 9", "/version"},
		{"no daemon version", func(_ *checkRig, a *nodeproto.SetupArgs) { a.DaemonVersion = "" }, "missing --daemon-version; copy it again from pbp setup", "/version"},
		{"incompatible", func(_ *checkRig, a *nodeproto.SetupArgs) { a.DaemonVersion = "0.4.0" }, "this node has pbp 0.3.0, the PBP host runs 0.4.0: install pbp 0.4.0 on the node first", "/version"},
		{"bad key", func(_ *checkRig, a *nodeproto.SetupArgs) { a.PubKey = "nope" }, "public key", "/version"},
		{"node not listed", func(r *checkRig, _ *nodeproto.SetupArgs) { r.nodes = `[{"node":"other"}]` }, `"pve1" is not in the cluster node list`, "/nodes"},
		{"not isolated", func(r *checkRig, _ *nodeproto.SetupArgs) {
			r.iface = `{"iface":"vmbr99","type":"bridge","bridge_ports":"eno1"}`
		}, "vmbr99 has ports eno1", "/nodes/pve1/network/vmbr99"},
		{"ipv6 on", func(r *checkRig, _ *nodeproto.SetupArgs) { r.bridge.DisableIPv6 = "0" }, "post-up echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6", "bridgestate"},
		{"unverifiable", func(r *checkRig, _ *nodeproto.SetupArgs) { r.bridge.Name = "other" }, "cannot be verified", "bridgestate"},
		{"bridge state error", func(r *checkRig, _ *nodeproto.SetupArgs) { r.bridgeErr = errors.New("boom") }, "boom", "bridgestate"},
		{"foreign guest", func(r *checkRig, _ *nodeproto.SetupArgs) {
			r.guests = `[{"type":"qemu","vmid":990002,"node":"pve1","name":"x","status":"stopped"}]`
		}, "990002", "/cluster/resources"},
		{"no usepam", func(r *checkRig, _ *nodeproto.SetupArgs) { r.sshd = "port 22\nusepam no\n" }, "UsePAM", "/usr/sbin/sshd -T -C user=pbp,host=localhost,addr=127.0.0.1"},
		{"storage", func(r *checkRig, _ *nodeproto.SetupArgs) { r.storages = nil }, "storage local-zfs", "/nodes/pve1/storage/local-zfs/status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			a := checkArgs()
			tc.mod(r, &a)
			_, err := r.check(a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if got := r.calls[len(r.calls)-1]; got != tc.last {
				t.Errorf("last probe = %q, want %q (calls %v)", got, tc.last, r.calls)
			}
		})
	}
}

func TestCheckMissingBridgePrintsStanza(t *testing.T) {
	r := newRig(t)
	r.iface = ""
	_, err := r.check(checkArgs())
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{BridgeStanza("vmbr99"), "Datacenter → pve1 → System → Network → Create → Linux Bridge (no ports, no IPv4/IPv6)",
		"post-up line", "ifreload -a", "pbp node setup again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if got := r.calls[len(r.calls)-1]; got != "/nodes/pve1/network/vmbr99" {
		t.Errorf("last probe = %q", got)
	}
}

func TestCheckIPv6HintAndOtherIfaceErrors(t *testing.T) {
	r := newRig(t)
	r.bridge.DisableIPv6 = "0"
	_, err := r.check(checkArgs())
	if err == nil || !strings.Contains(err.Error(), "ifreload -a") {
		t.Errorf("ipv6 error lacks the reload hint: %v", err)
	}
}

func TestBridgeStanza(t *testing.T) {
	want := "auto vmbr7\niface vmbr7 inet manual\n        bridge-ports none\n        bridge-stp off\n        bridge-fd 0\n        post-up echo 1 > /proc/sys/net/ipv6/conf/vmbr7/disable_ipv6\n"
	if got := BridgeStanza("vmbr7"); got != want {
		t.Errorf("stanza = %q", got)
	}
}

const realMissingStderr = "400 Parameter verification failed.\niface: interface does not exist\npvesh get <api_path> [OPTIONS] [FORMAT_OPTIONS]\n"

// pveshEnv drives Check through the production wiring PveshRun, with fail
// deciding what a pvesh network GET does.
func pveshEnv(r *checkRig, fail func() (string, int, error)) (Env, *pve.Client) {
	env := Env{Root: r.root}
	env.Run = func(_ context.Context, name string, args []string, _ []byte) (string, string, int, error) {
		if name == "/usr/sbin/sshd" {
			return r.sshd, "", 0, nil
		}
		path := args[1]
		switch {
		case path == "/version":
			return r.version, "", 0, nil
		case path == "/nodes":
			return r.nodes, "", 0, nil
		case strings.Contains(path, "/network/"):
			stderr, exit, err := fail()
			return "", stderr, exit, err
		}
		return "", "unexpected " + path, 1, nil
	}
	return env, pve.NewLocal("/usr/bin/pvesh", PveshRun(env))
}

func TestCheckMissingBridge(t *testing.T) {
	hostname = func() (string, error) { return "pve1", nil }
	r := newRig(t)
	env, api := pveshEnv(r, func() (string, int, error) { return realMissingStderr, 255, nil })
	_, err := Check(context.Background(), env, api, rigSystem{r: r}, checkArgs(), "0.3.0")
	if err == nil || !strings.Contains(err.Error(), BridgeStanza("vmbr99")) {
		t.Fatalf("want the stanza, got %v", err)
	}

	env, api = pveshEnv(r, func() (string, int, error) {
		return "", 0, errors.New(`exec: "/usr/bin/pvesh": executable file not found in $PATH`)
	})
	_, err = Check(context.Background(), env, api, rigSystem{r: r}, checkArgs(), "0.3.0")
	if err == nil || strings.Contains(err.Error(), BridgeStanza("vmbr99")) || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("exec failure must not look like a missing bridge: %v", err)
	}
}

func TestCheckSudoMissing(t *testing.T) {
	for _, p := range []string{"usr/bin/sudo", "usr/sbin/visudo", "etc/sudoers.d"} {
		t.Run(p, func(t *testing.T) {
			r := newRig(t)
			if err := os.RemoveAll(filepath.Join(r.root, p)); err != nil {
				t.Fatal(err)
			}
			_, err := r.check(checkArgs())
			if err == nil || !strings.Contains(err.Error(), "apt install sudo") || !strings.Contains(err.Error(), "/"+p) {
				t.Fatalf("err = %v, want the sudo prerequisite naming /%s", err, p)
			}
			if got := r.calls[len(r.calls)-1]; got != "/version" {
				t.Errorf("probes ran after the sudo check: %v", r.calls)
			}
		})
	}
	// /etc/sudoers.d must be a directory.
	r := newRig(t)
	if err := os.RemoveAll(filepath.Join(r.root, "etc/sudoers.d")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(r.root, "etc/sudoers.d"), "", 0o644)
	if _, err := r.check(checkArgs()); err == nil || !strings.Contains(err.Error(), "/etc/sudoers.d") {
		t.Fatalf("err = %v, want /etc/sudoers.d named", err)
	}
}

func TestCheckSshdArgs(t *testing.T) {
	r := newRig(t)
	if _, err := r.check(checkArgs()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "/usr/sbin/sshd -T -C user=pbp,host=localhost,addr=127.0.0.1") {
		t.Errorf("sshd not asked for pbp's connection: %v", r.calls)
	}
	// UsePAM set off in a Match block for pbp shows in -C output.
	r.sshd = "usepam no\n"
	if _, err := r.check(checkArgs()); err == nil || !strings.Contains(err.Error(), "UsePAM") {
		t.Errorf("err = %v", err)
	}
}

func TestDaemonWarning(t *testing.T) {
	root := t.TempDir()
	var exit int
	var runErr error
	env := Env{Root: root, Run: func(_ context.Context, name string, args []string, _ []byte) (string, string, int, error) {
		if name != "/usr/bin/systemctl" || strings.Join(args, " ") != "is-active --quiet pbp.service" {
			t.Errorf("ran %s %q", name, args)
		}
		return "", "", exit, runErr
	}}
	exit = 3
	if w := DaemonWarning(context.Background(), env); w != "" {
		t.Errorf("inactive, no config: %q", w)
	}
	runErr = errors.New("no systemctl")
	exit = 0
	if w := DaemonWarning(context.Background(), env); w != "" {
		t.Errorf("systemctl missing: %q", w)
	}
	runErr = nil
	if w := DaemonWarning(context.Background(), env); !strings.Contains(w, "pbp.service is active") || !strings.Contains(w, "separate PBP host") {
		t.Errorf("active: %q", w)
	}
	exit = 3
	mustWrite(t, filepath.Join(root, "etc/pbp/config.yaml"), "x\n", 0o640)
	if w := DaemonWarning(context.Background(), env); !strings.Contains(w, "/etc/pbp/config.yaml exists") || strings.Contains(w, "active") {
		t.Errorf("config: %q", w)
	}
}

// PveshRun bounds stderr at 512 bytes without cutting a UTF-8 character.
func TestPveshRunBoundsStderr(t *testing.T) {
	stderr := strings.Repeat("a", 511) + "é" + strings.Repeat("b", 100)
	env := Env{Run: func(context.Context, string, []string, []byte) (string, string, int, error) {
		return "", stderr, 255, nil
	}}
	_, err := PveshRun(env)(context.Background(), "/usr/bin/pvesh", []string{"get", "/x"})
	if err == nil {
		t.Fatal("no error")
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("error is not valid UTF-8: %q", err.Error()[len(err.Error())-8:])
	}
	if !strings.HasSuffix(err.Error(), strings.Repeat("a", 511)) {
		t.Errorf("error does not end at the cut: %q", err.Error()[len(err.Error())-8:])
	}
}
