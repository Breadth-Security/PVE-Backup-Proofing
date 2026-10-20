package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target/targetfake"
)

const (
	setupFP     = "11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF:11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF"
	setupSecret = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	answers     = "pve1.example.lan\n990000-990009\nvmbr77\nlocal-lvm\npbs-a\n\n\n"
)

// fakeTerm records echo changes on a fake terminal.
type fakeTerm struct{ off, restored int }

func (f *fakeTerm) echoOff() (func(), error) {
	f.off++
	return func() { f.restored++ }, nil
}

type setupEnv struct {
	t        *testing.T
	a        *app
	dir      string
	stateDir string
	out      *bytes.Buffer
	errOut   *bytes.Buffer
	term     *fakeTerm
	tgt      *targetfake.Fake
	checked  []config.Config
	pub      string
}

// newSetupEnv wires an app for pbp setup in a temp Docker directory with a
// key pair already there (so a canned reply can name it), fake ownership,
// a fake terminal and a fake node behind connect.
func newSetupEnv(t *testing.T) *setupEnv {
	t.Helper()
	t.Cleanup(hostsetup.TestHooks(func(string, int, int) error { return nil },
		func(string) (int, error) { return 4242, nil }, t.TempDir()))
	e := &setupEnv{t: t, dir: t.TempDir(), stateDir: filepath.Join(t.TempDir(), "state"),
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, term: &fakeTerm{}, tgt: targetfake.New(990000, 990009)}
	t.Setenv("PBP_STATE_DIR", e.stateDir)
	pub, _, err := hostsetup.EnsureKey(hostsetup.Layout{Dir: e.dir})
	if err != nil {
		t.Fatal(err)
	}
	e.pub = pub
	e.a = &app{stdout: e.out, stderr: e.errOut, now: func() time.Time { return fixedNow },
		isTerminal: func() bool { return true }, echoOff: e.term.echoOff,
		defaultTZ: func() string { return "Europe/Berlin" },
		connect: func(cfg config.Config) wiring {
			e.checked = append(e.checked, cfg)
			return wiring{api: pvefake.New(), target: e.tgt, notifier: &fakeNotifier{}, close: func() {}}
		}}
	return e
}

func (e *setupEnv) run(stdin string, args ...string) int {
	return e.runReader(strings.NewReader(stdin), args...)
}

func (e *setupEnv) runReader(stdin io.Reader, args ...string) int {
	e.out.Reset()
	e.errOut.Reset()
	e.a.stdin = stdin
	return e.a.main(context.Background(), append([]string{"setup", "--dir", e.dir}, args...))
}

func hostKeyLine(t *testing.T) string {
	t.Helper()
	k, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}
	return k.Type() + " " + base64.StdEncoding.EncodeToString(k.Marshal())
}

func (e *setupEnv) reply(mut func(*nodeproto.SetupReply)) string {
	r := nodeproto.SetupReply{Format: 1, Node: "pve1", HelperVersion: "0.3.0", VMIDRange: [2]int{990000, 990009},
		Bridge: "vmbr77", PubKey: e.pub, HostKey: hostKeyLine(e.t), APIFingerprint: setupFP,
		TokenID: hostsetup.TokenID, TokenSecret: setupSecret}
	if mut != nil {
		mut(&r)
	}
	block, err := nodeproto.EncodeReply(r)
	if err != nil {
		e.t.Fatal(err)
	}
	return "some terminal noise\n" + block
}

// stateEmpty fails the test if pbp setup created anything in the state dir
// (the state store or the run lock): setup runs as root and must not leave
// root-owned files there.
func (e *setupEnv) stateEmpty() {
	e.t.Helper()
	if _, err := os.Stat(e.stateDir); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("pbp setup touched the state dir %s: %v", e.stateDir, err)
	}
}

