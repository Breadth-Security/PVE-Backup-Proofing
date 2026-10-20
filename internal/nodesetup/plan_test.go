package nodesetup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl pbp"

func testArgs() nodeproto.SetupArgs {
	return nodeproto.SetupArgs{
		VMIDRange:   [2]int{990000, 990049},
		Bridge:      "vmbr99",
		PubKey:      testKey,
		Storages:    []string{"local-zfs", "pbs1"},
		CreateToken: true,
	}
}

type aclEntry struct {
	Path      string `json:"path"`
	RoleID    string `json:"roleid"`
	Type      string `json:"type"`
	UGID      string `json:"ugid"`
	Propagate int    `json:"propagate"`
}

// fakePVE answers `pveum … --output-format json` from in-memory state.
type fakePVE struct {
	roles  map[string]string   // roleid -> privs (comma separated)
	users  []string            // userids
	tokens map[string][]string // userid -> token names
	nosep  bool                // tokens are listed with privsep 0
	acls   []aclEntry
	calls  []string
}

func (f *fakePVE) run(_ context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "/usr/sbin/pveum" {
		return "", "unexpected command " + name, 127, nil
	}
	if n := len(args); n < 2 || args[n-2] != "--output-format" || args[n-1] != "json" {
		return "", "missing --output-format json", 2, nil
	}
	args = args[:len(args)-2]
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	switch strings.Join(args, " ") {
	case "role list":
		out := []map[string]any{{"roleid": "PVEAdmin", "privs": "Sys.Audit", "special": 1}}
		for id, p := range f.roles {
			out = append(out, map[string]any{"roleid": id, "privs": p, "special": 0})
		}
		return j(out), "", 0, nil
	case "user list":
		out := []map[string]any{{"userid": "root@pam", "enable": 1}}
		for _, u := range f.users {
			out = append(out, map[string]any{"userid": u, "enable": 1})
		}
		return j(out), "", 0, nil
	case "acl list":
		if f.acls == nil {
			return "[]", "", 0, nil
		}
		return j(f.acls), "", 0, nil
	}
	if len(args) == 4 && args[0] == "user" && args[1] == "token" && args[2] == "list" {
		if !slices.Contains(f.users, args[3]) {
			return "", "no such user ('" + args[3] + "')\n", 255, nil
		}
		out := []map[string]any{}
		for _, t := range f.tokens[args[3]] {
			sep := 1
			if f.nosep {
				sep = 0
			}
			out = append(out, map[string]any{"comment": "PBP proof token", "expire": 0, "tokenid": t, "privsep": sep})
		}
		return j(out), "", 0, nil
	}
	return "", "unexpected pveum " + strings.Join(args, " "), 2, nil
}

// complete returns the PVE state setup produces for testArgs.
func (f *fakePVE) complete(nodeName string) {
	f.roles = map[string]string{RoleName: strings.Join(RolePrivs, ",")}
	f.users = []string{UserID}
	f.tokens = map[string][]string{UserID: {TokenName}}
	f.acls = nil
	for _, p := range ACLPaths(nodeName, testArgs().Storages) {
		f.acls = append(f.acls,
			aclEntry{p, RoleName, "user", UserID, 1},
			aclEntry{p, RoleName, "token", TokenID, 1})
	}
}

