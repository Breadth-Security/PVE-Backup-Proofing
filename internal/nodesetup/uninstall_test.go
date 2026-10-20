package nodesetup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"golang.org/x/crypto/ssh"
)

// uninstallFake extends applyFake with the commands uninstall runs. Each
// event it records carries which setup files exist at that moment, so the
// test sees the order of file removals relative to the commands.
type uninstallFake struct {
	*applyFake
}

func newUninstallFake(t *testing.T) (*uninstallFake, Env) {
	t.Helper()
	af, env := newApplyFake(t, "/bin/sh")
	f := &uninstallFake{af}
	f.complete("pve1")
	layoutComplete(t, env.Root)
	env.Run = f.run
	chown := env.Chown
	env.Chown = func(path string, uid, gid int) error {
		err := chown(path, uid, gid)
		f.events[len(f.events)-1] += " " + f.files()
		return err
	}
	env.Fchown = func(fd, uid, gid int) error {
		p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		if err != nil {
			t.Errorf("fchown: %v", err)
		}
		return env.Chown(p, uid, gid)
	}
	return f, env
}

func (f *uninstallFake) files() string {
	var s []string
	for _, p := range []string{SudoersPath, AuthorizedKeys, SSHDir, NodeYAMLPath} {
		if _, err := os.Lstat(filepath.Join(f.root, p)); err == nil {
			s = append(s, filepath.Base(p))
		}
	}
	return "[" + strings.Join(s, " ") + "]"
}

func (f *uninstallFake) run(ctx context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	ev := name + " " + strings.Join(args, " ")
	isQuery := len(args) >= 2 && args[len(args)-2] == "--output-format"
	if !isQuery {
		ev += " " + f.files()
	}
	if f.fail != nil && f.fail(name, args) {
		f.events = append(f.events, ev)
		return "", "injected failure", 1, nil
	}
	n := len(args)
	switch {
	case name == "/usr/sbin/usermod":
		f.events = append(f.events, ev)
		if !slices.Equal(args, []string{"-s", "/usr/sbin/nologin", "pbp"}) {
			return "", "bad usermod", 2, nil
		}
		f.shell = "/usr/sbin/nologin"
		return "", "", 0, nil
	case name == "/usr/sbin/visudo":
		f.events = append(f.events, ev)
		if !slices.Equal(args, []string{"-c"}) {
			return "", "bad visudo", 2, nil
		}
		return "", "", f.visudoFinal, nil
	case name == "/usr/sbin/pveum" && slices.Equal(args, []string{"user", "token", "remove", UserID, TokenName}):
		_, _, _, _ = f.applyFake.run(ctx, name, args, stdin)
		f.events[len(f.events)-1] = ev
		return "", "", 0, nil
	case name == "/usr/sbin/pveum" && n >= 5 && args[0] == "acl" && args[1] == "delete":
		f.events = append(f.events, ev)
		path, rest := args[2], args[3:]
		var users, tokens bool
		for len(rest) >= 2 {
			switch rest[0] {
			case "--users":
				users = rest[1] == UserID
			case "--tokens":
				tokens = rest[1] == TokenID
			case "--roles":
				if rest[1] != RoleName {
					return "", "bad role", 2, nil
				}
			default:
				return "", "bad acl delete " + rest[0], 2, nil
			}
			rest = rest[2:]
		}
		// PVE checks that the named user and token exist, even on delete.
		if users && !slices.Contains(f.users, UserID) {
			return "", "user '" + UserID + "' does not exist", 255, nil
		}
		if tokens && !slices.Contains(f.tokens[UserID], TokenName) {
			return "", "no such token '" + TokenName + "' for user '" + UserID + "'", 255, nil
		}
		f.acls = slices.DeleteFunc(f.acls, func(e aclEntry) bool {
			return e.Path == path && e.RoleID == RoleName &&
				(users && e.Type == "user" && e.UGID == UserID || tokens && e.Type == "token" && e.UGID == TokenID)
		})
		return "", "", 0, nil
	case name == "/usr/sbin/pveum" && slices.Equal(args, []string{"user", "delete", UserID}):
		f.events = append(f.events, ev)
		if !slices.Contains(f.users, UserID) {
			return "", "no such user", 255, nil
		}
		f.users = slices.DeleteFunc(f.users, func(u string) bool { return u == UserID })
		delete(f.tokens, UserID)
		return "", "", 0, nil
	case name == "/usr/sbin/pveum" && slices.Equal(args, []string{"role", "delete", RoleName}):
		f.events = append(f.events, ev)
		if _, ok := f.roles[RoleName]; !ok {
			return "", "role does not exist", 255, nil
		}
		delete(f.roles, RoleName)
		return "", "", 0, nil
	}
	return f.applyFake.run(ctx, name, args, stdin)
}

