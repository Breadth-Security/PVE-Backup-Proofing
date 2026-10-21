package nodesetup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"

	"golang.org/x/sys/unix"
)

const pveum = "/usr/sbin/pveum"

// Status is how one item compares with its desired state.
type Status int

// Item statuses.
const (
	Correct Status = iota // already correct
	Create                // absent; setup creates it
	Differs               // present but different; changed only with --replace
	Remove                // present; uninstall removes it (or, for the shell, revokes it)
	Refused               // uninstall cannot remove it safely (Diff says why); left for the admin
)

func (s Status) String() string {
	switch s {
	case Correct:
		return "already correct"
	case Create:
		return "create"
	case Differs:
		return "differs"
	case Remove:
		return "remove"
	case Refused:
		return "refused"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Kind says what an item is, and so how Apply acts on it.
type Kind string

// Item kinds.
const (
	KindDir       Kind = "dir"       // Path: directory; Mode, Owner, Group
	KindFile      Kind = "file"      // Path: file; Mode, Owner, Group, Content
	KindShell     Kind = "shell"     // Path: user name; Content: the shell
	KindHome      Kind = "home"      // Path: pbp's home; check-only
	KindCheck     Kind = "check"     // a check-only condition; Path names what it checks
	KindTemplates Kind = "templates" // Path: template dir; written as node.InstallTemplates does
	KindRole      Kind = "role"      // Path: role id; privileges RolePrivs
	KindUser      Kind = "user"      // Path: user id
	KindToken     Kind = "token"     // Path: token id
	KindACL       Kind = "acl"       // Path: ACL path; user and token get RoleName
)

// Item is one resource of the node's desired state.
type Item struct {
	Kind Kind
	// Name is a short human label, e.g. "sudoers rule".
	Name string
	// Path identifies the resource: an absolute node path (not joined with
	// Env.Root) for dir, file, home and templates; the user name for shell;
	// the role, user or token id; the ACL path.
	Path   string
	Status Status
	// Diff explains Create/Differs: "-have"/"+want" lines or a sentence.
	// Empty when Correct.
	Diff string
	// Desired mode and owner names (dir, file).
	Mode         os.FileMode
	Owner, Group string
	// Content is the desired file content (file) or login shell (shell).
	Content []byte
	// CheckOnly items are reported, never changed: Apply refuses to proceed
	// while one is not Correct.
	CheckOnly bool
	// Backup marks an uninstall item whose file is not what setup writes:
	// Uninstall copies it to <file>.pbp-backup-<ts> before removing it.
	Backup bool
}

// Plan lists every item in apply order.
type Plan struct {
	Items []Item
	// Warnings name what setup leaves alone but the admin should know:
	// permissions for pbp@pve or its tokens beyond what PBP needs.
	Warnings []string
}

// Complete reports whether every item is already correct.
func (p Plan) Complete() bool {
	for _, it := range p.Items {
		if it.Status != Correct {
			return false
		}
	}
	return true
}

// BuildPlan compares the desired state for a on node with what is on disk
// and in PVE. Items come in apply order: /etc/pbp and node.yaml, shell (and
// the check-only home), sudoers, templates, role/user/token/ACLs, then the
// .ssh dir and authorized_keys last.
func BuildPlan(ctx context.Context, env Env, a nodeproto.SetupArgs, nodeName string) (Plan, error) {
	ids, err := lookupIDs(env)
	if err != nil {
		return Plan{}, err
	}
	pub, err := nodeproto.ParsePubKey(a.PubKey)
	if err != nil {
		return Plan{}, fmt.Errorf("public key: %w", err)
	}
	var p Plan
	add := func(it Item) { p.Items = append(p.Items, it) }

	add(planDir(env, ids, "/etc/pbp", EtcDir, 0o750, "root", "pbp"))
	add(planFile(env, ids, fileItem("helper config", NodeYAMLPath, 0o640, "root", "pbp",
		NodeYAML(a.VMIDRange[0], a.VMIDRange[1], a.Bridge)), sameNodeYAML(env, a)))
	add(planShell(ids))
	add(planHome(env, ids))
	add(planKeySources(ctx, env, ids))
	p.Warnings = append(p.Warnings, matchWarnings(env)...)
	add(planSSHDirContents(env))
	add(planFile(env, ids, fileItem("sudoers rule", SudoersPath, 0o440, "root", "root",
		[]byte(SudoersText)), sameSudoers))
	add(planTemplates(env))

	token := a.CreateToken || a.RotateToken
	if token || len(a.Storages) > 0 {
		pve, warn, err := planPVE(ctx, env, a, nodeName, token)
		if err != nil {
			return Plan{}, err
		}
		p.Items = append(p.Items, pve...)
		p.Warnings = append(p.Warnings, warn...)
	}

	add(planDir(env, ids, "pbp .ssh dir", SSHDir, 0o755, "root", "root"))
	add(planFile(env, ids, fileItem("authorized key", AuthorizedKeys, 0o644, "root", "root",
		[]byte(AuthorizedKeysLine(pub)+"\n")), sameAuthorizedKeys(pub)))
	return p, nil
}

// ids holds the uid/gid of root and pbp, plus pbp's home and shell.
type ids struct {
	uid, gid map[string]int
	home     string
	pbpShell string
}

func lookupIDs(env Env) (ids, error) {
	out := ids{uid: map[string]int{}, gid: map[string]int{}}
	for _, u := range []string{"root", PbpUser} {
		uid, gid, home, shell, err := env.Lookup(u)
		if err != nil {
			return ids{}, fmt.Errorf("look up user %s: %w", u, err)
		}
		out.uid[u], out.gid[u] = uid, gid
		if u == PbpUser {
			out.home, out.pbpShell = home, shell
		}
	}
	return out, nil
}

func fileItem(name, path string, mode os.FileMode, owner, group string, content []byte) Item {
	return Item{Kind: KindFile, Name: name, Path: path, Mode: mode, Owner: owner, Group: group, Content: content}
}

// statOwned lstats path under Root and returns mode/owner problems.
func statOwned(env Env, ids ids, it Item) (fs.FileInfo, []string, error) {
	fi, err := os.Lstat(env.path(it.Path))
	if err != nil {
		return nil, nil, err
	}
	var bad []string
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != it.Mode {
		bad = append(bad, fmt.Sprintf("mode %04o, want %04o", uint32(m), uint32(it.Mode)))
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		wu, wg := ids.uid[it.Owner], ids.gid[it.Group]
		if int(st.Uid) != wu || int(st.Gid) != wg {
			bad = append(bad, fmt.Sprintf("owner %d:%d, want %s:%s (%d:%d)", st.Uid, st.Gid, it.Owner, it.Group, wu, wg))
		}
	}
	return fi, bad, nil
}

func planDir(env Env, ids ids, name, path string, mode os.FileMode, owner, group string) Item {
	it := Item{Kind: KindDir, Name: name, Path: path, Mode: mode, Owner: owner, Group: group}
	fi, bad, err := statOwned(env, ids, it)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		it.Status, it.Diff = Create, fmt.Sprintf("+dir %04o %s:%s", uint32(mode), owner, group)
	case err != nil:
		it.Status, it.Diff = Differs, err.Error()
	case !fi.IsDir():
		it.Status, it.Diff = Differs, "not a directory ("+fi.Mode().Type().String()+")"
	case len(bad) > 0:
		it.Status, it.Diff = Differs, strings.Join(bad, "\n")
	}
	return it
}

// sameFunc compares file content with the desired content; when they do not
// match it returns the diff.
type sameFunc func(have []byte) (ok bool, diff string)

// readManaged reads a file setup manages for the plan: never through a
// symlink at the last component, never blocking on a FIFO swapped in after
// the Lstat (O_NONBLOCK, then fstat must say regular), and never more than
// maxManagedFile bytes (pbp can write its own .ssh before setup secures it).
func readManaged(env Env, path string) ([]byte, error) {
	fd, err := unix.Open(env.path(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, fi.Mode().Type())
	}
	return readCapped(f, path)
}

func planFile(env Env, ids ids, it Item, same sameFunc) Item {
	fi, bad, err := statOwned(env, ids, it)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		it.Status, it.Diff = Create, prefixLines("+", lines(it.Content))
		return it
	case err != nil:
		it.Status, it.Diff = Differs, err.Error()
		return it
	case !fi.Mode().IsRegular():
		it.Status, it.Diff = Differs, "not a regular file ("+fi.Mode().Type().String()+")"
		return it
	}
	have, err := readManaged(env, it.Path)
	if err != nil {
		it.Status, it.Diff = Differs, err.Error()
		return it
	}
	if ok, d := same(have); !ok {
		bad = append(bad, d)
	}
	if len(bad) > 0 {
		it.Status, it.Diff = Differs, strings.Join(bad, "\n")
	}
	return it
}

