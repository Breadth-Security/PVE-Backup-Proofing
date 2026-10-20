package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodesetup"
)

const fakeSecret = "3f1c9a2e-5b7d-4e8f-a1b2-c3d4e5f60718"

type fakeACL struct {
	Path      string `json:"path"`
	RoleID    string `json:"roleid"`
	Type      string `json:"type"`
	UGID      string `json:"ugid"`
	Propagate int    `json:"propagate"`
}

// fakeNode is a Proxmox node on a temp root: pvesh, sshd, pveum, visudo,
// usermod and the helper probes through sudo, all from in-memory state.
type fakeNode struct {
	t       *testing.T
	root    string
	host    string
	shell   string
	roles   map[string]string
	users   []string
	tokens  map[string][]string
	acls    []fakeACL
	events  []string // every non-query command, in order
	failACL string   // `acl modify` on this path fails
	onProbe func()   // runs on each self-test probe (after Apply)
	active  bool     // pbp.service is active
}

type fakeBridgeSys struct{ node.System }

func (fakeBridgeSys) BridgeState(_ context.Context, b string) (nodeproto.BridgeState, error) {
	return nodeproto.BridgeState{Name: b, DisableIPv6: "1", Addrs: []byte(`[]`)}, nil
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ = strings.Cut(host, ".")
	f := &fakeNode{t: t, root: t.TempDir(), host: host, shell: "/usr/sbin/nologin",
		roles: map[string]string{}, tokens: map[string][]string{}}
	for _, d := range []string{"var/lib/pbp", "etc/sudoers.d", "etc/ssh", "etc/pve/local", "usr/bin", "usr/sbin"} {
		if err := os.MkdirAll(filepath.Join(f.root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(f.root, "var/lib/pbp"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.write("/usr/bin/sudo", "", 0o755)
	f.write("/usr/sbin/visudo", "", 0o755)
	f.write(nodesetup.HostKeyPath, genKey(t, "root@node")+"\n", 0o644)
	cert, err := os.ReadFile("../../internal/nodesetup/testdata/api-cert.crt")
	if err != nil {
		t.Fatal(err)
	}
	f.write(nodesetup.PveCertPath, string(cert), 0o640)
	return f
}

func genKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
}

func (f *fakeNode) write(p, data string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, p), []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeNode) env() nodesetup.Env {
	uid, gid := os.Getuid(), os.Getgid()
	return nodesetup.Env{
		Root:   f.root,
		Run:    f.run,
		Chown:  func(string, int, int) error { return nil },
		Fchown: func(int, int, int) error { return nil },
		Lookup: func(user string) (int, int, string, string, error) {
			switch user {
			case "root":
				return uid, gid, "/root", "/bin/bash", nil
			case "pbp":
				return uid, gid, "/var/lib/pbp", f.shell, nil
			}
			return 0, 0, "", "", nodesetup.ErrNoSuchUser
		},
		Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
}

func (f *fakeNode) run(_ context.Context, name string, args []string, _ []byte) (string, string, int, error) {
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	line := strings.Join(args, " ")
	switch name {
	case "/usr/bin/pvesh":
		p := args[1]
		switch {
		case p == "/version":
			return `{"version":"9.0.3"}`, "", 0, nil
		case p == "/nodes":
			return j([]map[string]string{{"node": f.host}}), "", 0, nil
		case p == "/nodes/"+f.host+"/network/vmbr99":
			return `{"iface":"vmbr99","type":"bridge","bridge_ports":"none"}`, "", 0, nil
		case p == "/cluster/resources":
			return "[]", "", 0, nil
		case strings.HasPrefix(p, "/nodes/"+f.host+"/storage/"):
			return `{"active":1,"enabled":1}`, "", 0, nil
		}
		return "", "unexpected pvesh " + p, 2, nil
	case "/usr/sbin/sshd":
		return "port 22\nusepam yes\nauthorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2\nauthorizedkeyscommand none\n", "", 0, nil
	case "/usr/sbin/visudo":
		f.events = append(f.events, "visudo "+line)
		return "", "", 0, nil
	case "/usr/sbin/usermod":
		f.events = append(f.events, "usermod "+line)
		f.shell = args[1]
		return "", "", 0, nil
	case "/usr/bin/systemctl":
		if line != "is-active --quiet pbp.service" {
			return "", "unexpected systemctl " + line, 2, nil
		}
		if f.active {
			return "", "", 0, nil
		}
		return "", "", 3, nil
	case "/usr/bin/sudo":
		return f.probe(args)
	case "/usr/sbin/pveum":
		return f.pveum(args, j)
	}
	return "", "unexpected command " + name, 127, nil
}

func (f *fakeNode) probe(args []string) (string, string, int, error) {
	if f.onProbe != nil {
		f.onProbe()
	}
	req, err := nodeproto.Parse(strings.TrimPrefix(args[3], "SSH_ORIGINAL_COMMAND="))
	if err != nil {
		return "", "bad request", 2, nil
	}
	if req.Op == "version" {
		return "dev\n", "", 0, nil
	}
	return "", node.MarkerLine(nodeproto.KindRefused, req.Nonce, "out-of-range: vmid outside the range") + "\n", nodeproto.ExitRefused, nil
}

func (f *fakeNode) pveum(args []string, j func(any) string) (string, string, int, error) {
	line := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(line, "user token add "):
		f.events = append(f.events, "pveum user token add")
		f.tokens[args[3]] = append(f.tokens[args[3]], args[4])
		return j(map[string]any{"full-tokenid": nodesetup.TokenID, "value": fakeSecret}), "", 0, nil
	case line == "role list --output-format json":
		var out []map[string]any
		for id, p := range f.roles {
			out = append(out, map[string]any{"roleid": id, "privs": p, "special": 0})
		}
		return j(out), "", 0, nil
	case line == "user list --output-format json":
		out := []map[string]any{{"userid": "root@pam", "enable": 1}}
		for _, u := range f.users {
			out = append(out, map[string]any{"userid": u, "enable": 1})
		}
		return j(out), "", 0, nil
	case line == "acl list --output-format json":
		if f.acls == nil {
			return "[]", "", 0, nil
		}
		return j(f.acls), "", 0, nil
	case strings.HasPrefix(line, "user token list "):
		if !slices.Contains(f.users, args[3]) {
			return "", "no such user ('" + args[3] + "')\n", 255, nil
		}
		out := []map[string]any{}
		for _, tk := range f.tokens[args[3]] {
			out = append(out, map[string]any{"tokenid": tk, "privsep": 1, "comment": "", "expire": 0})
		}
		return j(out), "", 0, nil
	}
	f.events = append(f.events, "pveum "+line)
	switch {
	case args[0] == "role" && (args[1] == "add" || args[1] == "modify"):
		f.roles[args[2]] = args[4]
	case line == "user add pbp@pve --comment PBP proofing":
		f.users = append(f.users, args[2])
	case args[0] == "acl" && args[1] == "modify":
		if args[2] == f.failACL {
			return "", "acl failed", 1, nil
		}
		f.acls = append(f.acls,
			fakeACL{args[2], nodesetup.RoleName, "user", nodesetup.UserID, 1},
			fakeACL{args[2], nodesetup.RoleName, "token", nodesetup.TokenID, 1})
	case line == "user token remove pbp@pve proof":
		f.tokens[nodesetup.UserID] = nil
		f.acls = slices.DeleteFunc(f.acls, func(a fakeACL) bool { return a.Type == "token" })
	case args[0] == "acl" && args[1] == "delete":
		f.acls = slices.DeleteFunc(f.acls, func(a fakeACL) bool {
			return a.Path == args[2] && (a.Type == "user" && slices.Contains(args, "--users") ||
				a.Type == "token" && slices.Contains(args, "--tokens"))
		})
	case line == "user delete pbp@pve":
		f.users = nil
	case line == "role delete PBPProofer":
		delete(f.roles, nodesetup.RoleName)
	default:
		return "", "unexpected pveum " + line, 2, nil
	}
	return "", "", 0, nil
}

// files lists every regular file and dir under the root, with its content,
// so a test can tell whether anything changed.
func (f *fakeNode) snapshot() string {
	f.t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sb.WriteString(p + " " + fi.Mode().String() + "\n")
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sb.Write(b)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return sb.String()
}

type setupRig struct {
	*env
	f      *fakeNode
	pubkey string
}

// newSetupRig is an interactive root shell on the fake node: euid 0, stdin a
// terminal, no SSH_ORIGINAL_COMMAND and no SUDO_USER.
func newSetupRig(t *testing.T) *setupRig {
	t.Helper()
	e := newEnv(t)
	f := newFakeNode(t)
	e.a.euid = func() int { return 0 }
	e.a.isTerminal = func() bool { return true }
	e.a.nodeSetupEnv = f.env()
	e.a.nodeSys = fakeBridgeSys{}
	e.a.stdin = strings.NewReader("")
	unsetenv(t, "SSH_ORIGINAL_COMMAND")
	unsetenv(t, "SUDO_USER")
	return &setupRig{env: e, f: f, pubkey: genKey(t, "pbp@host")}
}

func (r *setupRig) setupArgs(extra ...string) []string {
	return append([]string{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99",
		"--pubkey", r.pubkey, "--storages", "local-zfs,pbs1", "--daemon-version", version, "--create-token"}, extra...)
}

func (r *setupRig) main(stdin string, args ...string) int {
	r.out.Reset()
	r.errOut.Reset()
	r.a.stdin = strings.NewReader(stdin)
	return r.a.main(context.Background(), args)
}

func TestNodeSetupGateRefuses(t *testing.T) {
	cases := []struct {
		name string
		mod  func(t *testing.T, r *setupRig)
	}{
		{"ssh original command set", func(t *testing.T, _ *setupRig) { t.Setenv("SSH_ORIGINAL_COMMAND", "") }},
		{"sudo from pbp", func(t *testing.T, _ *setupRig) { t.Setenv("SUDO_USER", "pbp") }},
		{"not root", func(_ *testing.T, r *setupRig) { r.a.euid = func() int { return 1000 } }},
		{"stdin not a terminal", func(_ *testing.T, r *setupRig) { r.a.isTerminal = func() bool { return false } }},
	}
	for _, sub := range []string{"setup", "uninstall"} {
		for _, tc := range cases {
			for _, yes := range []bool{false, true} {
				t.Run(sub+"/"+tc.name+map[bool]string{true: "/yes"}[yes], func(t *testing.T) {
					r := newSetupRig(t)
					tc.mod(t, r)
					before := r.f.snapshot()
					args := []string{"node", "uninstall", "--remove-token"}
					if sub == "setup" {
						args = r.setupArgs()
					}
					if yes {
						args = append(args, "--yes")
					}
					if code := r.main("y\n", args...); code != exitUsage {
						t.Errorf("exit %d, want %d; stderr %q", code, exitUsage, r.errOut)
					}
					if !strings.Contains(r.errOut.String(), "pbp node "+sub+": refused: run it interactively as root") ||
						!strings.Contains(r.errOut.String(), "usage:") {
						t.Errorf("stderr %q", r.errOut)
					}
					if len(r.f.events) != 0 || r.f.snapshot() != before {
						t.Errorf("refused run changed something: %v", r.f.events)
					}
				})
			}
		}
	}
}

func TestNodeSetupGateAllowsAdminSudo(t *testing.T) {
	for _, u := range []string{"root", "admin"} {
		t.Run(u, func(t *testing.T) {
			r := newSetupRig(t)
			t.Setenv("SUDO_USER", u)
			if code := r.main("", r.setupArgs("--dry-run")...); code != exitOK {
				t.Errorf("exit %d; stderr %q", code, r.errOut)
			}
		})
	}
}

func TestNodeSetupUsageErrors(t *testing.T) {
	r := newSetupRig(t)
	for _, args := range [][]string{
		{"node", "setup"},
		{"node", "setup", "--bridge", "vmbr99", "--pubkey", r.pubkey},
		{"node", "setup", "--vmid-range", "990049-990000", "--bridge", "vmbr99", "--pubkey", r.pubkey},
		{"node", "setup", "--vmid-range", "abc", "--bridge", "vmbr99", "--pubkey", r.pubkey},
		{"node", "setup", "--vmid-range", "99-200", "--bridge", "vmbr99", "--pubkey", r.pubkey},
		{"node", "setup", "--vmid-range", "990000-990049", "--pubkey", r.pubkey},
		{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99"},
		{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99", "--pubkey", r.pubkey, "--storages", "a,,b"},
		{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99", "--pubkey", r.pubkey, "extra"},
		{"node", "setup", "--bogus"},
		{"node", "uninstall", "extra"},
	} {
		if code := r.main("", args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d; stderr %q", args, code, exitUsage, r.errOut)
		}
	}
	if len(r.f.events) != 0 {
		t.Errorf("usage errors ran commands: %v", r.f.events)
	}
	// A bad bridge name is a usage error before any check runs.
	for _, b := range []string{"-x", "../etc", "vmbr99;id", "a/b", "averyveryverylongbridge"} {
		if code := r.main("", "node", "setup", "--vmid-range", "990000-990049", "--bridge", b, "--pubkey", r.pubkey); code != exitUsage ||
			!strings.Contains(r.errOut.String(), "--bridge must be a bridge name") {
			t.Errorf("--bridge %q: exit %d; stderr %q", b, code, r.errOut)
		}
	}
}

func TestNodeSetupMissingDaemonVersion(t *testing.T) {
	r := newSetupRig(t)
	args := []string{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99", "--pubkey", r.pubkey}
	if code := r.main("", args...); code != exitFail {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(r.errOut.String(), "check") || !strings.Contains(r.errOut.String(), "--daemon-version") {
		t.Errorf("stderr %q", r.errOut)
	}
}

func TestNodeSetupDryRunWritesNothing(t *testing.T) {
	r := newSetupRig(t)
	before := r.f.snapshot()
	if code := r.main("y\n", r.setupArgs("--dry-run")...); code != exitOK {
		t.Fatalf("exit %d; stderr %q", code, r.errOut)
	}
	if len(r.f.events) != 0 || r.f.snapshot() != before {
		t.Errorf("dry run changed something: %v", r.f.events)
	}
	out := r.out.String()
	for _, want := range []string{"create", "sudoers rule", nodesetup.SudoersPath, "+" + nodesetup.AuthorizedKeysLine(r.pubkey)} {
		if !strings.Contains(out, want) {
			t.Errorf("plan omits %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[y/N]") || strings.Contains(out, nodeproto.ReplyBegin) {
		t.Errorf("dry run prompted or replied:\n%s", out)
	}
}

func TestNodeSetupDeclineWritesNothing(t *testing.T) {
	for _, answer := range []string{"N\n", "\n", "", "nope\n"} {
		r := newSetupRig(t)
		before := r.f.snapshot()
		if code := r.main(answer, r.setupArgs()...); code != exitFail {
			t.Errorf("%q: exit %d", answer, code)
		}
		if !strings.Contains(r.out.String(), "Apply these changes? [y/N] ") || !strings.Contains(r.out.String()+r.errOut.String(), "nothing changed") {
			t.Errorf("%q: stdout %q stderr %q", answer, r.out, r.errOut)
		}
		if len(r.f.events) != 0 || r.f.snapshot() != before {
			t.Errorf("%q: declined run changed something: %v", answer, r.f.events)
		}
	}
}

func checkReply(t *testing.T, r *setupRig, out, secret string) nodeproto.SetupReply {
	t.Helper()
	rep, err := nodeproto.ReadReply(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ReadReply: %v\n%s", err, out)
	}
	if rep.Node != r.f.host || rep.TokenID != nodesetup.TokenID || rep.TokenSecret != secret ||
		rep.VMIDRange != [2]int{990000, 990049} || rep.Bridge != "vmbr99" || rep.HelperVersion != version ||
		rep.PubKey != r.pubkey {
		t.Errorf("reply %+v", rep)
	}
	if hasCopy := strings.Contains(out, "Copy this block now; the token secret is not shown again."); hasCopy != (secret != "") {
		t.Errorf("copy warning present %v with secret %q", hasCopy, secret)
	}
	return rep
}

func TestNodeSetupFullFlow(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("y\n", r.setupArgs()...); code != exitOK {
		t.Fatalf("exit %d; stderr %q\nstdout %s", code, r.errOut, r.out)
	}
	checkReply(t, r, r.out.String(), fakeSecret)
	if strings.Contains(r.errOut.String(), fakeSecret) {
		t.Error("secret on stderr")
	}
	if strings.Count(r.out.String(), fakeSecret) != 0 {
		t.Error("secret in clear text on stdout (it belongs only inside the reply block)")
	}
	b, err := os.ReadFile(filepath.Join(r.f.root, nodesetup.SudoersPath))
	if err != nil || string(b) != nodesetup.SudoersText {
		t.Errorf("sudoers %q %v", b, err)
	}

	// A second run finds everything correct.
	if code := r.main("", r.setupArgs()...); code != exitOK {
		t.Fatalf("second run: exit %d; stderr %q", code, r.errOut)
	}
	first, _, _ := strings.Cut(r.out.String(), "\n")
	if first != "setup has already been completed" {
		t.Errorf("first line %q", first)
	}
	if strings.Contains(r.out.String(), "[y/N]") {
		t.Error("already-complete run prompted")
	}
	checkReply(t, r, r.out.String(), "")
}

func TestNodeSetupYesSkipsPrompt(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("", r.setupArgs("--yes")...); code != exitOK {
		t.Fatalf("exit %d; stderr %q", code, r.errOut)
	}
	if strings.Contains(r.out.String(), "[y/N]") {
		t.Error("--yes prompted")
	}
	checkReply(t, r, r.out.String(), fakeSecret)
}

func TestNodeSetupRotateToken(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("", r.setupArgs("--yes")...); code != exitOK {
		t.Fatalf("setup: exit %d; stderr %q", code, r.errOut)
	}
	r.f.events = nil
	if code := r.main("", r.setupArgs("--rotate-token", "--yes")...); code != exitOK {
		t.Fatalf("rotate: exit %d; stderr %q", code, r.errOut)
	}
	if strings.Contains(r.out.String(), "setup has already been completed") {
		t.Error("rotate reported already complete")
	}
	if !slices.Contains(r.f.events, "pveum user token remove pbp@pve proof") || !slices.Contains(r.f.events, "pveum user token add") {
		t.Errorf("events %v", r.f.events)
	}
	checkReply(t, r, r.out.String(), fakeSecret)
}

func TestNodeSetupApplyFailureStillPrintsSecret(t *testing.T) {
	r := newSetupRig(t)
	r.f.failACL = "/storage/pbs1"
	code := r.main("", r.setupArgs("--yes")...)
	if code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	checkReply(t, r, r.out.String(), fakeSecret)
	if !strings.Contains(r.errOut.String(), "apply") || !strings.Contains(r.errOut.String(), "ACL /storage/pbs1") {
		t.Errorf("stderr %q does not name the step", r.errOut)
	}
	if strings.Contains(r.errOut.String(), fakeSecret) {
		t.Error("secret on stderr")
	}
	if _, err := os.Stat(filepath.Join(r.f.root, nodesetup.AuthorizedKeys)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("authorized_keys written after a failure: %v", err)
	}
}

func TestNodeSetupDiffersNeedsReplace(t *testing.T) {
	r := newSetupRig(t)
	r.f.write(nodesetup.SudoersPath, "pbp ALL=(ALL) NOPASSWD: ALL\n", 0o440)
	before := r.f.snapshot()
	if code := r.main("y\n", r.setupArgs()...); code != exitFail {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(r.out.String(), "differs") || !strings.Contains(r.errOut.String(), "--replace") {
		t.Errorf("stdout %q stderr %q", r.out, r.errOut)
	}
	if r.f.snapshot() != before {
		t.Error("changed files without --replace")
	}
	if code := r.main("y\n", r.setupArgs("--replace")...); code != exitOK {
		t.Fatalf("--replace: exit %d; stderr %q", code, r.errOut)
	}
	m, _ := filepath.Glob(filepath.Join(r.f.root, nodesetup.SudoersPath+".pbp-backup-*"))
	if len(m) != 1 {
		t.Errorf("backups %v", m)
	}
}

func TestNodeUninstall(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("", r.setupArgs("--yes")...); code != exitOK {
		t.Fatalf("setup: exit %d; stderr %q", code, r.errOut)
	}
	r.f.events = nil

	before := r.f.snapshot()
	if code := r.main("y\n", "node", "uninstall", "--remove-token", "--dry-run"); code != exitOK {
		t.Fatalf("dry run: exit %d; stderr %q", code, r.errOut)
	}
	if len(r.f.events) != 0 || r.f.snapshot() != before {
		t.Errorf("dry run changed something: %v", r.f.events)
	}
	for _, want := range []string{"remove", nodesetup.SudoersPath, nodesetup.AuthorizedKeys, nodesetup.TokenID} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("plan omits %q:\n%s", want, r.out)
		}
	}

	if code := r.main("n\n", "node", "uninstall", "--remove-token"); code != exitFail {
		t.Errorf("declined: exit %d", code)
	}
	if len(r.f.events) != 0 || r.f.snapshot() != before {
		t.Errorf("declined uninstall changed something: %v", r.f.events)
	}

	if code := r.main("y\n", "node", "uninstall", "--remove-token"); code != exitOK {
		t.Fatalf("uninstall: exit %d; stderr %q", code, r.errOut)
	}
	for _, p := range []string{nodesetup.SudoersPath, nodesetup.AuthorizedKeys, nodesetup.NodeYAMLPath} {
		if _, err := os.Lstat(filepath.Join(r.f.root, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left: %v", p, err)
		}
	}
	if r.f.shell != nodesetup.NoLogin || len(r.f.users) != 0 || len(r.f.roles) != 0 {
		t.Errorf("shell %q users %v roles %v", r.f.shell, r.f.users, r.f.roles)
	}

	if code := r.main("", "node", "uninstall", "--remove-token"); code != exitOK {
		t.Fatalf("second uninstall: exit %d", code)
	}
	if strings.TrimSpace(r.out.String()) != "nothing to remove" {
		t.Errorf("second uninstall stdout %q", r.out)
	}
}

func TestNodeUninstallReportsRefused(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("", r.setupArgs("--yes")...); code != exitOK {
		t.Fatalf("setup: exit %d; stderr %q", code, r.errOut)
	}
	yaml := filepath.Join(r.f.root, nodesetup.NodeYAMLPath)
	if err := os.Remove(yaml); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", yaml); err != nil {
		t.Fatal(err)
	}
	code := r.main("", "node", "uninstall", "--yes")
	if code != exitFail {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(r.out.String(), "refused") || !strings.Contains(r.errOut.String(), nodesetup.NodeYAMLPath) {
		t.Errorf("stdout %q stderr %q", r.out, r.errOut)
	}
	if _, err := os.Lstat(filepath.Join(r.f.root, nodesetup.SudoersPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sudoers left: %v", err)
	}
}

func TestNodeUsageListsSetup(t *testing.T) {
	for _, want := range []string{"pbp node setup", "pbp node uninstall"} {
		if !strings.Contains(nodeUsage, want) {
			t.Errorf("node usage omits %q", want)
		}
	}
	if !strings.Contains(usage, "setup") {
		t.Errorf("usage omits setup: %q", usage)
	}
}

func TestNodeSetupNoHostKeyChangesNothing(t *testing.T) {
	for name, mod := range map[string]func(f *fakeNode){
		"missing": func(f *fakeNode) {
			if err := os.Remove(filepath.Join(f.root, nodesetup.HostKeyPath)); err != nil {
				f.t.Fatal(err)
			}
		},
		"not ed25519": func(f *fakeNode) {
			f.write(nodesetup.HostKeyPath, "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC root@node\n", 0o644)
		},
		"no api cert": func(f *fakeNode) {
			if err := os.Remove(filepath.Join(f.root, nodesetup.PveCertPath)); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t)
			mod(r.f)
			before := r.f.snapshot()
			code := r.main("y\n", r.setupArgs("--yes")...)
			if code == exitOK {
				t.Fatal("exit 0")
			}
			if !strings.Contains(r.errOut.String(), "FAILED at reply") || !strings.Contains(r.out.String()+r.errOut.String(), "nothing changed") {
				t.Errorf("stdout %q stderr %q", r.out, r.errOut)
			}
			if strings.Contains(r.out.String(), nodeproto.ReplyBegin) || strings.Contains(r.out.String(), "[y/N]") {
				t.Errorf("replied or prompted: %q", r.out)
			}
			if len(r.f.events) != 0 || r.f.snapshot() != before {
				t.Errorf("changed something: %v", r.f.events)
			}
		})
	}
}

func TestNodeSetupReplyRacePrintsSecretOutsideBlock(t *testing.T) {
	r := newSetupRig(t)
	r.f.onProbe = func() { _ = os.Remove(filepath.Join(r.f.root, nodesetup.HostKeyPath)) }
	code := r.main("", r.setupArgs("--yes")...)
	if code == exitOK {
		t.Fatal("exit 0")
	}
	out := r.out.String()
	if strings.Count(out, fakeSecret) != 1 {
		t.Errorf("secret appears %d times on stdout, want once:\n%s", strings.Count(out, fakeSecret), out)
	}
	if strings.Contains(out, nodeproto.ReplyBegin) || strings.Contains(out, nodeproto.ReplyEnd) {
		t.Errorf("printed a reply block:\n%s", out)
	}
	if !strings.Contains(out, nodesetup.TokenID) || !strings.Contains(out, "by hand") {
		t.Errorf("secret not labelled:\n%s", out)
	}
	if strings.Contains(r.errOut.String(), fakeSecret) {
		t.Error("secret on stderr")
	}
	if !strings.Contains(r.errOut.String(), "FAILED at reply") {
		t.Errorf("stderr %q", r.errOut)
	}
}

// A PBP daemon on the node (config.yaml or an active pbp.service) gets a
// warning, not a refusal: PBP may run on the PVE host itself.
func TestNodeSetupWarnsAboutLocalDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*fakeNode)
		reason string
	}{
		{"none", func(*fakeNode) {}, ""},
		{"config", func(f *fakeNode) {
			if err := os.MkdirAll(filepath.Join(f.root, "etc/pbp"), 0o750); err != nil {
				t.Fatal(err)
			}
			f.write("/etc/pbp/config.yaml", "target: {}\n", 0o640)
		}, "/etc/pbp/config.yaml exists"},
		{"active", func(f *fakeNode) { f.active = true }, "pbp.service is active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSetupRig(t)
			tc.mod(r.f)
			if code := r.main("y\n", r.setupArgs()...); code != exitOK {
				t.Fatalf("exit %d; stderr %q", code, r.errOut)
			}
			warned := strings.Contains(r.errOut.String(), "warning: this node also runs a PBP daemon as user pbp")
			if warned != (tc.reason != "") || !strings.Contains(r.errOut.String(), tc.reason) {
				t.Errorf("stderr %q, want warning %v (%s)", r.errOut, tc.reason != "", tc.reason)
			}
			if warned && !strings.Contains(r.errOut.String(), "owns /var/lib/pbp") {
				t.Errorf("warning does not say why: %q", r.errOut)
			}
		})
	}
}
