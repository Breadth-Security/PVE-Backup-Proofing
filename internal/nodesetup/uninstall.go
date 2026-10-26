package nodesetup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
)

// NoLogin is the shell uninstall gives pbp (the .deb's default).
const NoLogin = "/usr/sbin/nologin"

// setupsFunc reports whether content is exactly what setup writes; anything
// else is backed up before uninstall removes it.
type setupsFunc func(env Env, content []byte) bool

// uninstallFiles are the files uninstall removes, in order: the sudo rule
// and the key first, so access goes before anything else.
var uninstallFiles = []struct {
	name, path string
	setups     setupsFunc
}{
	{"sudoers rule", SudoersPath, func(_ Env, b []byte) bool { ok, _ := sameSudoers(b); return ok }},
	{"authorized key", AuthorizedKeys, func(_ Env, b []byte) bool { return setupsAuthorizedKeys(b) }},
	{"helper config", NodeYAMLPath, func(env Env, _ []byte) bool { _, err := node.LoadConfig(env.path(NodeYAMLPath)); return err == nil }},
}

// setupsAuthorizedKeys: every directive line is AuthorizedKeysLine of a
// valid ssh-ed25519 key, as setup writes it (comments and blanks aside).
func setupsAuthorizedKeys(b []byte) bool {
	for _, l := range directives(b, sshComment) {
		key, ok := strings.CutPrefix(l, forcedCmdPrefix)
		if !ok {
			return false
		}
		norm, err := nodeproto.ParsePubKey(key)
		if err != nil || AuthorizedKeysLine(norm) != l {
			return false
		}
	}
	return true
}

// BuildUninstallPlan lists what Uninstall would do, in its order: the sudo
// rule, authorized_keys and the .ssh dir (if it would be left empty),
// node.yaml, the pbp shell back to nologin, and with removeToken the API
// token, its ACL entries, the user and the role. Every item has Status
// Remove; Backup marks a file that is not what setup writes. /etc/pbp, the
// templates and the package are never listed. A clean node gives an empty
// plan. A setup path that is not a regular file (or, for .ssh, a directory),
// or cannot be read, gets a Refused item (Diff says why) instead of failing
// the plan, so the sudo rule and the key are still removed. A missing pbp
// user (Lookup's ErrNoSuchUser) gives no shell item; any other Lookup
// failure gives a Refused one. Only a failing pveum query fails the plan.
func BuildUninstallPlan(ctx context.Context, env Env, removeToken bool) (Plan, error) {
	var p Plan
	refuse := func(kind Kind, name, path string, err error) {
		p.Items = append(p.Items, Item{Kind: kind, Name: name, Path: path, Status: Refused, Diff: err.Error()})
	}
	sshDir, sshErr := lstatSSHDir(env)
	for _, f := range uninstallFiles {
		if f.path == AuthorizedKeys {
			if sshErr != nil {
				refuse(KindDir, "pbp .ssh dir", SSHDir, sshErr)
			}
			if !sshDir {
				continue
			}
		}
		it, err := planRemoveFile(env, f.name, f.path, f.setups)
		if err != nil {
			refuse(KindFile, f.name, f.path, err)
			continue
		}
		if it != nil {
			p.Items = append(p.Items, *it)
		}
		if f.path == AuthorizedKeys {
			if empty, err := emptyAfter(env, it); err == nil && empty {
				p.Items = append(p.Items, Item{Kind: KindDir, Name: "pbp .ssh dir", Path: SSHDir, Status: Remove, Diff: "-dir (empty)"})
			}
		}
	}

	// No pbp user: nothing can log in as it, so there is no shell to change.
	// Any other lookup failure leaves the shell unknown: refuse the item so
	// it is reported, never silently skipped.
	_, _, _, shell, err := env.Lookup(PbpUser)
	switch {
	case errors.Is(err, ErrNoSuchUser):
	case err != nil:
		refuse(KindShell, "pbp login shell", PbpUser, fmt.Errorf("cannot read the pbp login shell: %w", err))
	case shell != NoLogin && shell != "/sbin/nologin":
		p.Items = append(p.Items, Item{Kind: KindShell, Name: "pbp login shell", Path: PbpUser, Status: Remove,
			Content: []byte(NoLogin), Diff: "-" + shell + "\n+" + NoLogin})
	}

	if removeToken {
		items, err := planRemovePVE(ctx, env)
		if err != nil {
			return Plan{}, err
		}
		p.Items = append(p.Items, items...)
	}
	return p, nil
}

