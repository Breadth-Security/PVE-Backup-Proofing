package nodesetup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

const testSecret = "0b5e3f6a-1d2c-4e8f-9a7b-3c2d1e0f9a8b"

// applyFake is a stateful node: it answers the planner's pveum queries
// through fakePVE, applies the pveum mutations to that state, and runs
// usermod and visudo. Every Run and Chown is recorded in events, in order.
type applyFake struct {
	*fakePVE
	t      *testing.T
	root   string
	shell  string
	events []string
	argv   [][]string
	// fail, when it returns true for a command, makes it exit 1.
	fail func(name string, args []string) bool
	// visudoFinal is the exit of `visudo -c`.
	visudoFinal int
	secret      string
}

func newApplyFake(t *testing.T, shell string) (*applyFake, Env) {
	t.Helper()
	f := &applyFake{fakePVE: &fakePVE{}, t: t, shell: shell, secret: testSecret}
	env := testEnv(t, f.fakePVE, shell)
	f.root = env.Root
	lookup := env.Lookup
	env.Lookup = func(user string) (int, int, string, string, error) {
		uid, gid, home, sh, err := lookup(user)
		if user == PbpUser {
			sh = f.shell
		}
		return uid, gid, home, sh, err
	}
	env.Run = f.run
	env.Chown = func(path string, uid, gid int) error {
		rel, err := filepath.Rel(f.root, path)
		if err != nil {
			t.Errorf("chown outside root: %s", path)
		}
		f.events = append(f.events, "chown /"+rel)
		return nil
	}
	// Tests run unprivileged: record the fd's path instead of chowning.
	env.Fchown = func(fd, uid, gid int) error {
		p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		if err != nil {
			t.Errorf("fchown: %v", err)
		}
		return env.Chown(p, uid, gid)
	}
	return f, env
}