// sameNodeYAML compares vmid_range and bridge by value; request_timeout is a
// tuning knob and is kept as is.
func sameNodeYAML(env Env, a nodeproto.SetupArgs) sameFunc {
	return func([]byte) (bool, string) {
		c, err := node.LoadConfig(env.path(NodeYAMLPath))
		if err != nil {
			return false, "invalid: " + err.Error()
		}
		var d []string
		if c.VMIDRange != a.VMIDRange {
			d = append(d, fmt.Sprintf("-vmid_range: [%d, %d]\n+vmid_range: [%d, %d]",
				c.VMIDRange[0], c.VMIDRange[1], a.VMIDRange[0], a.VMIDRange[1]))
		}
		if c.Bridge != a.Bridge {
			d = append(d, "-bridge: "+c.Bridge+"\n+bridge: "+a.Bridge)
		}
		return len(d) == 0, strings.Join(d, "\n")
	}
}

// directives returns the trimmed lines of b, without blank lines and the
// lines isComment drops.
func directives(b []byte, isComment func(string) bool) []string {
	var out []string
	for _, l := range lines(b) {
		if l = strings.TrimSpace(l); l != "" && !isComment(l) {
			out = append(out, l)
		}
	}
	return out
}

// sshComment: sshd ignores every line starting with #.
func sshComment(l string) bool { return strings.HasPrefix(l, "#") }

