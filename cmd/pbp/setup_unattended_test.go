package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
)

// sepFlags answer setup for the separate-host env (newSetupEnv), matching
// the reply e.reply builds.
var sepFlags = []string{"--unattended", "--address", "pve1.example.lan", "--vmid-range", "990000-990009",
	"--bridge", "vmbr77", "--restore-storage", "local-lvm", "--storages", "pbs-a"}

func TestSetupUnattendedOnNode(t *testing.T) {
	r := newLocalRig(t)
	r.a.isTerminal = func() bool { return false }
	code := r.mainNoStdin(t, "setup", "--deb", "--unattended", "--storages", "pbs1")
	out := r.out.String() + r.errOut.String()
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, never := range []string{"Set up this node here?", "[y/N]", fakeSecret} {
		if strings.Contains(out, never) {
			t.Errorf("output shows %q:\n%s", never, out)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(r.deb, "config.yaml"))
	if err != nil || !strings.Contains(string(cfg), "host: 127.0.0.1") || !strings.Contains(string(cfg), "vmid_range: [990000, 990049]") {
		t.Errorf("config:\n%s", cfg)
	}
	if b, err := os.ReadFile(filepath.Join(r.deb, "secrets", "pve_token")); err != nil || string(b) != fakeSecret+"\n" {
		t.Errorf("token file %q, %v", b, err)
	}
}

func TestSetupUnattendedSeparateHost(t *testing.T) {
	for _, viaStdin := range []bool{false, true} {
		e := newSetupEnv(t)
		code := e.runReader(noReadStdin{t}, sepFlags...)
		if code != exitWaitReply {
			t.Fatalf("first run: exit %d, want %d; stderr %q", code, exitWaitReply, e.errOut)
		}
		cfg, _, err := hostsetup.LoadState(hostsetup.Layout{Dir: e.dir})
		if err != nil {
			t.Fatal(err)
		}
		want := hostsetup.SetupArgs(cfg, e.pub, version, false).Line() + "\n"
		if e.out.String() != want {
			t.Errorf("stdout %q\nwant only the node line %q", e.out, want)
		}
		rep := e.reply(nil)
		if viaStdin {
			code = e.run(rep, append(sepFlags, "--node-reply", "-")...)
		} else {
			p := filepath.Join(t.TempDir(), "reply")
			if err := os.WriteFile(p, []byte(rep), 0o600); err != nil {
				t.Fatal(err)
			}
			code = e.runReader(noReadStdin{t}, append(sepFlags, "--node-reply", p)...)
		}
		if code != exitOK {
			t.Fatalf("with the reply (stdin %v): exit %d; stdout %q; stderr %q", viaStdin, code, e.out, e.errOut)
		}
		if b, err := os.ReadFile(filepath.Join(e.dir, "secrets", "pve_token")); err != nil || string(b) != setupSecret+"\n" {
			t.Errorf("token file %q, %v", b, err)
		}
	}
}

func TestSetupUnattendedMissing(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.runReader(noReadStdin{t}, "--unattended", "--address", "pve1.example.lan"); code != exitFail ||
		!strings.Contains(e.errOut.String(), "--storages is required") {
		t.Errorf("exit %d; stderr %q", code, e.errOut)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "config.yaml")); err == nil {
		t.Error("config.yaml was written")
	}
}

func TestSetupUnattendedConflict(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.run(answers + e.reply(nil)); code != exitOK {
		t.Fatalf("interactive setup: exit %d; stderr %q", code, e.errOut)
	}
	if code := e.runReader(noReadStdin{t}, "--unattended", "--bridge", "vmbr99"); code != exitFail ||
		!strings.Contains(e.errOut.String(), "--bridge vmbr99 differs from target.bridge vmbr77") {
		t.Errorf("exit %d; stderr %q", code, e.errOut)
	}
	if code := e.runReader(noReadStdin{t}, "--unattended", "--bridge", "vmbr77"); code != exitOK {
		t.Errorf("matching flag: exit %d; stderr %q", code, e.errOut)
	}
}