func (f *applyFake) run(ctx context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	f.argv = append(f.argv, append([]string{name}, args...))
	ev := name + " " + strings.Join(args, " ")
	if f.fail != nil && f.fail(name, args) {
		f.events = append(f.events, ev)
		return "", "injected failure", 1, nil
	}
	switch name {
	case "/usr/sbin/usermod":
		f.events = append(f.events, ev)
		if !slices.Equal(args, []string{"-s", "/bin/sh", "pbp"}) {
			return "", "bad usermod", 2, nil
		}
		f.shell = "/bin/sh"
		return "", "", 0, nil
	case "/usr/sbin/visudo":
		if len(args) == 2 && args[0] == "-cf" {
			tmp := args[1]
			if !strings.Contains(filepath.Base(tmp), ".") || filepath.Dir(tmp) != filepath.Join(f.root, "etc/sudoers.d") {
				f.t.Errorf("visudo -cf on %s: want a dotted name in /etc/sudoers.d", tmp)
			}
			fi, err := os.Stat(tmp)
			if err != nil || fi.Mode().Perm() != 0o440 {
				f.t.Errorf("visudo -cf on %s: stat %v, %v; want mode 0440", tmp, fi, err)
			}
			f.events = append(f.events, "visudo -cf")
			return "", "", 0, nil
		}
		f.events = append(f.events, ev)
		if slices.Equal(args, []string{"-c"}) {
			return "", "", f.visudoFinal, nil
		}
		return "", "bad visudo", 2, nil
	case "/usr/sbin/pveum":
	default:
		f.events = append(f.events, ev)
		return "", "unexpected command " + name, 127, nil
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	n := len(args)
	switch {
	case n == 5 && args[0] == "role" && (args[1] == "add" || args[1] == "modify") && args[2] == RoleName && args[3] == "--privs":
		f.events = append(f.events, ev)
		if f.roles == nil {
			f.roles = map[string]string{}
		}
		_, exists := f.roles[RoleName]
		if exists != (args[1] == "modify") {
			return "", "role exists state mismatch", 255, nil
		}
		f.roles[RoleName] = args[4]
		return "", "", 0, nil
	case slices.Equal(args, []string{"user", "add", UserID, "--comment", "PBP proofing"}):
		f.events = append(f.events, ev)
		if slices.Contains(f.users, UserID) {
			return "", "user exists", 255, nil
		}
		f.users = append(f.users, UserID)
		return "", "", 0, nil
	case slices.Equal(args, []string{"user", "token", "remove", UserID, TokenName}):
		f.events = append(f.events, ev)
		if !slices.Contains(f.tokens[UserID], TokenName) {
			return "", "no such token", 255, nil
		}
		f.tokens[UserID] = slices.DeleteFunc(f.tokens[UserID], func(s string) bool { return s == TokenName })
		// PVE drops a removed token's ACL entries, and re-adding the token
		// does not bring them back (verified on pve1).
		f.acls = slices.DeleteFunc(f.acls, func(e aclEntry) bool { return e.Type == "token" && e.UGID == TokenID })
		return "", "", 0, nil
	case slices.Equal(args, []string{"user", "token", "add", UserID, TokenName, "--privsep", "1", "--comment", "PBP proof token", "--output-format", "json"}):
		f.events = append(f.events, ev)
		if !slices.Contains(f.users, UserID) {
			return "", "no such user", 255, nil
		}
		if slices.Contains(f.tokens[UserID], TokenName) {
			return "", "token already exists", 255, nil
		}
		if f.tokens == nil {
			f.tokens = map[string][]string{}
		}
		f.tokens[UserID] = append(f.tokens[UserID], TokenName)
		f.nosep = false
		return j(map[string]any{"full-tokenid": TokenID, "info": map[string]any{"privsep": 1}, "value": f.secret}), "", 0, nil
	case n == 9 && args[0] == "acl" && args[1] == "modify" && slices.Equal(args[3:], []string{"--users", UserID, "--tokens", TokenID, "--roles", RoleName}):
		f.events = append(f.events, ev)
		f.acls = append(f.acls, aclEntry{args[2], RoleName, "user", UserID, 1}, aclEntry{args[2], RoleName, "token", TokenID, 1})
		return "", "", 0, nil
	}
	return f.fakePVE.run(ctx, name, args, stdin)
}

func applyArgs() nodeproto.SetupArgs { return testArgs() }

func apply(t *testing.T, env Env, a nodeproto.SetupArgs, o ApplyOpts) (string, error) {
	t.Helper()
	p := buildPlan(t, env, a)
	return Apply(context.Background(), env, p, a, "pve1", o)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyFresh(t *testing.T) {
	f, env := newApplyFake(t, "/usr/sbin/nologin")
	a := applyArgs()
	secret, err := apply(t, env, a, ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if secret != testSecret {
		t.Errorf("secret %q, want %q", secret, testSecret)
	}
	if p := buildPlan(t, env, a); !p.Complete() {
		t.Fatalf("plan after Apply is not complete:\n%s", dump(p))
	}

	privs := strings.Join(RolePrivs, ",")
	acl := func(p string) string {
		return "/usr/sbin/pveum acl modify " + p + " --users pbp@pve --tokens pbp@pve!proof --roles PBPProofer"
	}
	want := []string{
		"chown /etc/pbp",
		"chown /etc/pbp/TMP", // node.yaml's temp file, renamed into place
		"/usr/sbin/usermod -s /bin/sh pbp",
		"chown /etc/sudoers.d/" + "TMP",
		"visudo -cf",
		"/usr/sbin/visudo -c",
		"/usr/sbin/pveum role add PBPProofer --privs " + privs,
		"/usr/sbin/pveum user add pbp@pve --comment PBP proofing",
		"/usr/sbin/pveum user token add pbp@pve proof --privsep 1 --comment PBP proof token --output-format json",
		acl("/vms"), acl("/storage/local-zfs"), acl("/storage/pbs1"), acl("/nodes/pve1"), acl("/sdn/zones/localnetwork"),
		"chown /var/lib/pbp/.ssh",
		"chown /var/lib/pbp/.ssh/TMP", // authorized_keys' temp file: the last write
	}
	var got []string
	for _, e := range f.events {
		if strings.Contains(e, "--output-format json") && !strings.Contains(e, "token add") {
			continue // the planner's and Apply's read-only queries
		}
		e = reTmp.ReplaceAllString(e, "/TMP") // dotted temp names, renamed into place
		got = append(got, e)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}

	// No temp file is left behind (in /etc/sudoers.d a stray one could be live).
	_ = filepath.WalkDir(env.Root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".pbp.tmp-") {
			t.Errorf("left behind: %s", p)
		}
		return nil
	})
	for path, mode := range map[string]os.FileMode{
		EtcDir: 0o750 | os.ModeDir, NodeYAMLPath: 0o640, SudoersPath: 0o440,
		SSHDir: 0o755 | os.ModeDir, AuthorizedKeys: 0o644,
	} {
		fi, err := os.Lstat(env.path(path))
		if err != nil || fi.Mode() != mode {
			t.Errorf("%s: %v %v, want %v", path, fi.Mode(), err, mode)
		}
	}
	if got := readFile(t, env.path(SudoersPath)); got != SudoersText {
		t.Errorf("sudoers %q", got)
	}
	for _, argv := range f.argv {
		for _, x := range argv {
			if strings.Contains(x, testSecret) {
				t.Errorf("secret in argv %q", argv)
			}
		}
	}
}

func TestApplyRefusesDiffers(t *testing.T) {
	t.Run("differs without replace", func(t *testing.T) {
		f, env := newApplyFake(t, "/usr/sbin/nologin")
		mustWrite(t, env.path(SudoersPath), "pbp ALL=(ALL) NOPASSWD: ALL\n", 0o440)
		mustMkdir(t, env.path(EtcDir), 0o755)
		p := buildPlan(t, env, applyArgs())
		f.events = nil
		_, err := Apply(context.Background(), env, p, applyArgs(), "pve1", ApplyOpts{})
		if err == nil || !strings.Contains(err.Error(), "sudoers rule") || !strings.Contains(err.Error(), "/etc/pbp") || !strings.Contains(err.Error(), "--replace") {
			t.Fatalf("err %v, want a refusal naming sudoers rule and /etc/pbp", err)
		}
		if len(f.events) != 0 {
			t.Errorf("changes made despite refusal: %q", f.events)
		}
		if _, err := os.Stat(env.path(NodeYAMLPath)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("node.yaml written despite refusal: %v", err)
		}
	})
	t.Run("check-only home even with replace", func(t *testing.T) {
		f, env := newApplyFake(t, "/usr/sbin/nologin")
		mustMkdir(t, env.path(PbpHome), 0o755)
		p := buildPlan(t, env, applyArgs())
		f.events = nil
		_, err := Apply(context.Background(), env, p, applyArgs(), "pve1", ApplyOpts{Replace: true})
		if err == nil || !strings.Contains(err.Error(), "pbp home") {
			t.Fatalf("err %v, want a refusal naming pbp home", err)
		}
		if len(f.events) != 0 {
			t.Errorf("changes made despite refusal: %q", f.events)
		}
	})
}

var reTmp = regexp.MustCompile(`/\.pbp\.tmp-\d+$`)

var reBackup = regexp.MustCompile(`\.pbp-backup-\d{8}T\d{6}$`)

func TestApplyReplaceBacksUp(t *testing.T) {
	f, env := newApplyFake(t, "/bin/bash")
	a := applyArgs()
	layoutComplete(t, env.Root)
	f.complete("pve1")
	oldYAML := "vmid_range: [1, 2]\nbridge: vmbr0\nrequest_timeout: 15m\n"
	mustWrite(t, env.path(NodeYAMLPath), oldYAML, 0o600)
	oldKeys := "ssh-ed25519 AAAAother other\n"
	mustWrite(t, env.path(AuthorizedKeys), oldKeys, 0o644)
	mustMkdir(t, env.path(SSHDir), 0o700)
	f.roles[RoleName] = "VM.Audit"

	secret, err := apply(t, env, a, ApplyOpts{Replace: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if secret != "" {
		t.Errorf("secret %q for an existing token", secret)
	}
	if p := buildPlan(t, env, a); !p.Complete() {
		t.Fatalf("plan after Apply is not complete:\n%s", dump(p))
	}
	if !slices.Contains(f.events, "/usr/sbin/pveum role modify PBPProofer --privs "+strings.Join(RolePrivs, ",")) {
		t.Errorf("no role modify in %q", f.events)
	}
	for path, old := range map[string]struct {
		content string
		mode    os.FileMode
	}{NodeYAMLPath: {oldYAML, 0o600}, AuthorizedKeys: {oldKeys, 0o644}} {
		matches, _ := filepath.Glob(env.path(path) + ".pbp-backup-*")
		if len(matches) != 1 || !reBackup.MatchString(matches[0]) {
			t.Fatalf("%s backups %q", path, matches)
		}
		if !strings.HasSuffix(matches[0], ".pbp-backup-20261020T120000") {
			t.Errorf("backup name %s does not use Env.Now", matches[0])
		}
		if got := readFile(t, matches[0]); got != old.content {
			t.Errorf("backup of %s holds %q, want %q", path, got, old.content)
		}
		if fi, _ := os.Stat(matches[0]); fi.Mode() != old.mode {
			t.Errorf("backup of %s mode %v, want %v", path, fi.Mode(), old.mode)
		}
		if !slices.Contains(f.events, "chown /"+strings.TrimPrefix(matches[0], env.Root+"/")) {
			t.Errorf("backup %s owner not preserved: %q", matches[0], f.events)
		}
	}
	// The dir is fixed in place, without a backup.
	if m, _ := filepath.Glob(env.path(SSHDir) + ".pbp-backup-*"); len(m) != 0 {
		t.Errorf("dir backed up: %q", m)
	}
}

func TestApplyVisudoFailureRestores(t *testing.T) {
	t.Run("replace restores the old file", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		old := "Defaults:pbp env_reset\n"
		mustWrite(t, env.path(SudoersPath), old, 0o440)
		f.visudoFinal = 1
		_, err := apply(t, env, applyArgs(), ApplyOpts{Replace: true})
		if err == nil || !strings.Contains(err.Error(), "visudo") {
			t.Fatalf("err %v, want a visudo failure", err)
		}
		if got := readFile(t, env.path(SudoersPath)); got != old {
			t.Errorf("sudoers %q, want the old %q restored", got, old)
		}
		if fi, _ := os.Stat(env.path(SudoersPath)); fi.Mode().Perm() != 0o440 {
			t.Errorf("restored mode %v", fi.Mode())
		}
		if _, err := os.Stat(env.path(AuthorizedKeys)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("authorized_keys written after a failure: %v", err)
		}
		if slices.ContainsFunc(f.events, func(e string) bool { return strings.Contains(e, "pveum role") }) {
			t.Errorf("continued after the failure: %q", f.events)
		}
	})
	t.Run("create removes the new file", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		f.visudoFinal = 1
		if _, err := apply(t, env, applyArgs(), ApplyOpts{}); err == nil {
			t.Fatal("no error")
		}
		ents, _ := os.ReadDir(env.path("/etc/sudoers.d"))
		if len(ents) != 0 {
			t.Errorf("sudoers.d holds %v, want nothing", ents)
		}
	})
	t.Run("syntax check failure leaves nothing", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		f.fail = func(name string, args []string) bool { return name == "/usr/sbin/visudo" && args[0] == "-cf" }
		if _, err := apply(t, env, applyArgs(), ApplyOpts{}); err == nil || !strings.Contains(err.Error(), "visudo") {
			t.Fatalf("err %v", err)
		}
		ents, _ := os.ReadDir(env.path("/etc/sudoers.d"))
		if len(ents) != 0 {
			t.Errorf("sudoers.d holds %v, want nothing", ents)
		}
	})
}