// reSudoersComment matches only lines sudo surely treats as comments. sudo
// reads "#<digits>" as a user id and "#include"/"#includedir" as directives,
// so a # followed by a digit or "i" is kept and makes the file differ
// (over-strict is safe).
var reSudoersComment = regexp.MustCompile(`^#($|[^0-9i])`)

func sudoersComment(l string) bool { return reSudoersComment.MatchString(l) }

func sameSudoers(have []byte) (bool, string) {
	h, w := directives(have, sudoersComment), directives([]byte(SudoersText), sudoersComment)
	if slices.Equal(h, w) {
		return true, ""
	}
	return false, lineDiff(h, w)
}

func sameAuthorizedKeys(pub string) sameFunc {
	want := []string{AuthorizedKeysLine(pub)}
	return func(have []byte) (bool, string) {
		h := directives(have, sshComment)
		if slices.Equal(h, want) {
			return true, ""
		}
		return false, lineDiff(h, want)
	}
}

// lineDiff lists lines only in have as "-" and lines only in want as "+",
// or the whole of both when only the order differs.
func lineDiff(have, want []string) string {
	var d []string
	for _, l := range have {
		if !slices.Contains(want, l) {
			d = append(d, "-"+l)
		}
	}
	for _, l := range want {
		if !slices.Contains(have, l) {
			d = append(d, "+"+l)
		}
	}
	if len(d) == 0 {
		return prefixLines("-", have) + "\n" + prefixLines("+", want)
	}
	return strings.Join(d, "\n")
}

func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func prefixLines(p string, ls []string) string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = p + l
	}
	return strings.Join(out, "\n")
}

