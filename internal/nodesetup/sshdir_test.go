package nodesetup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// victimDir stands for /etc/pve/priv or /root/.ssh: a directory holding an
// authorized_keys that setup and uninstall must never touch.
func victimDir(t *testing.T) (dir, snapshot string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "priv")
	mustWrite(t, filepath.Join(dir, "authorized_keys"), "ssh-ed25519 AAAAroot root@pve\n", 0o600)
	return dir, dirSnapshot(t, dir)
}

// dirSnapshot lists dir's entries with their mode and content.
func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range ents {
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(e.Name() + " " + fi.Mode().String() + "\n")
		if fi.Mode().IsRegular() {
			sb.WriteString(readFile(t, filepath.Join(dir, e.Name())))
		}
	}
	return sb.String()
}

// swapSSHDir is what a process running as pbp can do in its own home:
// move .ssh away (if there) and put a symlink to target in its place.
func swapSSHDir(t *testing.T, env Env, target string) {
	t.Helper()
	p := env.path(SSHDir)
	if _, err := os.Lstat(p); err == nil {
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// .ssh swapped for a symlink between BuildPlan and Apply: Apply fails and
// writes nothing through it, whether it would create the key or replace it.
func TestApplySSHDirSwappedAfterPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *applyFake, env Env)
		opts  ApplyOpts
	}{
		{"create key and dir", func(*testing.T, *applyFake, Env) {}, ApplyOpts{}},
		{"create key, dir in place", func(t *testing.T, f *applyFake, env Env) {
			layoutComplete(t, env.Root)
			f.complete("pve1")
			if err := os.Remove(env.path(AuthorizedKeys)); err != nil {
				t.Fatal(err)
			}
		}, ApplyOpts{}},
		{"replace key", func(t *testing.T, f *applyFake, env Env) {
			layoutComplete(t, env.Root)
			f.complete("pve1")
			mustWrite(t, env.path(AuthorizedKeys), "ssh-ed25519 AAAAother other\n", 0o644)
		}, ApplyOpts{Replace: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, env := newApplyFake(t, "/bin/sh")
			tc.setup(t, f, env)
			a := applyArgs()
			p := buildPlan(t, env, a)
			if it := item(t, p, KindFile, AuthorizedKeys); it.Status == Correct {
				t.Fatalf("authorized_keys already correct:\n%s", dump(p))
			}
			victim, before := victimDir(t)
			swapSSHDir(t, env, victim)

			_, err := Apply(context.Background(), env, p, a, "pve1", tc.opts)
			if err == nil || !strings.Contains(err.Error(), SSHDir) || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("Apply err %v, want a refusal naming %s", err, SSHDir)
			}
			if got := dirSnapshot(t, victim); got != before {
				t.Errorf("victim dir changed:\n got %s\nwant %s", got, before)
			}
			if fi, err := os.Lstat(env.path(SSHDir)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the symlink at .ssh was changed: %v", err)
			}
			for _, e := range f.events {
				if strings.HasPrefix(e, "chown ") && strings.Contains(e, "/priv") {
					t.Errorf("chown through the symlink: %s", e)
				}
			}
		})
	}
}

// A real directory swapped in after planning (pbp's own, holding an
// authorized_keys2): Apply makes it root's, then finds the extra file and
// stops before writing authorized_keys.
func TestApplySSHDirSwappedForPbpDir(t *testing.T) {
	_, env := newApplyFake(t, "/bin/sh")
	a := applyArgs()
	p := buildPlan(t, env, a)
	mustWrite(t, filepath.Join(env.path(SSHDir), "authorized_keys2"), "ssh-ed25519 AAAAevil\n", 0o644)
	_, err := Apply(context.Background(), env, p, a, "pve1", ApplyOpts{})
	if err == nil || !strings.Contains(err.Error(), "authorized_keys2") {
		t.Fatalf("Apply err %v, want a refusal naming authorized_keys2", err)
	}
	if !gone(env, AuthorizedKeys) {
		t.Error("authorized_keys written into a dir holding authorized_keys2")
	}
	if fi, err := os.Stat(env.path(SSHDir)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf(".ssh not made 0755 before the check: %v %v", fi, err)
	}
}

// .ssh swapped for a symlink between BuildUninstallPlan and Uninstall: the
// key in the victim dir is neither backed up nor removed, the sudo rule
// still goes, and the error names .ssh.
func TestUninstallSSHDirSwappedAfterPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys string
	}{
		{"setup's key", AuthorizedKeysLine(testKey) + "\n"},
		{"edited key (backup first)", "ssh-ed25519 AAAAother other\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, env := newUninstallFake(t)
			mustWrite(t, env.path(AuthorizedKeys), tc.keys, 0o644)
			p := buildUninstallPlan(t, env, false)
			if !slices.Contains(planKeys(p), "file "+AuthorizedKeys) && !slices.Contains(planKeys(p), "file "+AuthorizedKeys+" (backup)") {
				t.Fatalf("plan lacks authorized_keys: %q", planKeys(p))
			}
			victim, before := victimDir(t)
			swapSSHDir(t, env, victim)

			err := Uninstall(context.Background(), env, p, false)
			if err == nil || !strings.Contains(err.Error(), SSHDir) {
				t.Fatalf("Uninstall err %v, want a refusal naming %s", err, SSHDir)
			}
			if got := dirSnapshot(t, victim); got != before {
				t.Errorf("victim dir changed:\n got %s\nwant %s", got, before)
			}
			if !gone(env, SudoersPath) {
				t.Error("sudoers rule left")
			}
			if fi, err := os.Lstat(env.path(SSHDir)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the symlink at .ssh was changed: %v", err)
			}
		})
	}
}

func TestSSHDirAllowed(t *testing.T) {
	for name, want := range map[string]bool{
		"authorized_keys": true, "authorized_keys.pbp-backup-20261020T120000": true,
		"authorized_keys2": false, "known_hosts": false, ".pbp.tmp-1": false, "id_ed25519": false,
	} {
		if got := sshDirAllowed(name); got != want {
			t.Errorf("sshDirAllowed(%q) = %v", name, got)
		}
	}
}