func TestSetupCommandRoundTrip(t *testing.T) {
	e := newSetupEnv(t)
	code := e.run(answers + e.reply(nil))
	out := e.out.String()
	if code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, e.errOut)
	}
	for _, want := range []string{"Node address", "On the node, as root:",
		"pbp node setup --vmid-range 990000-990009 --bridge vmbr77 --pubkey '" + e.pub + "' --storages local-lvm,pbs-a",
		"--create-token", msgPasteReply, "preflight (", "): ok\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, setupSecret) || strings.Contains(e.errOut.String(), setupSecret) {
		t.Error("the token secret was printed")
	}
	if strings.Contains(out, nodeproto.ReplyBegin) {
		t.Error("the reply was printed back")
	}
	l := hostsetup.Layout{Dir: e.dir}
	for _, p := range []string{l.ConfigPath(), l.KnownHostsPath(), l.TokenPath(), l.EnvPath(), l.KeyPath()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if tok, _ := os.ReadFile(l.TokenPath()); string(tok) != setupSecret+"\n" {
		t.Errorf("token file = %q", tok)
	}
	if env, _ := os.ReadFile(l.EnvPath()); string(env) != "TZ=Europe/Berlin\n" {
		t.Errorf(".env = %q", env)
	}
	cfg, pending, err := hostsetup.LoadState(l)
	if err != nil || pending || cfg.Target.Node != "pve1" || cfg.Target.API.TLSFingerprint != setupFP {
		t.Fatalf("config after setup: pending=%v err=%v target=%+v", pending, err, cfg.Target)
	}
	// The written config keeps the in-container paths; the check used the
	// on-disk ones.
	if cfg.Target.SSH.KeyFile != "/run/secrets/pve_ssh_key" {
		t.Errorf("config key_file = %q", cfg.Target.SSH.KeyFile)
	}
	if len(e.checked) != 1 {
		t.Fatalf("check ran %d times", len(e.checked))
	}
	c := e.checked[0]
	if c.Target.SSH.KeyFile != l.KeyPath() || c.Target.API.TokenSecretFile != l.TokenPath() ||
		c.Target.SSH.KnownHostsFile != l.KnownHostsPath() || c.Target.Node != "pve1" {
		t.Errorf("check config: ssh %+v api %+v", c.Target.SSH, c.Target.API)
	}
	if e.term.off != 1 || e.term.restored != 1 {
		t.Errorf("echo off %d, restored %d", e.term.off, e.term.restored)
	}
	// The read-only preflight: no janitor (it would list owned guests).
	for _, call := range e.tgt.CallLog() {
		if call == "Owned" || call == "Destroy" {
			t.Errorf("setup's check ran the janitor: %q", e.tgt.CallLog())
		}
	}
	e.stateEmpty()

	// Run again: everything is in place and the check passes.
	e.checked = nil
	if code := e.run(""); code != exitOK || e.out.String() != msgAlreadyComplete+"\n" {
		t.Errorf("rerun: exit %d out %q err %q", code, e.out, e.errOut)
	}
	e.stateEmpty()
}

func TestSetupAlreadyCompletedButCheckFails(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.run(answers + e.reply(nil)); code != exitOK {
		t.Fatalf("first run: exit %d err %s", code, e.errOut)
	}
	// The node broke: the rerun shows why and waits for a new reply.
	e.tgt.PrepareErr = errors.New("bridge not isolated")
	code := e.run(e.reply(func(r *nodeproto.SetupReply) { r.TokenSecret = "" }))
	out := e.out.String()
	if strings.Contains(out, msgAlreadyComplete) || !strings.Contains(out, "bridge not isolated") ||
		!strings.Contains(out, msgPasteReply) || strings.Contains(out, "--create-token") {
		t.Errorf("out:\n%s", out)
	}
	if code != exitFail {
		t.Errorf("exit %d, want %d (the check still fails)", code, exitFail)
	}
	e.stateEmpty()
}

func TestSetupNeedRotate(t *testing.T) {
	e := newSetupEnv(t)
	code := e.run(answers + e.reply(func(r *nodeproto.SetupReply) { r.TokenSecret = "" }))
	out := e.out.String() + e.errOut.String()
	if code != exitFail {
		t.Fatalf("exit %d out %s", code, out)
	}
	for _, want := range []string{"pbp@pve!proof already exists on the node and its secret cannot be read back",
		"--rotate-token", "loses access"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(e.checked) != 0 {
		t.Error("the check ran")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "known_hosts")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("known_hosts written: %v", err)
	}
}

func TestSetupRefusesBadReply(t *testing.T) {
	e := newSetupEnv(t)
	code := e.run(answers + e.reply(func(r *nodeproto.SetupReply) { r.Bridge = "vmbr99" }))
	if code != exitFail || !strings.Contains(e.errOut.String(), "bridge") {
		t.Errorf("exit %d err %q", code, e.errOut)
	}
	if strings.Contains(e.errOut.String(), setupSecret) {
		t.Error("the secret leaked")
	}
	if e.term.off != 1 || e.term.restored != 1 {
		t.Errorf("echo off %d, restored %d", e.term.off, e.term.restored)
	}
	// Ctrl-C or a bad reply keeps what was written: the rerun asks nothing.
	code = e.run(e.reply(nil))
	if code != exitOK || strings.Contains(e.out.String(), "Node address") {
		t.Errorf("rerun: exit %d out %s err %s", code, e.out, e.errOut)
	}
}