// planShell: a no-login shell (the .deb's default) is Create; any other
// shell than /bin/sh is Differs.
func planShell(ids ids) Item {
	it := Item{Kind: KindShell, Name: "pbp login shell", Path: PbpUser, Content: []byte(PbpShell)}
	switch ids.pbpShell {
	case PbpShell:
	case "", "/usr/sbin/nologin", "/sbin/nologin", "/bin/false", "/usr/bin/false":
		it.Status, it.Diff = Create, "-"+ids.pbpShell+"\n+"+PbpShell
	default:
		it.Status, it.Diff = Differs, "-"+ids.pbpShell+"\n+"+PbpShell
	}
	return it
}

// planHome checks pbp's home: /var/lib/pbp, a directory, 0755, owned by
// root, so pbp cannot rename or replace .ssh in it. The package makes it so
// (postinst); setup never changes it, and the item is check-only.
func planHome(env Env, ids ids) Item {
	it := Item{Kind: KindHome, Name: "pbp home", Path: PbpHome, Mode: PbpHomeMode, Owner: "root", Group: "root", CheckOnly: true}
	var bad []string
	if ids.home != PbpHome {
		bad = append(bad, "pbp's home is "+ids.home+", want "+PbpHome)
	}
	fi, err := os.Lstat(env.path(PbpHome))
	switch {
	case err != nil:
		bad = append(bad, err.Error())
	case !fi.IsDir():
		bad = append(bad, PbpHome+" is not a directory")
	default:
		if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != PbpHomeMode {
			bad = append(bad, fmt.Sprintf("mode %04o, want %04o", uint32(m), uint32(PbpHomeMode)))
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !ok:
			bad = append(bad, "cannot read the owner of "+PbpHome)
		case int(st.Uid) != ids.uid["root"] || int(st.Gid) != ids.gid["root"]:
			bad = append(bad, fmt.Sprintf("owner %d:%d, want root:root (%d:%d)", st.Uid, st.Gid, ids.uid["root"], ids.gid["root"]))
		}
	}
	if len(bad) > 0 {
		it.Status, it.Diff = Differs, strings.Join(bad, "\n")+
			"\nthe pbp package makes "+PbpHome+" root:root 0755: install it again (apt install --reinstall pbp), or fix it by hand"
	}
	return it
}

// planKeySources checks where sshd looks for pbp's keys (sshd -T for a pbp
// connection, so Match blocks apply): every AuthorizedKeysFile entry but
// /var/lib/pbp/.ssh/authorized_keys must be absent, that one must be
// listed, and no AuthorizedKeysCommand may apply. Otherwise a key setup
// never wrote would let a host in. Check-only: setup never edits sshd's
// configuration.
func planKeySources(ctx context.Context, env Env, ids ids) Item {
	it := Item{Kind: KindCheck, Name: "sshd key sources for pbp", Path: "sshd -T", CheckOnly: true}
	cfg, err := sshdConfig(ctx, env)
	if err != nil {
		it.Status, it.Diff = Differs, "cannot read the sshd configuration: "+err.Error()
		return it
	}
	var bad []string
	listed := false
	for _, f := range strings.Fields(cfg["authorizedkeysfile"]) {
		if f == "none" {
			continue
		}
		p, ok := expandKeysFile(f, ids.uid[PbpUser])
		if !ok {
			bad = append(bad, "AuthorizedKeysFile "+f+": cannot tell which file this names for pbp; use a plain path, %h or %u")
			continue
		}
		if p == AuthorizedKeys {
			listed = true
			continue
		}
		_, err := os.Lstat(env.path(p))
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		case err != nil:
			bad = append(bad, p+": "+err.Error())
		default:
			bad = append(bad, p+" exists, and sshd reads pbp's keys from it too: remove it (setup writes only "+AuthorizedKeys+")")
		}
	}
	if !listed {
		bad = append(bad, "sshd does not read "+AuthorizedKeys+" for pbp (AuthorizedKeysFile "+cfg["authorizedkeysfile"]+"): set AuthorizedKeysFile .ssh/authorized_keys in /etc/ssh/sshd_config")
	}
	if c := cfg["authorizedkeyscommand"]; c != "" && c != "none" {
		bad = append(bad, "AuthorizedKeysCommand "+c+" applies to pbp: sshd would accept keys it prints; set it to none for pbp in /etc/ssh/sshd_config")
	}
	// A user CA bypasses authorized_keys altogether: a certificate from it
	// naming the principal pbp (or one the principals file or command
	// allows) logs in without the forced command, with pbp's /bin/sh.
	if c := cfg["trustedusercakeys"]; c != "" && c != "none" {
		bad = append(bad, "TrustedUserCAKeys "+c+" applies to pbp: a certificate from that CA logs in as pbp without the forced command; add \"Match User pbp\" with \"TrustedUserCAKeys none\" to /etc/ssh/sshd_config")
	}
	if c := cfg["authorizedprincipalscommand"]; c != "" && c != "none" {
		bad = append(bad, "AuthorizedPrincipalsCommand "+c+" applies to pbp: set it to none for pbp in /etc/ssh/sshd_config")
	}
	if len(bad) > 0 {
		it.Status, it.Diff = Differs, strings.Join(bad, "\n")
	}
	return it
}