// testEnv is an Env on a temp root. Owner checks use the test process's own
// uid/gid for both root and pbp, since tests cannot chown.
func testEnv(t *testing.T, pve *fakePVE, shell string) Env {
	t.Helper()
	root := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	mustMkdir(t, filepath.Join(root, "var/lib/pbp"), 0o700)
	mustMkdir(t, filepath.Join(root, "etc/sudoers.d"), 0o750)
	return Env{
		Root: root,
		Run:  pve.run,
		Chown: func(string, int, int) error {
			return errors.New("chown not expected in planning")
		},
		Lookup: func(user string) (int, int, string, string, error) {
			switch user {
			case "root":
				return uid, gid, "/root", "/bin/bash", nil
			case "pbp":
				return uid, gid, "/var/lib/pbp", shell, nil
			}
			return 0, 0, "", "", errors.New("unknown user " + user)
		},
		Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
}

func mustMkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// layoutComplete lays out the temp root exactly per the Global Constraints.
func layoutComplete(t *testing.T, root string) {
	t.Helper()
	a := testArgs()
	mustMkdir(t, filepath.Join(root, "etc/pbp"), 0o750)
	mustWrite(t, filepath.Join(root, "etc/pbp/node.yaml"), string(NodeYAML(a.VMIDRange[0], a.VMIDRange[1], a.Bridge)), 0o640)
	mustWrite(t, filepath.Join(root, "etc/sudoers.d/pbp"), SudoersText, 0o440)
	for n, b := range report.Templates() {
		mustWrite(t, filepath.Join(root, node.DefaultTemplateDir, n), string(b), 0o640)
	}
	mustMkdir(t, filepath.Join(root, "var/lib/pbp/.ssh"), 0o755)
	mustWrite(t, filepath.Join(root, "var/lib/pbp/.ssh/authorized_keys"), AuthorizedKeysLine(testKey)+"\n", 0o644)
}

func buildPlan(t *testing.T, env Env, a nodeproto.SetupArgs) Plan {
	t.Helper()
	p, err := BuildPlan(context.Background(), env, a, "pve1")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func item(t *testing.T, p Plan, k Kind, path string) Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Kind == k && it.Path == path {
			return it
		}
	}
	t.Fatalf("no %s item %q in plan:\n%s", k, path, dump(p))
	return Item{}
}

func dump(p Plan) string {
	var sb strings.Builder
	for _, it := range p.Items {
		sb.WriteString(string(it.Kind) + " " + it.Path + ": " + it.Status.String() + "\n")
		if it.Diff != "" {
			sb.WriteString("  " + strings.ReplaceAll(it.Diff, "\n", "\n  ") + "\n")
		}
	}
	return sb.String()
}

func TestPlanFreshNode(t *testing.T) {
	pve := &fakePVE{}
	env := testEnv(t, pve, "/usr/sbin/nologin")
	p := buildPlan(t, env, testArgs())

	type key struct {
		k    Kind
		path string
	}
	want := []key{
		{KindDir, "/etc/pbp"},
		{KindFile, "/etc/pbp/node.yaml"},
		{KindShell, "pbp"},
		{KindHome, "/var/lib/pbp"},
		{KindFile, "/etc/sudoers.d/pbp"},
		{KindTemplates, node.DefaultTemplateDir},
		{KindRole, RoleName},
		{KindUser, UserID},
		{KindToken, TokenID},
		{KindACL, "/vms"},
		{KindACL, "/storage/local-zfs"},
		{KindACL, "/storage/pbs1"},
		{KindACL, "/nodes/pve1"},
		{KindACL, "/sdn/zones/localnetwork"},
		{KindDir, "/var/lib/pbp/.ssh"},
		{KindFile, "/var/lib/pbp/.ssh/authorized_keys"},
	}
	var got []key
	for _, it := range p.Items {
		got = append(got, key{it.Kind, it.Path})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items (apply order):\n got %v\nwant %v", got, want)
	}
	for _, it := range p.Items {
		wantStatus := Create
		if it.Kind == KindHome {
			wantStatus = Correct // check-only: the .deb made it right
			if !it.CheckOnly {
				t.Errorf("home item must be check-only")
			}
		}
		if it.Status != wantStatus {
			t.Errorf("%s %s: status %v, want %v (diff %q)", it.Kind, it.Path, it.Status, wantStatus, it.Diff)
		}
	}
	if p.Complete() {
		t.Error("fresh plan reports Complete")
	}

	// Desired values Apply needs travel on the items.
	sud := item(t, p, KindFile, "/etc/sudoers.d/pbp")
	if string(sud.Content) != SudoersText || sud.Mode != 0o440 || sud.Owner != "root" || sud.Group != "root" {
		t.Errorf("sudoers item = %+v", sud)
	}
	ny := item(t, p, KindFile, "/etc/pbp/node.yaml")
	if ny.Mode != 0o640 || ny.Owner != "root" || ny.Group != "pbp" || !strings.Contains(string(ny.Content), "request_timeout: 15m") {
		t.Errorf("node.yaml item = %+v", ny)
	}
	ak := item(t, p, KindFile, "/var/lib/pbp/.ssh/authorized_keys")
	if string(ak.Content) != `restrict,command="sudo /usr/bin/pbp node" `+testKey+"\n" {
		t.Errorf("authorized_keys content = %q", ak.Content)
	}
}

