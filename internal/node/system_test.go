package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
)

type cannedCmd struct {
	stdout, stderr string
	exit           int
	err            error
}

// cannedRunner answers commands (joined with spaces) from a table and
// records them. Nothing is executed.
type cannedRunner struct {
	answers map[string]cannedCmd
	calls   []string
	stdins  []string
}

func (c *cannedRunner) run(ctx context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	c.calls = append(c.calls, cmd)
	c.stdins = append(c.stdins, string(stdin))
	a, ok := c.answers[cmd]
	if !ok {
		return "", "", -1, errors.New("unexpected command " + cmd)
	}
	return a.stdout, a.stderr, a.exit, a.err
}

func procWith(t *testing.T, bridge, disableIPv6 string) string {
	t.Helper()
	root := t.TempDir()
	if disableIPv6 != "" {
		d := filepath.Join(root, "sys/net/ipv6/conf", bridge)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "disable_ipv6"), []byte(disableIPv6), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const addrJSON = `[{"ifindex":7,"ifname":"vmbr99","flags":["BROADCAST","UP"],"addr_info":[]}]`

func TestRealBridgeStateLinux(t *testing.T) {
	r := &cannedRunner{answers: map[string]cannedCmd{
		"ovs-vsctl --timeout=10 br-exists vmbr99": {exit: 2},
		"ip -j addr show dev vmbr99":              {stdout: addrJSON + "\n"},
		"ip -j link show master vmbr99":           {stdout: `[{"ifname":"veth990001i0"},{"ifname":"fwpr990002p0"}]`},
	}}
	s := &RealSystem{ProcRoot: procWith(t, "vmbr99", "1\n"), run: r.run}
	got, err := s.BridgeState(context.Background(), "vmbr99")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "vmbr99" || got.OVS || got.DisableIPv6 != "1" || string(got.Addrs) != addrJSON ||
		strings.Join(got.Members, ",") != "veth990001i0,fwpr990002p0" {
		t.Errorf("got %+v (addrs %s)", got, got.Addrs)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"addrs":[{"ifindex":7`) {
		t.Errorf("json %s", b)
	}
}

func TestRealBridgeStateLinuxNoMembers(t *testing.T) {
	r := &cannedRunner{answers: map[string]cannedCmd{
		"ovs-vsctl --timeout=10 br-exists vmbr99": {exit: 2},
		"ip -j addr show dev vmbr99":              {stdout: addrJSON},
		"ip -j link show master vmbr99":           {stdout: "\n"},
	}}
	s := &RealSystem{ProcRoot: procWith(t, "vmbr99", "0"), run: r.run}
	got, err := s.BridgeState(context.Background(), "vmbr99")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisableIPv6 != "0" || got.Members == nil || len(got.Members) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestRealBridgeStateOVS(t *testing.T) {
	r := &cannedRunner{answers: map[string]cannedCmd{
		"ovs-vsctl --timeout=10 br-exists vmbr99":  {exit: 0},
		"ip -j addr show dev vmbr99":               {stdout: addrJSON},
		"ovs-vsctl --timeout=10 list-ports vmbr99": {stdout: "fwln990001o0\ntap990002i0\n"},
	}}
	s := &RealSystem{ProcRoot: procWith(t, "vmbr99", "1"), run: r.run}
	got, err := s.BridgeState(context.Background(), "vmbr99")
	if err != nil {
		t.Fatal(err)
	}
	if !got.OVS || strings.Join(got.Members, ",") != "fwln990001o0,tap990002i0" {
		t.Errorf("got %+v", got)
	}
}

func TestRealBridgeStateOVSNotInstalled(t *testing.T) {
	r := &cannedRunner{answers: map[string]cannedCmd{
		"ovs-vsctl --timeout=10 br-exists vmbr99": {exit: -1, err: errNotFound},
		"ip -j addr show dev vmbr99":              {stdout: addrJSON},
		"ip -j link show master vmbr99":           {stdout: "[]"},
	}}
	s := &RealSystem{ProcRoot: procWith(t, "vmbr99", "1"), run: r.run}
	got, err := s.BridgeState(context.Background(), "vmbr99")
	if err != nil {
		t.Fatal(err)
	}
	if got.OVS {
		t.Error("OVS without ovs-vsctl")
	}
}

func TestRealBridgeStateErrors(t *testing.T) {
	base := func() map[string]cannedCmd {
		return map[string]cannedCmd{
			"ovs-vsctl --timeout=10 br-exists vmbr99": {exit: 2},
			"ip -j addr show dev vmbr99":              {stdout: addrJSON},
			"ip -j link show master vmbr99":           {stdout: "[]"},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(m map[string]cannedCmd)
		ipv6   string
	}{
		{"missing disable_ipv6", func(m map[string]cannedCmd) {}, ""},
		{"ovs undecidable", func(m map[string]cannedCmd) {
			m["ovs-vsctl --timeout=10 br-exists vmbr99"] = cannedCmd{exit: 1, stderr: "database connection failed"}
		}, "1"},
		{"ip addr fails", func(m map[string]cannedCmd) {
			m["ip -j addr show dev vmbr99"] = cannedCmd{exit: 1, stderr: `Device "vmbr99" does not exist.`}
		}, "1"},
		{"ip addr not json", func(m map[string]cannedCmd) {
			m["ip -j addr show dev vmbr99"] = cannedCmd{stdout: "inet 10.0.0.1"}
		}, "1"},
		{"ip link not json", func(m map[string]cannedCmd) {
			m["ip -j link show master vmbr99"] = cannedCmd{stdout: "veth1"}
		}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			s := &RealSystem{ProcRoot: procWith(t, "vmbr99", tc.ipv6), run: (&cannedRunner{answers: m}).run}
			if _, err := s.BridgeState(context.Background(), "vmbr99"); err == nil {
				t.Error("no error")
			}
		})
	}
}

func TestRealPctConfig(t *testing.T) {
	r := &cannedRunner{answers: map[string]cannedCmd{
		"pct config 990001": {stdout: "arch: amd64\nhostname: proof-1\ntags: keep;pbp\n"},
		"pct config 990002": {exit: 2, stderr: "Configuration file 'nodes/pve1/lxc/990002.conf' does not exist\n"},
	}}
	s := &RealSystem{run: r.run}
	c, err := s.PctConfig(context.Background(), 990001)
	if err != nil || c["tags"] != "keep;pbp" || c["hostname"] != "proof-1" {
		t.Errorf("got %v, %v", c, err)
	}
	if _, err := s.PctConfig(context.Background(), 990002); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err %v", err)
	}
}

func TestRealNotify(t *testing.T) {
	payload := `{"severity":"info","data":{},"fields":{"type":"pbp"}}`
	ok := &cannedRunner{answers: map[string]cannedCmd{"perl -e " + notifyScript: {}}}
	s := &RealSystem{run: ok.run}
	if err := s.Notify(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if ok.stdins[0] != payload {
		t.Errorf("stdin %q", ok.stdins[0])
	}

	for _, a := range []cannedCmd{
		{exit: 2, stderr: "Can't locate PVE/Notify.pm"},
		{stderr: "some log\nERROR: could not notify via target `mail-to-root`: template failed\n"},
	} {
		bad := &cannedRunner{answers: map[string]cannedCmd{"perl -e " + notifyScript: a}}
		s := &RealSystem{run: bad.run}
		if err := s.Notify(context.Background(), []byte(payload)); err == nil {
			t.Errorf("%+v: no error", a)
		}
	}
}

func TestRealTemplateHashes(t *testing.T) {
	dir := t.TempDir()
	tmpl := report.Templates()
	var name string
	for n := range tmpl {
		name = n
		break
	}
	if err := os.WriteFile(filepath.Join(dir, name), tmpl[name], 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (&RealSystem{TemplateDir: dir}).TemplateHashes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(tmpl) {
		t.Fatalf("got %v", got)
	}
	sum := sha256.Sum256(tmpl[name])
	for n := range tmpl {
		want := ""
		if n == name {
			want = hex.EncodeToString(sum[:])
		}
		if got[n] != want {
			t.Errorf("%s: got %q, want %q", n, got[n], want)
		}
	}
}

func TestNewSystemDefaults(t *testing.T) {
	s := NewSystem()
	if s.TemplateDir != DefaultTemplateDir || s.ProcRoot != "/proc" || s.run == nil {
		t.Errorf("got %+v", s)
	}
}

func TestPctExecArgs(t *testing.T) {
	got := pctExecArgs(990001, []string{"sh", "-c 'id'; reboot", "--"})
	want := []string{"exec", "990001", "--", "sh", "-c 'id'; reboot", "--"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
}

func TestStreamPassesThrough(t *testing.T) {
	var out, errb strings.Builder
	exit, err := stream(context.Background(), "/bin/sh", []string{"-c", "cat; echo err >&2; exit 7"}, strings.NewReader("in"), &out, &errb)
	if err != nil || exit != 7 || out.String() != "in" || errb.String() != "err\n" {
		t.Errorf("exit %d err %v out %q stderr %q", exit, err, out.String(), errb.String())
	}
}

func TestStreamNotFound(t *testing.T) {
	_, err := stream(context.Background(), "/nonexistent/pbp-test-binary", nil, nil, io.Discard, io.Discard)
	if err == nil {
		t.Error("no error")
	}
}

// Review Focus 3: on ctx done the whole process group dies, including a
// background grandchild that does not hold the output pipes.
func TestStreamKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := stream(ctx, "/bin/sh", []string{"-c", "sleep 30 >/dev/null 2>&1 & echo $! > " + pidFile + "; wait"}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Error("killed command returned no error")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v", el)
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for i := 0; ; i++ {
		// A killed orphan may linger briefly as a zombie until reaped.
		st, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if len(st) == 0 || strings.Contains(string(st), ") Z ") {
			break
		}
		if i == 100 {
			t.Fatalf("grandchild %d still alive: %s", pid, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