// planSSHDirContents checks that pbp's .ssh holds nothing but
// authorized_keys and setup's backups of it. Check-only: setup never
// removes a file it did not write. A missing .ssh, or one that is not a
// directory (the dir item reports that), is correct here.
func planSSHDirContents(env Env) Item {
	it := Item{Kind: KindCheck, Name: "pbp .ssh contents", Path: SSHDir, CheckOnly: true}
	fi, err := os.Lstat(env.path(SSHDir))
	if err != nil || !fi.IsDir() {
		return it
	}
	ents, err := os.ReadDir(env.path(SSHDir))
	if err != nil {
		it.Status, it.Diff = Differs, err.Error()
		return it
	}
	var extra []string
	for _, e := range ents {
		if !sshDirAllowed(e.Name()) {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		it.Status, it.Diff = Differs, "unexpected "+strings.Join(extra, ", ")+": only "+akName+" (and its backups) may be here, since sshd may read keys from other files; move them away"
	}
	return it
}

// planTemplates compares the installed templates by sha256 with the
// embedded ones, as CheckTemplates does; no mode or owner checks.
func planTemplates(env Env) Item {
	it := Item{Kind: KindTemplates, Name: "notification templates", Path: TemplateDir}
	tmpl := report.Templates()
	names := make([]string, 0, len(tmpl))
	for n := range tmpl {
		names = append(names, n)
	}
	sort.Strings(names)
	var missing, differ []string
	for _, n := range names {
		// #nosec G304 -- the fixed template dir and an embedded template name
		have, err := os.ReadFile(filepath.Join(env.path(TemplateDir), n))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, "+"+n)
		case err != nil:
			differ = append(differ, n+": "+err.Error())
		default:
			hs, ws := sha256.Sum256(have), sha256.Sum256(tmpl[n])
			if hs != ws {
				differ = append(differ, fmt.Sprintf("%s differs (sha256 %s, want %s)", n,
					hex.EncodeToString(hs[:])[:12], hex.EncodeToString(ws[:])[:12]))
			}
		}
	}
	switch {
	case len(differ) > 0:
		it.Status = Differs
	case len(missing) > 0:
		it.Status = Create
	}
	it.Diff = strings.Join(append(differ, missing...), "\n")
	return it
}

// runPveum runs `pveum args… --output-format json` and decodes stdout into v.
// A non-zero exit returns its stderr as the error.
func runPveum(ctx context.Context, env Env, v any, args ...string) (exit int, stderr string, err error) {
	stdout, stderr, exit, err := env.Run(ctx, pveum, append(args, "--output-format", "json"), nil)
	if err != nil {
		return exit, stderr, fmt.Errorf("pveum %s: %w", strings.Join(args, " "), err)
	}
	if exit != 0 {
		return exit, stderr, fmt.Errorf("pveum %s: exit %d: %s", strings.Join(args, " "), exit, strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		return exit, stderr, fmt.Errorf("pveum %s: %w", strings.Join(args, " "), err)
	}
	return 0, stderr, nil
}

// privList accepts privs as a comma-separated string or as an object keyed
// by privilege.
type privList []string

func (p *privList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				*p = append(*p, x)
			}
		}
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k := range m {
		*p = append(*p, k)
	}
	return nil
}