func TestPlanItemsOnlyWithToken(t *testing.T) {
	pve := &fakePVE{}
	env := testEnv(t, pve, "/bin/sh")
	a := testArgs()
	a.CreateToken = false
	a.Storages = nil
	for _, it := range buildPlan(t, env, a).Items {
		switch it.Kind {
		case KindRole, KindUser, KindToken, KindACL:
			t.Errorf("unexpected %s item without --create-token or --storages", it.Kind)
		}
	}
	if len(pve.calls) != 0 {
		t.Errorf("pveum called without token items: %v", pve.calls)
	}

	// ACLs name the token, so an absent token is created for them.
	a.Storages = []string{"pbs1"}
	p := buildPlan(t, env, a)
	if tok := item(t, p, KindToken, TokenID); tok.Status != Create {
		t.Errorf("absent token with ACLs planned: %v", tok.Status)
	}
	item(t, p, KindRole, RoleName)
	item(t, p, KindACL, "/storage/pbs1")

	// A present token is left alone without --create-token.
	pve.complete("pve1")
	for _, it := range buildPlan(t, env, a).Items {
		if it.Kind == KindToken {
			t.Error("token item for a present token without --create-token")
		}
	}
	pve.users = nil // the user (and so the token) absent
	item(t, buildPlan(t, env, a), KindToken, TokenID)

	a.RotateToken = true
	item(t, buildPlan(t, env, a), KindToken, TokenID)
}

func TestPlanComplete(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	p := buildPlan(t, env, testArgs())
	if !p.Complete() {
		t.Fatalf("plan not complete:\n%s", dump(p))
	}
}

func TestPlanRotateTokenNotComplete(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	a := testArgs()
	a.RotateToken = true
	p := buildPlan(t, env, a)
	if tok := item(t, p, KindToken, TokenID); tok.Status != Create || tok.Diff == "" {
		t.Errorf("rotate on existing token: %+v", tok)
	}
	if p.Complete() {
		t.Error("--rotate-token plan reports Complete")
	}
}

func TestPlanToleratesHandWritten(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	mustWrite(t, filepath.Join(env.Root, "etc/sudoers.d/pbp"),
		"# PBP helper, see README\n\n  Defaults:pbp env_reset  \n\tDefaults:pbp env_keep = \"SSH_ORIGINAL_COMMAND SSH_CONNECTION\"\n\npbp ALL=(root) NOPASSWD: /usr/bin/pbp node   \n", 0o440)
	mustWrite(t, filepath.Join(env.Root, "var/lib/pbp/.ssh/authorized_keys"),
		"# pbp forced command\n\n  "+AuthorizedKeysLine(testKey)+"  \n\n", 0o644)
	mustWrite(t, filepath.Join(env.Root, "etc/pbp/node.yaml"),
		"# node helper config\nbridge: vmbr99   # isolated\nvmid_range:\n  - 990000\n  - 990049\nrequest_timeout: 20m\n", 0o640)
	p := buildPlan(t, env, testArgs())
	if !p.Complete() {
		t.Fatalf("hand-written files not accepted:\n%s", dump(p))
	}
}