// changes are the recorded events without read-only queries.
func (f *uninstallFake) changes() []string {
	var out []string
	for _, e := range f.events {
		if strings.Contains(e, "--output-format json") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func buildUninstallPlan(t *testing.T, env Env, removeToken bool) Plan {
	t.Helper()
	p, err := BuildUninstallPlan(context.Background(), env, removeToken)
	if err != nil {
		t.Fatalf("BuildUninstallPlan: %v", err)
	}
	return p
}

func uninstall(t *testing.T, env Env, removeToken bool) error {
	t.Helper()
	return Uninstall(context.Background(), env, buildUninstallPlan(t, env, removeToken), removeToken)
}

func planKeys(p Plan) []string {
	var out []string
	for _, it := range p.Items {
		s := string(it.Kind) + " " + it.Path
		if it.Backup {
			s += " (backup)"
		}
		out = append(out, s)
	}
	return out
}

var reUninstBackup = regexp.MustCompile(`\.pbp-backup-20261020T120000$`)

func backups(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && reUninstBackup.MatchString(p) {
			rel, _ := filepath.Rel(root, p)
			out = append(out, "/"+rel)
		}
		return nil
	})
	return out
}

func TestUninstallFull(t *testing.T) {
	f, env := newUninstallFake(t)
	p := buildUninstallPlan(t, env, true)
	acls := ACLPaths("pve1", testArgs().Storages)
	want := []string{"file " + SudoersPath, "file " + AuthorizedKeys, "dir " + SSHDir, "file " + NodeYAMLPath, "shell pbp", "token " + TokenID}
	for _, a := range acls {
		want = append(want, "acl "+a)
	}
	want = append(want, "user "+UserID, "role "+RoleName)
	if got := planKeys(p); !slices.Equal(got, want) {
		t.Fatalf("plan\n got %q\nwant %q", got, want)
	}
	for _, it := range p.Items {
		if it.Status != Remove || it.Status.String() != "remove" {
			t.Errorf("%s %s: status %v, want remove", it.Kind, it.Path, it.Status)
		}
	}

	f.events = nil
	if err := Uninstall(context.Background(), env, p, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	// The sudo rule goes first, then the key; the .ssh dir and node.yaml
	// are gone before the shell; PVE last. PVE drops the token's ACL entries
	// with the token, so the ACL deletes name only the user.
	wantEv := []string{
		"/usr/sbin/visudo -c [authorized_keys .ssh node.yaml]",
		"/usr/sbin/usermod -s /usr/sbin/nologin pbp []",
		"/usr/sbin/pveum user token remove pbp@pve proof []",
	}
	for _, a := range acls {
		wantEv = append(wantEv, "/usr/sbin/pveum acl delete "+a+" --users pbp@pve --roles PBPProofer []")
	}
	wantEv = append(wantEv, "/usr/sbin/pveum user delete pbp@pve []", "/usr/sbin/pveum role delete PBPProofer []")
	if got := f.changes(); !slices.Equal(got, wantEv) {
		t.Fatalf("events\n got %q\nwant %q", got, wantEv)
	}

	if b := backups(t, env.Root); len(b) != 0 {
		t.Errorf("backups of setup's own files: %q", b)
	}
	if fi, err := os.Stat(env.path(EtcDir)); err != nil || !fi.IsDir() {
		t.Errorf("/etc/pbp was removed: %v", err)
	}
	for n := range report.Templates() {
		if _, err := os.Stat(filepath.Join(env.path(node.DefaultTemplateDir), n)); err != nil {
			t.Errorf("template %s: %v", n, err)
		}
	}
	if len(f.acls) != 0 || len(f.users) != 0 || len(f.roles) != 0 || len(f.tokens[UserID]) != 0 {
		t.Errorf("PVE state left: acls %v users %v roles %v tokens %v", f.acls, f.users, f.roles, f.tokens)
	}
	if p := buildUninstallPlan(t, env, true); len(p.Items) != 0 {
		t.Errorf("second plan is not empty: %q", planKeys(p))
	}
}

func TestUninstallACLsBeforeTokenGone(t *testing.T) {
	// The token is already gone but its user still holds entries; with the
	// token present but without ACL entries; both: the delete names only
	// the identities that hold entries and exist.
	f, env := newUninstallFake(t)
	f.tokens = map[string][]string{UserID: nil}
	// A stale token entry: the token is gone, so PVE refuses --tokens.
	f.acls = []aclEntry{{"/vms", RoleName, "user", UserID, 1}, {"/vms", "PVEAuditor", "user", UserID, 1}, {"/vms", RoleName, "token", TokenID, 1}}
	p := buildUninstallPlan(t, env, true)
	want := []string{"acl /vms", "user " + UserID, "role " + RoleName}
	if got := planKeys(p)[5:]; !slices.Equal(got, want) {
		t.Fatalf("PVE plan items %q, want %q", got, want)
	}
	f.events = nil
	if err := Uninstall(context.Background(), env, p, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !slices.Contains(f.changes(), "/usr/sbin/pveum acl delete /vms --users pbp@pve --roles PBPProofer []") {
		t.Errorf("events %q", f.changes())
	}
}

func TestUninstallKeepsToken(t *testing.T) {
	f, env := newUninstallFake(t)
	p := buildUninstallPlan(t, env, false)
	want := []string{"file " + SudoersPath, "file " + AuthorizedKeys, "dir " + SSHDir, "file " + NodeYAMLPath, "shell pbp"}
	if got := planKeys(p); !slices.Equal(got, want) {
		t.Fatalf("plan %q, want %q", got, want)
	}
	f.events = nil
	if err := Uninstall(context.Background(), env, p, false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, e := range f.events {
		if strings.HasPrefix(e, "/usr/sbin/pveum") {
			t.Errorf("pveum ran without removeToken: %s", e)
		}
	}
	if len(f.acls) != 10 || !slices.Contains(f.tokens[UserID], TokenName) {
		t.Error("token or ACLs changed")
	}
	if p := buildUninstallPlan(t, env, false); len(p.Items) != 0 {
		t.Errorf("second plan is not empty: %q", planKeys(p))
	}
}

func TestUninstallCleanNode(t *testing.T) {
	_, env := newApplyFake(t, "/usr/sbin/nologin")
	if p := buildUninstallPlan(t, env, true); len(p.Items) != 0 {
		t.Fatalf("plan on a clean node: %q", planKeys(p))
	}
}

// newKey returns a fresh, valid ssh-ed25519 public key with a comment.
func newKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " other"
}

func TestUninstallOrderWithBackups(t *testing.T) {
	f, env := newUninstallFake(t)
	mustWrite(t, env.path(SudoersPath), "pbp ALL=(ALL) NOPASSWD: ALL\n", 0o440)
	mustWrite(t, env.path(AuthorizedKeys), "ssh-ed25519 AAAA bad\n", 0o644)
	mustWrite(t, env.path(NodeYAMLPath), "bridge: x\n", 0o640)
	f.events = nil
	if err := uninstall(t, env, false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	const ts = ".pbp-backup-20261020T120000"
	want := []string{
		"chown " + SudoersPath + ts + " [pbp authorized_keys .ssh node.yaml]",
		"/usr/sbin/visudo -c [authorized_keys .ssh node.yaml]",
		"chown " + AuthorizedKeys + ts + " [authorized_keys .ssh node.yaml]",
		"chown " + NodeYAMLPath + ts + " [.ssh node.yaml]",
		"/usr/sbin/usermod -s /usr/sbin/nologin pbp [.ssh]",
	}
	if got := f.changes(); !slices.Equal(got, want) {
		t.Fatalf("events\n got %q\nwant %q", got, want)
	}
}

func TestUninstallBacksUpEdited(t *testing.T) {
	const edited = "pbp ALL=(ALL) NOPASSWD: ALL\n"
	otherKey := newKey(t)
	for name, tc := range map[string]struct {
		path, content string
		backup        bool
	}{
		"sudoers edited":   {SudoersPath, edited, true},
		"sudoers #include": {SudoersPath, SudoersText + "#includedir /tmp/x\n", true},
		"sudoers comments": {SudoersPath, "# managed by pbp\n" + SudoersText + "\n", false},
		"keys extra line":  {AuthorizedKeys, AuthorizedKeysLine(testKey) + "\n" + otherKey + "\n", true},
		"keys no restrict": {AuthorizedKeys, testKey + "\n", true},
		"keys two spaces":  {AuthorizedKeys, `restrict,command="sudo /usr/bin/pbp node"  ` + testKey + "\n", true},
		"keys bad key":     {AuthorizedKeys, AuthorizedKeysLine("ssh-ed25519 AAAAnotakey") + "\n", true},
		"keys other key":   {AuthorizedKeys, "# old\n" + AuthorizedKeysLine(otherKey) + "\n", false},
		"keys empty":       {AuthorizedKeys, "", false},
		"yaml invalid":     {NodeYAMLPath, "vmid_range: [1, 2]\nbridge: vmbr0\n", true},
		"yaml unknown key": {NodeYAMLPath, string(NodeYAML(990000, 990049, "vmbr99")) + "extra: 1\n", true},
		"yaml other range": {NodeYAMLPath, string(NodeYAML(500, 600, "vmbr0")), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, env := newUninstallFake(t)
			mustWrite(t, env.path(tc.path), tc.content, 0o640)
			p := buildUninstallPlan(t, env, false)
			var it Item
			for _, x := range p.Items {
				if x.Path == tc.path {
					it = x
				}
			}
			if it.Backup != tc.backup {
				t.Fatalf("%s: Backup %v, want %v", tc.path, it.Backup, tc.backup)
			}
			if err := Uninstall(context.Background(), env, p, false); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			if _, err := os.Lstat(env.path(tc.path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s not removed: %v", tc.path, err)
			}
			b := backups(t, env.Root)
			if !tc.backup {
				if len(b) != 0 {
					t.Errorf("backups %q, want none", b)
				}
				return
			}
			if !slices.Equal(b, []string{tc.path + ".pbp-backup-20261020T120000"}) {
				t.Fatalf("backups %q", b)
			}
			if got := readFile(t, env.path(b[0])); got != tc.content {
				t.Errorf("backup holds %q, want %q", got, tc.content)
			}
			if fi, _ := os.Lstat(env.path(b[0])); fi.Mode().Perm() != 0o640 {
				t.Errorf("backup mode %v, want 0640", fi.Mode())
			}
			// A backed-up authorized_keys keeps its .ssh dir.
			if tc.path == AuthorizedKeys {
				if slices.Contains(planKeys(p), "dir "+SSHDir) {
					t.Errorf("plan removes .ssh, which keeps the backup: %q", planKeys(p))
				}
				if _, err := os.Lstat(env.path(SSHDir)); err != nil {
					t.Errorf(".ssh removed with a backup in it: %v", err)
				}
			}
			if p := buildUninstallPlan(t, env, false); len(p.Items) != 0 {
				t.Errorf("second plan is not empty: %q", planKeys(p))
			}
		})
	}
}

func TestUninstallSSHDirNotEmpty(t *testing.T) {
	_, env := newUninstallFake(t)
	mustWrite(t, filepath.Join(env.path(SSHDir), "known_hosts"), "x\n", 0o644)
	p := buildUninstallPlan(t, env, false)
	if slices.Contains(planKeys(p), "dir "+SSHDir) {
		t.Errorf("plan removes a non-empty .ssh: %q", planKeys(p))
	}
	if err := Uninstall(context.Background(), env, p, false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.path(SSHDir), "known_hosts")); err != nil {
		t.Error(err)
	}
}

// gone reports whether path (under Root) no longer exists.
func gone(env Env, path string) bool {
	_, err := os.Lstat(env.path(path))
	return errors.Is(err, os.ErrNotExist)
}

// otherAccessGone checks that the sudo rule and the key not at path were
// still removed (the key only when .ssh itself is usable).
func otherAccessGone(t *testing.T, env Env, path string) {
	t.Helper()
	if path != SudoersPath && !gone(env, SudoersPath) {
		t.Errorf("sudoers left while %s was refused", path)
	}
	if path != AuthorizedKeys && path != SSHDir && !gone(env, AuthorizedKeys) {
		t.Errorf("authorized_keys left while %s was refused", path)
	}
}

func TestUninstallRefusesNonRegular(t *testing.T) {
	for _, path := range []string{SudoersPath, AuthorizedKeys, NodeYAMLPath, SSHDir} {
		t.Run(path, func(t *testing.T) {
			_, env := newUninstallFake(t)
			// For .ssh the symlink points at a directory holding an
			// authorized_keys, which must survive.
			victim, victimFile := filepath.Join(env.Root, "victim"), filepath.Join(env.Root, "victim")
			if path == SSHDir {
				victimFile = filepath.Join(victim, "authorized_keys")
			}
			mustWrite(t, victimFile, "keep\n", 0o600)
			swap := func() {
				t.Helper()
				if err := os.RemoveAll(env.path(path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, env.path(path)); err != nil {
					t.Fatal(err)
				}
			}
			check := func(err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("Uninstall err %v, want a refusal naming %s", err, path)
				}
				if fi, err := os.Lstat(env.path(path)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("symlink at %s was changed: %v", path, err)
				}
				if got := readFile(t, victimFile); got != "keep\n" {
					t.Errorf("victim changed: %q", got)
				}
				if b := backups(t, env.Root); len(b) != 0 {
					t.Errorf("backups %q", b)
				}
				otherAccessGone(t, env, path)
			}

			// At plan time: a Refused item, not a failed plan.
			swap()
			p := buildUninstallPlan(t, env, false)
			var found bool
			for _, it := range p.Items {
				if it.Path == path && it.Status == Refused {
					found = true
					if !strings.Contains(it.Diff, "not a regular file") && !strings.Contains(it.Diff, "not a directory") {
						t.Errorf("refusal %q", it.Diff)
					}
				}
			}
			if !found {
				t.Fatalf("no refused item for %s in %q", path, planKeys(p))
			}
			check(Uninstall(context.Background(), env, p, false))

			// Swapped in after planning: Uninstall checks again.
			if err := os.Remove(env.path(path)); err != nil {
				t.Fatal(err)
			}
			layoutComplete(t, env.Root)
			p = buildUninstallPlan(t, env, false)
			swap()
			check(Uninstall(context.Background(), env, p, false))
		})
	}
}

func TestUninstallAccessBestEffort(t *testing.T) {
	const ts = ".pbp-backup-20261020T120000"
	for _, path := range []string{SudoersPath, AuthorizedKeys} {
		t.Run(path, func(t *testing.T) {
			f, env := newUninstallFake(t)
			// An edited file whose backup name is taken: the O_EXCL backup
			// fails, so this file stays.
			mustWrite(t, env.path(path), "edited\n", 0o440)
			mustWrite(t, env.path(path+ts), "earlier backup\n", 0o600)
			f.events = nil
			err := uninstall(t, env, true)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("err %v, want a failure naming %s", err, path)
			}
			if gone(env, path) {
				t.Errorf("%s removed without a backup", path)
			}
			otherAccessGone(t, env, path)
			// Then it stops: node.yaml, the shell and PVE are untouched.
			if gone(env, NodeYAMLPath) {
				t.Error("continued to node.yaml after an access failure")
			}
			for _, e := range f.changes() {
				if !strings.HasPrefix(e, "/usr/sbin/visudo") && !strings.HasPrefix(e, "chown ") {
					t.Errorf("continued after an access failure: %s", e)
				}
			}
		})
	}
}

func TestUninstallSymlinkedNodeYAML(t *testing.T) {
	f, env := newUninstallFake(t)
	victim := filepath.Join(env.Root, "victim")
	mustWrite(t, victim, "keep\n", 0o600)
	if err := os.Remove(env.path(NodeYAMLPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, env.path(NodeYAMLPath)); err != nil {
		t.Fatal(err)
	}
	p, err := BuildUninstallPlan(context.Background(), env, true)
	if err != nil {
		t.Fatalf("BuildUninstallPlan: %v", err)
	}
	err = Uninstall(context.Background(), env, p, true)
	if err == nil || !strings.Contains(err.Error(), NodeYAMLPath) {
		t.Fatalf("err %v, want a refusal naming %s", err, NodeYAMLPath)
	}
	for _, path := range []string{SudoersPath, AuthorizedKeys, SSHDir} {
		if !gone(env, path) {
			t.Errorf("%s left", path)
		}
	}
	if f.shell != NoLogin || len(f.users) != 0 {
		t.Errorf("shell %s users %v: the rest of uninstall did not run", f.shell, f.users)
	}
	if got := readFile(t, victim); got != "keep\n" {
		t.Errorf("victim changed: %q", got)
	}
}

func TestUninstallNoPbpUser(t *testing.T) {
	_, env := newUninstallFake(t)
	lookup := env.Lookup
	env.Lookup = func(user string) (int, int, string, string, error) {
		if user == PbpUser {
			return 0, 0, "", "", fmt.Errorf("pbp: %w", ErrNoSuchUser)
		}
		return lookup(user)
	}
	p := buildUninstallPlan(t, env, false)
	if slices.Contains(planKeys(p), "shell pbp") {
		t.Errorf("shell item without a pbp user: %q", planKeys(p))
	}
	if err := Uninstall(context.Background(), env, p, false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

// A Lookup failure other than "no such user" (an unreadable /etc/passwd)
// must not silently drop the shell item: it is Refused and reported, while
// the access files are still removed.
func TestUninstallLookupFailureRefusesShell(t *testing.T) {
	_, env := newUninstallFake(t)
	lookup := env.Lookup
	env.Lookup = func(user string) (int, int, string, string, error) {
		if user == PbpUser {
			return 0, 0, "", "", errors.New("read /etc/passwd: permission denied")
		}
		return lookup(user)
	}
	p := buildUninstallPlan(t, env, false)
	var shell *Item
	for i := range p.Items {
		if p.Items[i].Kind == KindShell {
			shell = &p.Items[i]
		}
	}
	if shell == nil || shell.Status != Refused || !strings.Contains(shell.Diff, "permission denied") {
		t.Fatalf("shell item %+v, want Refused naming the lookup error", shell)
	}
	err := Uninstall(context.Background(), env, p, false)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Uninstall err %v, want the lookup failure reported", err)
	}
	for _, f := range []string{SudoersPath, AuthorizedKeys} {
		if _, err := os.Lstat(env.path(f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not removed: %v", f, err)
		}
	}
}

func TestUninstallVisudoFailure(t *testing.T) {
	f, env := newUninstallFake(t)
	f.visudoFinal = 1
	err := uninstall(t, env, false)
	if err == nil || !strings.Contains(err.Error(), "visudo -c") {
		t.Fatalf("err %v, want a visudo -c failure", err)
	}
	// The failure is reported, and access is still removed.
	if _, err := os.Lstat(env.path(AuthorizedKeys)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("authorized_keys left after visudo failure: %v", err)
	}
	if _, err := os.Lstat(env.path(SudoersPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sudoers restored: %v", err)
	}
	if f.shell != "/usr/sbin/nologin" {
		t.Errorf("shell %s", f.shell)
	}
}

func TestUninstallStopsOnFailure(t *testing.T) {
	f, env := newUninstallFake(t)
	f.fail = func(name string, args []string) bool { return name == "/usr/sbin/usermod" }
	err := uninstall(t, env, true)
	if err == nil || !strings.Contains(err.Error(), "pbp login shell") {
		t.Fatalf("err %v, want a failure naming the shell item", err)
	}
	for _, e := range f.events {
		if strings.Contains(e, "token remove") {
			t.Errorf("continued after a failure: %s", e)
		}
	}
}