func TestSetupUnattendedRotation(t *testing.T) {
	r := newLocalRig(t)
	r.f.complete()
	r.a.isTerminal = func() bool { return false }
	before := len(r.f.events)
	code := r.mainNoStdin(t, "setup", "--deb", "--unattended", "--storages", "pbs1")
	if code != exitNeedRotate || !strings.Contains(r.out.String()+r.errOut.String(), "pass --rotate-token") {
		t.Fatalf("exit %d:\n%s%s", code, r.out, r.errOut)
	}
	for _, ev := range r.f.events[before:] {
		if strings.Contains(ev, "token remove") {
			t.Errorf("token rotated without --rotate-token: %s", ev)
		}
	}

	r2 := newLocalRig(t)
	r2.f.complete()
	r2.a.isTerminal = func() bool { return false }
	if code := r2.mainNoStdin(t, "setup", "--deb", "--unattended", "--storages", "pbs1", "--rotate-token"); code != exitOK {
		t.Fatalf("with --rotate-token: exit %d:\n%s%s", code, r2.out, r2.errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(r2.deb, "secrets", "pve_token")); string(b) != fakeSecret+"\n" {
		t.Errorf("token file %q", b)
	}
}

func TestSetupFlagsNeedUnattended(t *testing.T) {
	e := newSetupEnv(t)
	for _, args := range [][]string{{"--storages", "pbs1"}, {"--node-reply", "x"}, {"--rotate-token"}} {
		if code := e.a.main(context.Background(), append([]string{"setup", "--dir", e.dir}, args...)); code != exitUsage ||
			!strings.Contains(e.errOut.String(), "need --unattended") {
			t.Errorf("%q: exit %d; stderr %q", args, code, e.errOut)
		}
		e.errOut.Reset()
	}
}

// --rotate-token acts only when this run sets up the node itself; on a
// separate host it is refused before anything is written, not ignored
// (issue #134).
func TestSetupRotateTokenRefusedOffNode(t *testing.T) {
	e := newSetupEnv(t)
	code := e.runReader(noReadStdin{t}, append(sepFlags, "--rotate-token")...)
	if code != exitUsage || !strings.Contains(e.errOut.String(), "--rotate-token") || !strings.Contains(e.errOut.String(), "pbp node setup") {
		t.Errorf("exit %d, want %d; stderr %q", code, exitUsage, e.errOut)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "config.yaml")); err == nil {
		t.Error("config.yaml was written")
	}
}

// A token that must be replaced has its own exit code (issue #134), and an
// unattended run puts only the node's --rotate-token line on stdout, as
// exit 4 does with the node line.
func TestSetupUnattendedNeedRotate(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.runReader(noReadStdin{t}, sepFlags...); code != exitWaitReply {
		t.Fatalf("first run: exit %d; stderr %q", code, e.errOut)
	}
	p := filepath.Join(t.TempDir(), "reply")
	if err := os.WriteFile(p, []byte(e.reply(func(r *nodeproto.SetupReply) { r.TokenSecret = "" })), 0o600); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	e.errOut.Reset()
	code := e.runReader(noReadStdin{t}, append(sepFlags, "--node-reply", p)...)
	if code != exitNeedRotate {
		t.Fatalf("exit %d, want %d; stderr %q", code, exitNeedRotate, e.errOut)
	}
	cfg, _, err := hostsetup.LoadState(hostsetup.Layout{Dir: e.dir})
	if err != nil {
		t.Fatal(err)
	}
	sa := hostsetup.SetupArgs(cfg, e.pub, version, false)
	sa.RotateToken = true
	if want := sa.Line() + "\n"; e.out.String() != want {
		t.Errorf("stdout %q\nwant only the rotate line %q", e.out, want)
	}
	if !strings.Contains(e.errOut.String(), "already exists on the node") {
		t.Errorf("stderr lacks the explanation: %q", e.errOut)
	}
}