func TestPlanDiffers(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	pve.roles[RoleName] = strings.Join(RolePrivs, ",") + ",Datastore.Allocate"
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	// widened env_keep
	mustWrite(t, filepath.Join(env.Root, "etc/sudoers.d/pbp"),
		strings.Replace(SudoersText, "SSH_CONNECTION", "SSH_CONNECTION LD_PRELOAD", 1), 0o440)
	// a second key line
	mustWrite(t, filepath.Join(env.Root, "var/lib/pbp/.ssh/authorized_keys"),
		AuthorizedKeysLine(testKey)+"\nssh-ed25519 AAAAother admin\n", 0o644)
	p := buildPlan(t, env, testArgs())

	for _, c := range []struct {
		k        Kind
		path     string
		contains string
	}{
		{KindFile, "/etc/sudoers.d/pbp", "LD_PRELOAD"},
		{KindFile, "/var/lib/pbp/.ssh/authorized_keys", "AAAAother"},
		{KindRole, RoleName, "-Datastore.Allocate"},
	} {
		it := item(t, p, c.k, c.path)
		if it.Status != Differs || !strings.Contains(it.Diff, c.contains) {
			t.Errorf("%s %s: status %v diff %q, want Differs mentioning %q", c.k, c.path, it.Status, it.Diff, c.contains)
		}
	}
	if p.Complete() {
		t.Error("differing plan reports Complete")
	}

	// sudo reads #<digits> as a user id and #include/#includedir as
	// directives, not comments: each is Differs.
	for _, extra := range []string{"#0 ALL=(ALL) NOPASSWD: ALL", "#1001 ALL=(ALL) NOPASSWD: ALL",
		"#include /tmp/x", "#includedir /tmp/d"} {
		mustWrite(t, filepath.Join(env.Root, "etc/sudoers.d/pbp"), SudoersText+extra+"\n", 0o440)
		it := item(t, buildPlan(t, env, testArgs()), KindFile, "/etc/sudoers.d/pbp")
		if it.Status != Differs || !strings.Contains(it.Diff, "-"+extra) {
			t.Errorf("sudoers with %q: status %v diff %q, want Differs", extra, it.Status, it.Diff)
		}
	}
	for _, comment := range []string{"# comment", "#", "#comment", "#\tx"} {
		mustWrite(t, filepath.Join(env.Root, "etc/sudoers.d/pbp"), comment+"\n"+SudoersText, 0o440)
		if it := item(t, buildPlan(t, env, testArgs()), KindFile, "/etc/sudoers.d/pbp"); it.Status != Correct {
			t.Errorf("sudoers with comment %q: status %v diff %q, want Correct", comment, it.Status, it.Diff)
		}
	}

	// Correct content, wrong mode.
	mustWrite(t, filepath.Join(env.Root, "etc/sudoers.d/pbp"), SudoersText, 0o640)
	it := item(t, buildPlan(t, env, testArgs()), KindFile, "/etc/sudoers.d/pbp")
	if it.Status != Differs || !strings.Contains(it.Diff, "0640") || !strings.Contains(it.Diff, "0440") {
		t.Errorf("sudoers mode 0640: status %v diff %q", it.Status, it.Diff)
	}
}

func TestPlanDiffersMore(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	pve.roles[RoleName] = "VM.Audit,Sys.Audit" // missing privileges
	pve.acls = pve.acls[:len(pve.acls)-1]      // token entry missing on the last path
	env := testEnv(t, pve, "/bin/bash")        // a real shell that is not /bin/sh
	layoutComplete(t, env.Root)
	mustMkdir(t, filepath.Join(env.Root, "var/lib/pbp"), 0o755)
	mustWrite(t, filepath.Join(env.Root, "etc/pbp/node.yaml"), "vmid_range: [990000, 990099]\nbridge: vmbr98\n", 0o640)
	tmplDir := filepath.Join(env.Root, node.DefaultTemplateDir)
	for n := range report.Templates() {
		mustWrite(t, filepath.Join(tmplDir, n), "old", 0o640)
		break
	}
	p := buildPlan(t, env, testArgs())

	for _, c := range []struct {
		k        Kind
		path     string
		status   Status
		contains string
	}{
		{KindRole, RoleName, Differs, "+VM.Allocate"},
		{KindACL, "/sdn/zones/localnetwork", Create, TokenID},
		{KindShell, "pbp", Differs, "/bin/bash"},
		{KindHome, "/var/lib/pbp", Differs, "0755"},
		{KindFile, "/etc/pbp/node.yaml", Differs, "vmbr98"},
		{KindTemplates, node.DefaultTemplateDir, Differs, "differs"},
	} {
		it := item(t, p, c.k, c.path)
		if it.Status != c.status || !strings.Contains(it.Diff, c.contains) {
			t.Errorf("%s %s: status %v diff %q, want %v mentioning %q", c.k, c.path, it.Status, it.Diff, c.status, c.contains)
		}
	}
}