func planPVE(ctx context.Context, env Env, a nodeproto.SetupArgs, nodeName string, token bool) ([]Item, []string, error) {
	var items []Item
	var warn []string

	var roles []struct {
		RoleID string   `json:"roleid"`
		Privs  privList `json:"privs"`
	}
	if _, _, err := runPveum(ctx, env, &roles, "role", "list"); err != nil {
		return nil, nil, err
	}
	role := Item{Kind: KindRole, Name: "PVE role", Path: RoleName, Status: Create, Diff: prefixLines("+", RolePrivs)}
	for _, r := range roles {
		if r.RoleID != RoleName {
			continue
		}
		role.Status, role.Diff = Correct, ""
		have := slices.Clone([]string(r.Privs))
		want := slices.Clone(RolePrivs)
		sort.Strings(have)
		sort.Strings(want)
		have = slices.Compact(have)
		if !slices.Equal(have, want) {
			role.Status, role.Diff = Differs, lineDiff(have, want)
		}
	}
	items = append(items, role)

	var users []struct {
		UserID string          `json:"userid"`
		Groups json.RawMessage `json:"groups"`
	}
	if _, _, err := runPveum(ctx, env, &users, "user", "list"); err != nil {
		return nil, nil, err
	}
	user := Item{Kind: KindUser, Name: "PVE user", Path: UserID, Status: Create, Diff: "+" + UserID}
	for _, u := range users {
		if u.UserID == UserID {
			user.Status, user.Diff = Correct, ""
			if g := groupList(u.Groups); g != "" {
				warn = append(warn, UserID+" is in group(s) "+g+": the groups' permissions apply to it too, beyond what PBP needs")
			}
		}
	}
	items = append(items, user)

	// The token item appears when asked for, or when it is absent: the
	// ACL items name the token, and `acl modify --tokens` fails on a token
	// that does not exist.
	{
		tok := Item{Kind: KindToken, Name: "API token", Path: TokenID, Status: Create, Diff: "+" + TokenID + " (privsep 1)"}
		if user.Status == Correct {
			var toks []struct {
				TokenID string          `json:"tokenid"`
				Privsep json.RawMessage `json:"privsep"`
			}
			exit, stderr, err := runPveum(ctx, env, &toks, "user", "token", "list", UserID)
			switch {
			case exit != 0 && strings.Contains(strings.ToLower(stderr), "no such user"):
			case err != nil:
				return nil, nil, err
			}
			for _, t := range toks {
				if t.TokenID != TokenName {
					warn = append(warn, UserID+" has another API token, "+UserID+"!"+t.TokenID+"; setup manages only "+TokenID)
					continue
				}
				// privsep 0 gives the token every permission of the user.
				switch sep := strings.Trim(string(t.Privsep), `"`); sep {
				case "1", "true":
					tok.Status, tok.Diff = Correct, ""
				default:
					tok.Status, tok.Diff = Differs, "-privsep "+sep+"\n+privsep 1"
				}
			}
		}
		// Rotation removes the token and adds it again with --privsep 1,
		// which also fixes a privsep 0 token.
		if tok.Status != Create && a.RotateToken {
			tok.Status, tok.Diff = Create, "rotate: the existing "+TokenID+" is removed and created again with a new secret;\n"+
				"PVE drops a removed token's ACLs, so the new token gets only the paths below (storages: --storages)"
		}
		if token || tok.Status == Create {
			items = append(items, tok)
		} else if tok.Status == Differs {
			// Not in the plan without a token flag, but privsep 0 means
			// the token has every right the user has, now and whatever is
			// granted to it later (code review 3.9).
			warn = append(warn, TokenID+" has privsep 0, so it gets every permission of "+UserID+
				"; run the setup line with --rotate-token to recreate it with privsep 1")
		}
	}

	var acls []struct {
		Path      string          `json:"path"`
		RoleID    string          `json:"roleid"`
		Type      string          `json:"type"`
		UGID      string          `json:"ugid"`
		Propagate json.RawMessage `json:"propagate"`
	}
	if _, _, err := runPveum(ctx, env, &acls, "acl", "list"); err != nil {
		return nil, nil, err
	}
	paths := ACLPaths(nodeName, a.Storages)
	// Any grant to pbp@pve or one of its tokens beyond PBPProofer on these
	// paths widens what a stolen token can do. Setup leaves it (an admin may
	// grant more on purpose, see docs/setup.md) but says so.
	for _, e := range acls {
		mine := e.UGID == UserID || strings.HasPrefix(e.UGID, UserID+"!")
		if mine && (e.RoleID != RoleName || !slices.Contains(paths, e.Path)) {
			warn = append(warn, fmt.Sprintf("%s %s also has role %s on %s, beyond what PBP needs", e.Type, e.UGID, e.RoleID, e.Path))
		}
	}
	for _, path := range paths {
		var hasUser, hasToken bool
		for _, e := range acls {
			// An entry that does not propagate covers only the path itself,
			// not the guests or storages under it: count it as missing, and
			// acl modify (which propagates by default) sets it again.
			if e.Path != path || e.RoleID != RoleName || strings.Trim(string(e.Propagate), `"`) == "0" {
				continue
			}
			hasUser = hasUser || e.Type == "user" && e.UGID == UserID
			hasToken = hasToken || e.Type == "token" && e.UGID == TokenID
		}
		it := Item{Kind: KindACL, Name: "ACL " + path, Path: path}
		var d []string
		if !hasUser {
			d = append(d, "+user "+UserID+" "+RoleName)
		}
		if !hasToken {
			d = append(d, "+token "+TokenID+" "+RoleName)
		}
		if len(d) > 0 {
			it.Status, it.Diff = Create, strings.Join(d, "\n")
		}
		items = append(items, it)
	}
	return items, warn, nil
}