func TestApplyInterrupted(t *testing.T) {
	f, env := newApplyFake(t, "/usr/sbin/nologin")
	a := applyArgs()
	f.fail = func(name string, args []string) bool {
		return len(args) > 2 && args[0] == "user" && args[1] == "token" && args[2] == "add"
	}
	_, err := apply(t, env, a, ApplyOpts{})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err %v, want a token failure", err)
	}
	if _, err := os.Stat(env.path(AuthorizedKeys)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authorized_keys exists after an interrupted apply: %v", err)
	}
	f.fail = nil
	secret, err := apply(t, env, a, ApplyOpts{})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if secret != testSecret {
		t.Errorf("secret %q", secret)
	}
	if p := buildPlan(t, env, a); !p.Complete() {
		t.Fatalf("plan after rerun is not complete:\n%s", dump(p))
	}
}

// A failure after the token was added still returns its one-time secret.
func TestApplyFailureAfterTokenKeepsSecret(t *testing.T) {
	f, env := newApplyFake(t, "/usr/sbin/nologin")
	f.fail = func(name string, args []string) bool {
		return len(args) > 2 && args[0] == "acl" && args[1] == "modify" && args[2] == "/storage/pbs1"
	}
	secret, err := apply(t, env, applyArgs(), ApplyOpts{})
	if err == nil || !strings.Contains(err.Error(), "ACL /storage/pbs1") {
		t.Fatalf("err %v, want the ACL failure", err)
	}
	if secret != testSecret {
		t.Errorf("secret %q with the error, want %q", secret, testSecret)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("secret in error: %v", err)
	}
	if _, err := os.Stat(env.path(AuthorizedKeys)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("authorized_keys written after a failure: %v", err)
	}
}