func TestPlanTokenWithoutPrivsep(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	pve.nosep = true
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	p := buildPlan(t, env, testArgs())
	if tok := item(t, p, KindToken, TokenID); tok.Status != Differs || !strings.Contains(tok.Diff, "privsep") {
		t.Errorf("privsep 0 token: status %v diff %q, want Differs", tok.Status, tok.Diff)
	}
	if p.Complete() {
		t.Error("privsep 0 token reports Complete")
	}
}

func TestPlanOwnerDiffers(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	base := env.Lookup
	env.Lookup = func(user string) (int, int, string, string, error) {
		uid, gid, home, shell, err := base(user)
		if user == "root" {
			uid, gid = uid+1, gid+1 // files on disk are now "owned by pbp", not root
		}
		return uid, gid, home, shell, err
	}
	p := buildPlan(t, env, testArgs())
	it := item(t, p, KindFile, "/etc/sudoers.d/pbp")
	if it.Status != Differs || !strings.Contains(it.Diff, "owner") {
		t.Errorf("sudoers owned by pbp: status %v diff %q", it.Status, it.Diff)
	}
	if it := item(t, p, KindHome, "/var/lib/pbp"); it.Status != Correct {
		t.Errorf("home owned by pbp: status %v diff %q", it.Status, it.Diff)
	}
}

func TestPlanNotRegularFile(t *testing.T) {
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	p := filepath.Join(env.Root, "etc/sudoers.d/pbp")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", p); err != nil {
		t.Fatal(err)
	}
	it := item(t, buildPlan(t, env, testArgs()), KindFile, "/etc/sudoers.d/pbp")
	if it.Status != Differs || !strings.Contains(it.Diff, "not a regular file") {
		t.Errorf("symlinked sudoers: status %v diff %q", it.Status, it.Diff)
	}
}

func TestPlanPveumError(t *testing.T) {
	env := testEnv(t, &fakePVE{}, "/bin/sh")
	env.Run = func(context.Context, string, []string, []byte) (string, string, int, error) {
		return "", "ipcc_send_rec failed", 255, nil
	}
	if _, err := BuildPlan(context.Background(), env, testArgs(), "pve1"); err == nil || !strings.Contains(err.Error(), "ipcc_send_rec") {
		t.Errorf("err = %v, want pveum failure", err)
	}
}

func TestDesiredValues(t *testing.T) {
	if got := string(NodeYAML(990000, 990049, "vmbr99")); got != "vmid_range: [990000, 990049]\nbridge: vmbr99\nrequest_timeout: 15m\n" {
		t.Errorf("NodeYAML = %q", got)
	}
	if got := strings.Join(ACLPaths("pve1", []string{"a", "b"}), " "); got != "/vms /storage/a /storage/b /nodes/pve1 /sdn/zones/localnetwork" {
		t.Errorf("ACLPaths = %q", got)
	}
	if len(RolePrivs) != 13 || RolePrivs[0] != "VM.Allocate" || RolePrivs[12] != "SDN.Use" {
		t.Errorf("RolePrivs = %v", RolePrivs)
	}
	if Correct.String() != "already correct" || Create.String() != "create" || Differs.String() != "differs" {
		t.Error("Status strings")
	}
}