// groupList renders pveum's "groups" field, a comma-separated string (or,
// tolerated, a list), as "a, b"; "" when there are none.
func groupList(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.Join(strings.FieldsFunc(str, func(r rune) bool { return r == ',' || r == ' ' }), ", ")
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, ", ")
	}
	return ""
}

// reMatchByConnection finds an sshd Match line whose criteria depend on the
// connection rather than the user.
var reMatchByConnection = regexp.MustCompile(`(?i)^\s*match\s.*\b(address|host|localaddress|localport|rdomain)\b`)

// matchWarnings names the Match blocks in sshd's configuration that depend
// on the connection. The key-source check evaluates a connection from
// localhost, so a block that matches the PBP host's address could add a
// key source or CA it never sees (code review 3.2). Setup warns rather
// than refuses: such blocks are common and usually harmless.
func matchWarnings(env Env) []string {
	files := []string{"/etc/ssh/sshd_config"}
	if more, err := filepath.Glob(env.path("/etc/ssh/sshd_config.d/*.conf")); err == nil {
		for _, m := range more {
			rel, _ := filepath.Rel(env.Root, m)
			files = append(files, "/"+filepath.ToSlash(rel))
		}
	}
	var warn []string
	for _, f := range files {
		// #nosec G304 -- fixed sshd configuration paths under env.Root
		b, err := os.ReadFile(env.path(f))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if reMatchByConnection.MatchString(line) {
				warn = append(warn, fmt.Sprintf("%s:%d %q: the sshd key-source check only sees a connection from localhost; "+
					"make sure this block adds no AuthorizedKeysFile, AuthorizedKeysCommand or TrustedUserCAKeys for pbp",
					f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	return warn
}