func TestApplyExistingTokenNoSecret(t *testing.T) {
	f, env := newApplyFake(t, "/usr/sbin/nologin")
	f.complete("pve1")
	secret, err := apply(t, env, applyArgs(), ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if secret != "" {
		t.Errorf("secret %q, want none for an existing token", secret)
	}
	if slices.ContainsFunc(f.events, func(e string) bool {
		return strings.Contains(e, "pveum user token add") || strings.Contains(e, "token remove")
	}) {
		t.Errorf("token touched: %q", f.events)
	}
	if p := buildPlan(t, env, applyArgs()); !p.Complete() {
		t.Fatalf("not complete:\n%s", dump(p))
	}
}

func TestApplyRotate(t *testing.T) {
	removeThenAdd := func(t *testing.T, events []string) {
		t.Helper()
		rm := slices.Index(events, "/usr/sbin/pveum user token remove pbp@pve proof")
		add := slices.IndexFunc(events, func(e string) bool { return strings.HasPrefix(e, "/usr/sbin/pveum user token add") })
		if rm < 0 || add < rm {
			t.Errorf("want token remove then add: %q", events)
		}
	}
	t.Run("rotate", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		f.complete("pve1")
		f.secret = "new-secret-value"
		a := applyArgs()
		a.CreateToken, a.RotateToken = false, true
		secret, err := apply(t, env, a, ApplyOpts{})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if secret != "new-secret-value" {
			t.Errorf("secret %q", secret)
		}
		removeThenAdd(t, f.events)
		a.RotateToken = false
		a.CreateToken = true
		if p := buildPlan(t, env, a); !p.Complete() {
			t.Fatalf("not complete:\n%s", dump(p))
		}
	})
	t.Run("rotate with the token absent adds it", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		a := applyArgs()
		a.CreateToken, a.RotateToken = false, true
		secret, err := apply(t, env, a, ApplyOpts{})
		if err != nil || secret != testSecret {
			t.Fatalf("Apply: %q, %v", secret, err)
		}
		if slices.ContainsFunc(f.events, func(e string) bool { return strings.Contains(e, "token remove") }) {
			t.Errorf("removed an absent token: %q", f.events)
		}
	})
	t.Run("privsep 0 with replace", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		f.complete("pve1")
		f.nosep = true
		a := applyArgs()
		if _, err := apply(t, env, a, ApplyOpts{}); err == nil || !strings.Contains(err.Error(), "API token") {
			t.Fatalf("err %v, want a refusal naming the token", err)
		}
		secret, err := apply(t, env, a, ApplyOpts{Replace: true})
		if err != nil || secret != testSecret {
			t.Fatalf("Apply: %q, %v", secret, err)
		}
		removeThenAdd(t, f.events)
		if p := buildPlan(t, env, a); !p.Complete() {
			t.Fatalf("not complete:\n%s", dump(p))
		}
	})
	t.Run("bad token output never leaks", func(t *testing.T) {
		f, env := newApplyFake(t, "/bin/sh")
		f.secret = "" // the fake then returns no value
		_, err := apply(t, env, applyArgs(), ApplyOpts{})
		if err == nil || strings.Contains(err.Error(), "full-tokenid") {
			t.Fatalf("err %v: want an error without the command's output", err)
		}
	})
}