// lstatSSHDir reports whether /var/lib/pbp/.ssh exists; anything there but
// a directory (a symlink too) is refused.
func lstatSSHDir(env Env) (bool, error) {
	fi, err := os.Lstat(env.path(SSHDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, fmt.Errorf("%s is not a directory (%s); uninstall does not touch it, move it away by hand", SSHDir, fi.Mode().Type())
	}
	return true, nil
}

func notRegularUninstall(path string, fi fs.FileInfo) error {
	return fmt.Errorf("%s is not a regular file (%s); uninstall does not remove it, move it away by hand", path, fi.Mode().Type())
}

// planRemoveFile returns the item for a setup file, or nil when it is absent.
func planRemoveFile(env Env, name, path string, setups setupsFunc) (*Item, error) {
	b, err := readRegular(env, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if errors.Is(err, errTooLarge) && path == AuthorizedKeys {
		return &Item{Kind: KindFile, Name: name, Path: path, Status: Remove,
			Diff: "-file (larger than 64 KiB, so not setup's: removed without a backup)"}, nil
	}
	if err != nil {
		return nil, err
	}
	it := &Item{Kind: KindFile, Name: name, Path: path, Status: Remove, Diff: "-file"}
	if !setups(env, b) {
		it.Backup = true
		it.Diff = "-file (not what setup writes: backed up to " + path + backupSuffix + "<time> first)"
	}
	return it, nil
}

// readRegular reads the regular file at path (under Root) without following
// a symlink; anything else is refused.
func readRegular(env Env, path string) ([]byte, error) {
	p := env.path(path)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegularUninstall(path, fi)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- a path setup manages
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegularUninstall(path, fi)
	}
	return readCapped(f, path)
}

// emptyAfter reports whether .ssh is left empty once ak (nil when absent)
// is removed; a backup of authorized_keys would stay in it.
func emptyAfter(env Env, ak *Item) (bool, error) {
	ents, err := os.ReadDir(env.path(SSHDir))
	if err != nil {
		return false, err
	}
	for _, e := range ents {
		if ak == nil || ak.Backup || e.Name() != "authorized_keys" {
			return false, nil
		}
	}
	return true, nil
}

// pveACLs lists PBPProofer entries of the user and the token, by path in
// the order PVE lists them.
func pveACLs(ctx context.Context, env Env) (paths []string, user, token map[string]bool, err error) {
	var acls []struct {
		Path   string `json:"path"`
		RoleID string `json:"roleid"`
		Type   string `json:"type"`
		UGID   string `json:"ugid"`
	}
	if _, _, err := runPveum(ctx, env, &acls, "acl", "list"); err != nil {
		return nil, nil, nil, err
	}
	user, token = map[string]bool{}, map[string]bool{}
	for _, e := range acls {
		if e.RoleID != RoleName {
			continue
		}
		switch {
		case e.Type == "user" && e.UGID == UserID:
			user[e.Path] = true
		case e.Type == "token" && e.UGID == TokenID:
			token[e.Path] = true
		default:
			continue
		}
		if !slices.Contains(paths, e.Path) {
			paths = append(paths, e.Path)
		}
	}
	return paths, user, token, nil
}

func planRemovePVE(ctx context.Context, env Env) ([]Item, error) {
	var items []Item
	tok, err := tokenExists(ctx, env)
	if err != nil {
		return nil, err
	}
	if tok {
		items = append(items, Item{Kind: KindToken, Name: "API token", Path: TokenID, Status: Remove, Diff: "-" + TokenID})
	}

	paths, user, token, err := pveACLs(ctx, env)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		var d []string
		if user[p] {
			d = append(d, "-user "+UserID+" "+RoleName)
		}
		if token[p] {
			d = append(d, "-token "+TokenID+" "+RoleName)
		}
		items = append(items, Item{Kind: KindACL, Name: "ACL " + p, Path: p, Status: Remove, Diff: strings.Join(d, "\n")})
	}

	var users []struct {
		UserID string `json:"userid"`
	}
	if _, _, err := runPveum(ctx, env, &users, "user", "list"); err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.UserID != UserID {
			continue
		}
		// Removing the user removes all its tokens: name the ones setup
		// did not make (code review 3.7).
		d := []string{"-" + UserID}
		var toks []struct {
			TokenID string `json:"tokenid"`
		}
		if _, _, err := runPveum(ctx, env, &toks, "user", "token", "list", UserID); err != nil {
			return nil, err
		}
		for _, t := range toks {
			if t.TokenID != TokenName {
				d = append(d, "-token "+UserID+"!"+t.TokenID+" (not setup's; removed with the user)")
			}
		}
		items = append(items, Item{Kind: KindUser, Name: "PVE user", Path: UserID, Status: Remove, Diff: strings.Join(d, "\n")})
	}

	var roles []struct {
		RoleID string `json:"roleid"`
	}
	if _, _, err := runPveum(ctx, env, &roles, "role", "list"); err != nil {
		return nil, err
	}
	for _, r := range roles {
		if r.RoleID != RoleName {
			continue
		}
		// PVE drops every ACL entry that uses a removed role, including
		// grants to other users: name them (code review 3.7).
		d := []string{"-" + RoleName}
		var acls []struct {
			Path   string `json:"path"`
			RoleID string `json:"roleid"`
			UGID   string `json:"ugid"`
		}
		if _, _, err := runPveum(ctx, env, &acls, "acl", "list"); err != nil {
			return nil, err
		}
		for _, e := range acls {
			if e.RoleID == RoleName && e.UGID != UserID && !strings.HasPrefix(e.UGID, UserID+"!") {
				d = append(d, "-acl "+e.Path+" "+e.UGID+" "+RoleName+" (not setup's; dropped with the role)")
			}
		}
		items = append(items, Item{Kind: KindRole, Name: "PVE role", Path: RoleName, Status: Remove, Diff: strings.Join(d, "\n")})
	}
	return items, nil
}