// Echo is restored when the read fails (no reply before end of input).
func TestSetupEchoRestoredOnError(t *testing.T) {
	e := newSetupEnv(t)
	code := e.run(answers + "not a reply\n")
	if code != exitFail || !strings.Contains(e.errOut.String(), "no node reply found") {
		t.Errorf("exit %d err %q", code, e.errOut)
	}
	if e.term.off != 1 || e.term.restored != 1 {
		t.Errorf("echo off %d, restored %d", e.term.off, e.term.restored)
	}
}

// Not a terminal: echo is left alone.
func TestSetupNoTerminalNoEcho(t *testing.T) {
	e := newSetupEnv(t)
	e.a.isTerminal = func() bool { return false }
	if code := e.run(answers + e.reply(nil)); code != exitOK {
		t.Fatalf("exit %d err %s", code, e.errOut)
	}
	if e.term.off != 0 {
		t.Error("echo turned off on a non-terminal")
	}
}

// An interrupt while waiting for the reply restores echo and exits non-zero;
// what was written stays.
func TestSetupInterruptRestoresEcho(t *testing.T) {
	e := newSetupEnv(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	e.a.stdin = pr
	go func() {
		_, _ = io.WriteString(pw, answers)
		cancel()
	}()
	code := e.a.main(ctx, []string{"setup", "--dir", e.dir})
	if code == exitOK {
		t.Fatalf("exit %d", code)
	}
	if e.term.off != e.term.restored {
		t.Errorf("echo off %d, restored %d", e.term.off, e.term.restored)
	}
	if !strings.Contains(e.errOut.String(), "interrupted") {
		t.Errorf("err %q", e.errOut)
	}
}

func TestSetupUsage(t *testing.T) {
	e := newSetupEnv(t)
	e.a.stdin = strings.NewReader("")
	for _, args := range [][]string{{"setup", "--deb", "--dir", "/x"}, {"setup", "extra"}, {"setup", "--bogus"}} {
		e.errOut.Reset()
		if code := e.a.main(context.Background(), args); code != exitUsage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

func TestHostTZ(t *testing.T) {
	dir := t.TempDir()
	tzFile := filepath.Join(dir, "timezone")
	lt := filepath.Join(dir, "localtime")
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := hostTZ(getenv, tzFile, lt); got != "Etc/UTC" {
		t.Errorf("nothing: %q", got)
	}
	if err := os.Symlink("/usr/share/zoneinfo/America/New_York", lt); err != nil {
		t.Fatal(err)
	}
	if got := hostTZ(getenv, tzFile, lt); got != "America/New_York" {
		t.Errorf("localtime: %q", got)
	}
	if err := os.WriteFile(tzFile, []byte("Europe/Paris\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := hostTZ(getenv, tzFile, lt); got != "Europe/Paris" {
		t.Errorf("timezone file: %q", got)
	}
	env["TZ"] = ":Asia/Tokyo"
	if got := hostTZ(getenv, tzFile, lt); got != "Asia/Tokyo" {
		t.Errorf("TZ: %q", got)
	}
}

// A config.yaml naming other key, token or known_hosts files (a 0.2 .deb
// host, say) is refused before the key is created or any node line is
// printed.
func TestSetupRefusesOtherConfigPaths(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.run(answers + e.reply(nil)); code != exitOK {
		t.Fatalf("first run: exit %d err %s", code, e.errOut)
	}
	l := hostsetup.Layout{Dir: e.dir}
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "/run/secrets/pve_ssh_key", "/run/secrets/id_ed25519", 1)
	if edited == string(b) {
		t.Fatal("key_file not found in config.yaml")
	}
	if err := os.Chmod(l.ConfigPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ConfigPath(), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(l.SecretsDir()); err != nil {
		t.Fatal(err)
	}
	e.checked = nil
	code := e.run("")
	if code != exitFail {
		t.Errorf("exit %d, want %d", code, exitFail)
	}
	for _, want := range []string{`target.ssh.key_file is "/run/secrets/id_ed25519", expected /run/secrets/pve_ssh_key`, "sudoedit " + l.ConfigPath()} {
		if !strings.Contains(e.errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, e.errOut)
		}
	}
	if strings.Contains(e.out.String(), "pbp node setup") || len(e.checked) != 0 {
		t.Errorf("went on after the refusal:\n%s", e.out)
	}
	if exists(l.KeyPath()) {
		t.Error("created a key after the refusal")
	}
}
