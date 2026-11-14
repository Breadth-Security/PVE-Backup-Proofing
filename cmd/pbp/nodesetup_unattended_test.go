package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodesetup"
)

// noReadStdin fails the test on any read: an unattended run never reads
// stdin, whatever is piped to it (issue #131).
type noReadStdin struct{ t *testing.T }

func (r noReadStdin) Read([]byte) (int, error) {
	r.t.Error("an unattended run read stdin")
	return 0, os.ErrClosed
}

func (r *setupRig) mainNoStdin(t *testing.T, args ...string) int {
	r.out.Reset()
	r.errOut.Reset()
	r.a.stdin = noReadStdin{t}
	return r.a.main(context.Background(), args)
}

func TestNodeSetupUnattendedNoTTY(t *testing.T) {
	r := newSetupRig(t)
	r.a.isTerminal = func() bool { return false }
	p := filepath.Join(t.TempDir(), "reply")
	if code := r.mainNoStdin(t, r.setupArgs("--yes", "--unattended", "--reply-file", p)...); code != exitOK {
		t.Fatalf("exit %d; stderr %q", code, r.errOut)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("reply file mode %v, want 0600", fi.Mode().Perm())
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rep, err := nodeproto.ReadReply(f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TokenSecret == "" {
		t.Error("the reply file lacks the token secret")
	}
	if strings.Contains(r.out.String(), rep.TokenSecret) || strings.Contains(r.errOut.String(), rep.TokenSecret) {
		t.Error("the token secret was printed")
	}
	if !strings.Contains(r.out.String(), p) {
		t.Errorf("stdout does not name the reply file:\n%s", r.out)
	}
}

func TestNodeGateUnattendedKeepsOtherRefusals(t *testing.T) {
	cases := []struct {
		name string
		mod  func(t *testing.T, r *setupRig)
	}{
		{"ssh original command set", func(t *testing.T, _ *setupRig) { t.Setenv("SSH_ORIGINAL_COMMAND", "") }},
		{"sudo from pbp", func(t *testing.T, _ *setupRig) { t.Setenv("SUDO_USER", "pbp") }},
		{"not root", func(_ *testing.T, r *setupRig) { r.a.euid = func() int { return 1000 } }},
	}
	for _, sub := range []string{"setup", "uninstall"} {
		for _, tc := range cases {
			t.Run(sub+"/"+tc.name, func(t *testing.T) {
				r := newSetupRig(t)
				r.a.isTerminal = func() bool { return false }
				tc.mod(t, r)
				before := r.f.snapshot()
				args := []string{"node", "uninstall", "--remove-token", "--yes", "--unattended"}
				if sub == "setup" {
					args = r.setupArgs("--yes", "--unattended", "--reply-file", filepath.Join(t.TempDir(), "reply"))
				}
				if code := r.mainNoStdin(t, args...); code != exitUsage {
					t.Errorf("exit %d, want %d; stderr %q", code, exitUsage, r.errOut)
				}
				if !strings.Contains(r.errOut.String(), "refused") {
					t.Errorf("stderr %q", r.errOut)
				}
				if len(r.f.events) != 0 || r.f.snapshot() != before {
					t.Errorf("refused run changed something: %v", r.f.events)
				}
			})
		}
	}
}

func TestNodeSetupUnattendedNeedsYes(t *testing.T) {
	for _, args := range [][]string{
		{"node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99", "--pubkey", "x", "--unattended"},
		{"node", "uninstall", "--unattended"},
	} {
		r := newSetupRig(t)
		r.a.isTerminal = func() bool { return false }
		if code := r.mainNoStdin(t, args...); code != exitUsage || !strings.Contains(r.errOut.String(), "--unattended requires --yes") {
			t.Errorf("%q: exit %d; stderr %q", args, code, r.errOut)
		}
	}
}

func TestNodeSetupReplyFileRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{existing, link} {
		r := newSetupRig(t)
		before := r.f.snapshot()
		if code := r.main("", r.setupArgs("--yes", "--reply-file", p)...); code != exitFail {
			t.Errorf("%s: exit %d; stderr %q", p, code, r.errOut)
		}
		if !strings.Contains(r.errOut.String(), "already exists") {
			t.Errorf("%s: stderr %q", p, r.errOut)
		}
		if len(r.f.events) != 0 || r.f.snapshot() != before {
			t.Errorf("%s: the node was changed before the reply file was refused: %v", p, r.f.events)
		}
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep" {
		t.Errorf("existing file overwritten: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); err == nil {
		t.Error("the symlink's target was written")
	}
}

func TestNodeUninstallUnattended(t *testing.T) {
	r := newSetupRig(t)
	if code := r.main("", r.setupArgs("--yes")...); code != exitOK {
		t.Fatalf("setup: exit %d; stderr %q", code, r.errOut)
	}
	r.a.isTerminal = func() bool { return false }
	if code := r.mainNoStdin(t, "node", "uninstall", "--remove-token", "--yes", "--unattended"); code != exitOK {
		t.Fatalf("uninstall: exit %d; stderr %q", code, r.errOut)
	}
	for _, p := range []string{nodesetup.SudoersPath, nodesetup.AuthorizedKeys, nodesetup.NodeYAMLPath} {
		if r.f.exists(p) {
			t.Errorf("%s still present", p)
		}
	}
}