// isAccess: the sudo rule and the key, removed best-effort together.
func isAccess(it Item) bool {
	return it.Kind == KindFile && (it.Path == SudoersPath || it.Path == AuthorizedKeys)
}

// Uninstall carries out p (from BuildUninstallPlan) in order. Each file is
// checked again as it is removed: a non-regular file is refused, and one
// that is not what setup writes (now, or when planned) is backed up first.
//
// The sudo rule and the key are access, and go best-effort: a failure on
// one still tries the other, and only then does uninstall stop with the
// errors. After the sudo rule is removed `visudo -c` runs; its failure is
// reported in the returned error, but uninstall goes on (there is nothing
// to restore). Refused items are skipped and reported in the error at the
// end. Any other failure stops it. PVE items run only with removeToken;
// each acts only on what is present.
func Uninstall(ctx context.Context, env Env, p Plan, removeToken bool) error {
	var visudoErr, accessErr, refused error
	for _, it := range p.Items {
		if it.Status == Refused {
			refused = errors.Join(refused, fmt.Errorf("%s (%s) not removed: %s", it.Name, it.Path, it.Diff))
			continue
		}
		if it.Status != Remove {
			continue
		}
		if accessErr != nil && !isAccess(it) {
			return errors.Join(accessErr, visudoErr, refused)
		}
		var err error
		switch it.Kind {
		case KindFile:
			err = removeFile(env, it)
			if err == nil && it.Path == SudoersPath {
				if verr := runCmd(ctx, env, visudo, "-c"); verr != nil {
					visudoErr = fmt.Errorf("after removing %s, the sudo configuration does not check; fix it now: %w", SudoersPath, verr)
				}
			}
			if err != nil && isAccess(it) {
				accessErr = errors.Join(accessErr, fmt.Errorf("%s: %w", it.Name, err))
				continue
			}
		case KindDir:
			if it.Path != SSHDir {
				err = fmt.Errorf("%s is not a directory uninstall removes", it.Path)
			} else {
				err = removeSSHDir(env)
			}
		case KindShell:
			err = runCmd(ctx, env, usermod, "-s", NoLogin, PbpUser)
		case KindToken, KindACL, KindUser, KindRole:
			if removeToken {
				err = removePVE(ctx, env, it)
			}
		default:
			err = fmt.Errorf("unknown item kind %q", it.Kind)
		}
		if err != nil {
			return errors.Join(accessErr, fmt.Errorf("%s: %w", it.Name, err), visudoErr, refused)
		}
	}
	return errors.Join(accessErr, visudoErr, refused)
}