// The templates item writes the way node.InstallTemplates does.
func TestApplyTemplates(t *testing.T) {
	_, env := newApplyFake(t, "/bin/sh")
	mustWrite(t, filepath.Join(env.Root, node.DefaultTemplateDir, "pbp-body.txt.hbs"), "old", 0o644)
	if _, err := apply(t, env, applyArgs(), ApplyOpts{Replace: true}); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(env.Root, node.DefaultTemplateDir, "*.pbp-backup-*"))
	if len(m) != 1 || readFile(t, m[0]) != "old" {
		t.Errorf("template backups %q", m)
	}
	if p := buildPlan(t, env, applyArgs()); item(t, p, KindTemplates, TemplateDir).Status != Correct {
		t.Errorf("templates not installed:\n%s", dump(p))
	}
}

// A pre-placed symlink at a backup name is never written through.
func TestApplyBackupNeverFollowsSymlink(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"file", NodeYAMLPath},
		{"template", filepath.Join(TemplateDir, "pbp-body.txt.hbs")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, env := newApplyFake(t, "/bin/sh")
			mustWrite(t, env.path(tc.path), "old\n", 0o640)
			victim := filepath.Join(t.TempDir(), "victim")
			mustWrite(t, victim, "precious\n", 0o600)
			bak := env.path(tc.path) + ".pbp-backup-20261020T120000"
			if err := os.Symlink(victim, bak); err != nil {
				t.Fatal(err)
			}
			if _, err := apply(t, env, applyArgs(), ApplyOpts{Replace: true}); err == nil {
				t.Fatal("no error with a symlink at the backup path")
			}
			if got := readFile(t, victim); got != "precious\n" {
				t.Errorf("symlink target written: %q", got)
			}
			if got := readFile(t, env.path(tc.path)); got != "old\n" {
				t.Errorf("%s replaced without a backup: %q", tc.path, got)
			}
		})
	}
}

