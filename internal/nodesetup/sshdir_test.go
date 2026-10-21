package nodesetup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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
		"authorized_keys2": false, "known_hosts": false, "id_ed25519": false,
		".pbp.tmp-1": true, ".pbp.tmp-": false, ".pbp.tmp-x": false, ".pbp.tmp-1/../x": false,
	} {
		if got := sshDirAllowed(name); got != want {
			t.Errorf("sshDirAllowed(%q) = %v", name, got)
		}
	}
}

// pbp hides .ssh while the plan is made (so the key is a Create) and puts
// it back, with an edited key, before Apply: Apply re-reads the key through
// the fd and refuses instead of overwriting it without a backup.
func TestApplyKeyAppearedAfterPlan(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		wantErr   bool
	}{
		{"edited key", "ssh-ed25519 AAAAadmin admin-edited\n", true},
		{"setup's own key", AuthorizedKeysLine(testKey) + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, env := newApplyFake(t, "/bin/sh")
			a := applyArgs()
			p := buildPlan(t, env, a)
			if it := item(t, p, KindFile, AuthorizedKeys); it.Status != Create {
				t.Fatalf("authorized_keys status %v, want Create", it.Status)
			}
			mustWrite(t, env.path(AuthorizedKeys), tc.key, 0o644)

			_, err := Apply(context.Background(), env, p, a, "pve1", ApplyOpts{})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "after the plan") {
					t.Fatalf("Apply err %v, want a refusal", err)
				}
			} else if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := readFile(t, env.path(AuthorizedKeys)); got != tc.key {
				t.Errorf("authorized_keys = %q, want it left as %q", got, tc.key)
			}
			if names := backupsIn(t, env.path(SSHDir)); len(names) > 0 {
				t.Errorf("unexpected backups %v", names)
			}
		})
	}
}

// An authorized_keys over 64 KiB (sparse here) is never read in full or
// copied: setup's plan calls it Differs, Apply refuses even with --replace
// and makes no backup, and uninstall removes it without a backup.
func TestOversizedAuthorizedKeys(t *testing.T) {
	big := func(t *testing.T, env Env) {
		t.Helper()
		mustWrite(t, env.path(AuthorizedKeys), "", 0o644)
		if err := os.Truncate(env.path(AuthorizedKeys), 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("setup", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		layoutComplete(t, env.Root)
		f.complete("pve1")
		big(t, env)
		a := applyArgs()
		p := buildPlan(t, env, a)
		it := item(t, p, KindFile, AuthorizedKeys)
		if it.Status != Differs || !strings.Contains(it.Diff, "64 KiB") {
			t.Fatalf("authorized_keys %v %q, want Differs naming 64 KiB", it.Status, it.Diff)
		}
		_, err := Apply(context.Background(), env, p, a, "pve1", ApplyOpts{Replace: true})
		if err == nil || !strings.Contains(err.Error(), "64 KiB") {
			t.Fatalf("Apply err %v, want a refusal naming 64 KiB", err)
		}
		if names := backupsIn(t, env.path(SSHDir)); len(names) > 0 {
			t.Errorf("backed up an oversized file: %v", names)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		_, env := newUninstallFake(t)
		big(t, env)
		p := buildUninstallPlan(t, env, false)
		if !slices.Contains(planKeys(p), "file "+AuthorizedKeys) {
			t.Fatalf("plan %q, want authorized_keys removed without a backup", planKeys(p))
		}
		if err := Uninstall(context.Background(), env, p, false); err != nil {
			t.Fatalf("Uninstall: %v", err)
		}
		if !gone(env, AuthorizedKeys) {
			t.Error("oversized authorized_keys left")
		}
		if names := backupsIn(t, filepath.Dir(env.path(AuthorizedKeys))); len(names) > 0 {
			t.Errorf("backed up an oversized file: %v", names)
		}
	})
}

// Temp files an interrupted setup left in .ssh do not block the next run:
// the plan accepts them and Apply removes them once .ssh is root's.
func TestLeftoverTempFilesRemoved(t *testing.T) {
	_, env := newApplyFake(t, "/bin/sh")
	mustWrite(t, filepath.Join(env.path(SSHDir), ".pbp.tmp-12345"), "partial", 0o600)
	a := applyArgs()
	p := buildPlan(t, env, a)
	for _, it := range p.Items {
		if it.Path == SSHDir && it.Kind == KindCheck && it.Status != Correct {
			t.Fatalf("plan refuses the leftover temp file: %s", it.Diff)
		}
	}
	if _, err := Apply(context.Background(), env, p, a, "pve1", ApplyOpts{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !gone(env, filepath.Join(SSHDir, ".pbp.tmp-12345")) {
		t.Error("leftover temp file not removed")
	}
	if gone(env, AuthorizedKeys) {
		t.Error("authorized_keys not written")
	}
}

// readManaged never blocks on a FIFO and never reads past the cap.
func TestReadManaged(t *testing.T) {
	env := Env{Root: t.TempDir()}
	fifo := "/etc/pbp/node.yaml"
	if err := os.MkdirAll(filepath.Dir(env.path(fifo)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscallMkfifo(env.path(fifo)); err != nil {
		t.Fatal(err)
	}
	if _, err := readManaged(env, fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("FIFO: err %v, want not a regular file", err)
	}
	link := "/etc/pbp/link"
	if err := os.Symlink(env.path(fifo), env.path(link)); err != nil {
		t.Fatal(err)
	}
	if _, err := readManaged(env, link); err == nil {
		t.Error("read through a symlink")
	}
}

// backupsIn lists the .pbp-backup- files in dir.
func backupsIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), backupSuffix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// The rewrite of a right-content authorized_keys sets the planned owner,
// root's ids, not pbp's (code review I16, its #49 review): ids differ here,
// unlike in the shared test environment.
func TestApplyAuthorizedKeysSetsRootIDs(t *testing.T) {
	dir := t.TempDir()
	line := []byte("restrict,command=\"sudo /usr/bin/pbp node\" ssh-ed25519 AAAA pbp\n")
	if err := os.WriteFile(filepath.Join(dir, akName), line, 0o666); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	var gotUID, gotGID = -1, -1
	env := Env{Fchown: func(_, uid, gid int) error { gotUID, gotGID = uid, gid; return nil }}
	id := ids{uid: map[string]int{"root": 1111, PbpUser: 2222}, gid: map[string]int{"root": 3333, PbpUser: 4444}}
	it := Item{Kind: KindFile, Path: AuthorizedKeys, Status: Differs, Mode: 0o644, Owner: "root", Group: "root", Content: line}
	if err := applyAuthorizedKeys(env, id, fd, it); err != nil {
		t.Fatal(err)
	}
	if gotUID != 1111 || gotGID != 3333 {
		t.Errorf("chown %d:%d, want root's 1111:3333", gotUID, gotGID)
	}
	if fi, err := os.Stat(filepath.Join(dir, akName)); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, %v", fi.Mode().Perm(), err)
	}
}