// removeFile removes a setup file, after a backup unless it is exactly what
// setup writes. Already absent is fine.
func removeFile(env Env, it Item) error {
	var setups setupsFunc
	for _, f := range uninstallFiles {
		if f.path == it.Path {
			setups = f.setups
		}
	}
	if setups == nil {
		return fmt.Errorf("%s is not a file uninstall removes", it.Path)
	}
	// Were pbp's home pbp's, it could swap .ssh for a symlink after planning:
	// authorized_keys is reached only through a dir fd (see sshdir.go).
	if it.Path == AuthorizedKeys {
		return removeAuthorizedKeys(env, it, setups)
	}
	b, err := readRegular(env, it.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	p := env.path(it.Path)
	if it.Backup || !setups(env, b) {
		if _, err := backup(env, p); err != nil {
			return err
		}
	}
	return os.Remove(p)
}

// removePVE removes one PVE item if it is still present. The ACL delete
// names only the identities that still hold an entry there: PVE refuses
// `acl delete --tokens` for a token that does not exist, and removing the
// token (the step before) already drops its entries.
func removePVE(ctx context.Context, env Env, it Item) error {
	switch it.Kind {
	case KindToken:
		ok, err := tokenExists(ctx, env)
		if err != nil || !ok {
			return err
		}
		return runCmd(ctx, env, pveum, "user", "token", "remove", UserID, TokenName)
	case KindACL:
		_, user, token, err := pveACLs(ctx, env)
		if err != nil {
			return err
		}
		if token[it.Path] {
			if token[it.Path], err = tokenExists(ctx, env); err != nil {
				return err
			}
		}
		if !user[it.Path] && !token[it.Path] {
			return nil
		}
		args := []string{"acl", "delete", it.Path}
		if user[it.Path] {
			args = append(args, "--users", UserID)
		}
		if token[it.Path] {
			args = append(args, "--tokens", TokenID)
		}
		return runCmd(ctx, env, pveum, append(args, "--roles", RoleName)...)
	case KindUser:
		var users []struct {
			UserID string `json:"userid"`
		}
		if _, _, err := runPveum(ctx, env, &users, "user", "list"); err != nil {
			return err
		}
		for _, u := range users {
			if u.UserID == UserID {
				return runCmd(ctx, env, pveum, "user", "delete", UserID)
			}
		}
		return nil
	case KindRole:
		var roles []struct {
			RoleID string `json:"roleid"`
		}
		if _, _, err := runPveum(ctx, env, &roles, "role", "list"); err != nil {
			return err
		}
		for _, r := range roles {
			if r.RoleID == RoleName {
				return runCmd(ctx, env, pveum, "role", "delete", RoleName)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown item kind %q", it.Kind)
}