// On the node, but where this run may not set it up (here: not root),
// --rotate-token is refused too.
func TestSetupRotateTokenRefusedWhenGateRefuses(t *testing.T) {
	r := newLocalRig(t)
	r.a.isTerminal = func() bool { return false }
	r.a.euid = func() int { return 1000 }
	before := len(r.f.events)
	if code := r.mainNoStdin(t, "setup", "--deb", "--unattended", "--storages", "pbs1", "--rotate-token"); code != exitUsage {
		t.Errorf("exit %d, want %d; stderr %q", code, exitUsage, r.errOut)
	}
	if len(r.f.events) != before {
		t.Errorf("the node changed: %v", r.f.events[before:])
	}
}

// A rejected secret on a separate host: the first run prints only the
// --rotate-token line and exits 5; the run with that line's reply
// finishes (issue #134).
func TestSetupUnattendedRejectedSecret(t *testing.T) {
	e := newSetupEnv(t)
	if code := e.runReader(noReadStdin{t}, sepFlags...); code != exitWaitReply {
		t.Fatalf("first run: exit %d; stderr %q", code, e.errOut)
	}
	p := filepath.Join(t.TempDir(), "reply")
	if err := os.WriteFile(p, []byte(e.reply(nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.runReader(noReadStdin{t}, append(sepFlags, "--node-reply", p)...); code != exitOK {
		t.Fatalf("setup: exit %d; stderr %q", code, e.errOut)
	}
	tokenPath := filepath.Join(e.dir, "secrets", "pve_token")
	rejectSecret(e.a, tokenPath, setupSecret)

	code := e.runReader(noReadStdin{t}, sepFlags...)
	if code != exitNeedRotate {
		t.Fatalf("rejected: exit %d, want %d; stderr %q", code, exitNeedRotate, e.errOut)
	}
	cfg, _, err := hostsetup.LoadState(hostsetup.Layout{Dir: e.dir})
	if err != nil {
		t.Fatal(err)
	}
	if want := rotateLine(cfg, e.pub) + "\n"; e.out.String() != want {
		t.Errorf("stdout %q\nwant only the rotate line %q", e.out, want)
	}

	if err := os.WriteFile(p, []byte(e.reply(func(r *nodeproto.SetupReply) { r.TokenSecret = "rotated-secret" })), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.runReader(noReadStdin{t}, append(sepFlags, "--node-reply", p)...); code != exitOK {
		t.Fatalf("with the new secret: exit %d; stdout %q; stderr %q", code, e.out, e.errOut)
	}
	if b, _ := os.ReadFile(tokenPath); string(b) != "rotated-secret\n" {
		t.Errorf("token file %q", b)
	}
}

// On the node, a rejected secret is replaced with --rotate-token; without
// it the run exits 5 with nothing on stdout (the remedy is the flag, not a
// line to run elsewhere).
func TestSetupUnattendedOnNodeRejectedSecret(t *testing.T) {
	r := newLocalRig(t)
	r.f.complete()
	r.a.isTerminal = func() bool { return false }
	args := []string{"setup", "--deb", "--unattended", "--storages", "pbs1"}
	if code := r.mainNoStdin(t, append(args, "--rotate-token")...); code != exitOK {
		t.Fatalf("setup: exit %d:\n%s%s", code, r.out, r.errOut)
	}
	tokenPath := filepath.Join(r.deb, "secrets", "pve_token")
	if err := os.WriteFile(tokenPath, []byte("stale-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectSecret(r.a, tokenPath, "stale-secret")

	if code := r.mainNoStdin(t, args...); code != exitNeedRotate {
		t.Fatalf("rejected: exit %d, want %d:\n%s%s", code, exitNeedRotate, r.out, r.errOut)
	}
	if r.out.String() != "" {
		t.Errorf("stdout %q, want nothing", r.out)
	}
	if !strings.Contains(r.errOut.String(), "--rotate-token") {
		t.Errorf("stderr does not name --rotate-token: %q", r.errOut)
	}
	if code := r.mainNoStdin(t, append(args, "--rotate-token")...); code != exitOK {
		t.Fatalf("with --rotate-token: exit %d:\n%s%s", code, r.out, r.errOut)
	}
	if b, _ := os.ReadFile(tokenPath); string(b) != fakeSecret+"\n" {
		t.Errorf("token file %q", b)
	}
}