// A path that is not a regular file is refused, not backed up or replaced.
func TestApplyRefusesNonRegular(t *testing.T) {
	for _, path := range []string{NodeYAMLPath, SudoersPath} {
		t.Run(path, func(t *testing.T) {
			f, env := newApplyFake(t, "/bin/sh")
			mustMkdir(t, env.path(EtcDir), 0o750)
			target := filepath.Join(t.TempDir(), "target")
			mustWrite(t, target, "x\n", 0o644)
			if err := os.Symlink(target, env.path(path)); err != nil {
				t.Fatal(err)
			}
			_, err := apply(t, env, applyArgs(), ApplyOpts{Replace: true})
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("err %v, want a refusal naming %s", err, path)
			}
			if fi, err := os.Lstat(env.path(path)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s changed: %v %v", path, fi, err)
			}
			if m, _ := filepath.Glob(env.path(path) + ".pbp-backup-*"); len(m) != 0 {
				t.Errorf("backed up: %q", m)
			}
			if readFile(t, target) != "x\n" {
				t.Error("target written")
			}
			_ = f
		})
	}
}

func TestPlanRefusal(t *testing.T) {
	differs := Plan{Items: []Item{{Name: "sudoers rule", Path: SudoersPath, Status: Differs}}}
	home := Plan{Items: []Item{{Name: "pbp home", Path: PbpHome, Status: Differs, CheckOnly: true}}}
	ok := Plan{Items: []Item{{Name: "x", Path: "/x", Status: Create}, {Name: "pbp home", Path: PbpHome, Status: Correct, CheckOnly: true}}}
	if err := differs.Refusal(ApplyOpts{}); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("differs: %v", err)
	}
	if err := differs.Refusal(ApplyOpts{Replace: true}); err != nil {
		t.Errorf("differs with replace: %v", err)
	}
	if err := home.Refusal(ApplyOpts{Replace: true}); err == nil || !strings.Contains(err.Error(), "fix it by hand") {
		t.Errorf("home: %v", err)
	}
	if err := ok.Refusal(ApplyOpts{}); err != nil {
		t.Errorf("ok: %v", err)
	}
}
