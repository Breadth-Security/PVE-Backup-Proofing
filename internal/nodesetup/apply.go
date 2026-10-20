package nodesetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
)

const (
	usermod = "/usr/sbin/usermod"
	visudo  = "/usr/sbin/visudo"
	// tmpPattern names temp files. sudo skips every file in /etc/sudoers.d
	// whose name contains a '.' (or ends in '~'), so a dotted temp name
	// there is never live, even before visudo has checked it. The same
	// pattern is used for every file for simplicity.
	tmpPattern = ".pbp.tmp-*"
	// backupSuffix + Env.Now in backupTime makes "<file>.pbp-backup-<ts>".
	// It contains a '.', so sudo never reads a sudoers backup either.
	backupSuffix = ".pbp-backup-"
	backupTime   = "20060102T150405"
)

// ApplyOpts are the operator's choices for Apply.
type ApplyOpts struct {
	// Replace allows changing items that differ, after a backup.
	Replace bool
}

// Refusal is why Apply would refuse p with o before changing anything: a
// check-only item is not correct, or an item differs and o.Replace is
// false. It is nil when Apply would go ahead.
func (p Plan) Refusal(o ApplyOpts) error {
	var checkOnly, differs []string
	for _, it := range p.Items {
		switch {
		case it.CheckOnly && it.Status != Correct:
			checkOnly = append(checkOnly, it.Name+" ("+it.Path+")")
		case it.Status == Differs && !o.Replace:
			differs = append(differs, it.Name+" ("+it.Path+")")
		}
	}
	if len(checkOnly) > 0 {
		return fmt.Errorf("setup does not change %s; fix it by hand and run setup again", strings.Join(checkOnly, ", "))
	}
	if len(differs) > 0 {
		return fmt.Errorf("these differ from what setup writes: %s; rerun with --replace to back them up and replace them", strings.Join(differs, ", "))
	}
	return nil
}

// Apply brings the node to the state p describes, item by item in plan
// order (authorized_keys last, so the host cannot log in before the rest is
// in place). Correct items are skipped. It refuses, changing nothing, when a
// check-only item is not correct or an item differs and o.Replace is false.
// It returns the API token's secret when it added the token, else "".
// tokenSecret may be non-empty when err != nil (a step after the token
// failed): the secret is shown only once, so the caller must still hand it
// over. The secret never appears in an argv, a log or an error.
// a.RotateToken removes an existing token before adding it again. ACL paths
// come from p; nodeName is unused.
func Apply(ctx context.Context, env Env, p Plan, a nodeproto.SetupArgs, nodeName string, o ApplyOpts) (tokenSecret string, err error) {
	if err := p.Refusal(o); err != nil {
		return "", err
	}
	ids, err := lookupIDs(env)
	if err != nil {
		return "", err
	}
	// readded: PVE drops a token's ACL entries when the token is removed,
	// and adding it again does not restore them, so after the token is added
	// every ACL item is applied, whatever its planned status.
	readded := false
	// sshDir is pbp's .ssh, opened and made root's once (see sshdir.go);
	// the .ssh items act only through it.
	sshDir := -1
	defer func() {
		if sshDir >= 0 {
			_ = unix.Close(sshDir)
		}
	}()
	ssh := func() (int, error) {
		if sshDir < 0 {
			fd, err := secureSSHDir(env, ids)
			if err != nil {
				return -1, err
			}
			sshDir = fd
		}
		return sshDir, nil
	}
	for _, it := range p.Items {
		if it.Status == Correct && (it.Kind != KindACL || !readded) {
			continue
		}
		var err error
		switch it.Kind {
		case KindDir:
			if it.Path == SSHDir {
				_, err = ssh()
			} else {
				err = applyDir(env, ids, it)
			}
		case KindFile:
			switch it.Path {
			case SudoersPath:
				err = applySudoers(ctx, env, ids, it)
			case AuthorizedKeys:
				var dir int
				if dir, err = ssh(); err == nil {
					err = applyAuthorizedKeys(env, ids, dir, it)
				}
			default:
				err = applyFile(env, ids, it)
			}
		case KindShell:
			err = runCmd(ctx, env, usermod, "-s", string(it.Content), it.Path)
		case KindTemplates:
			err = applyTemplates(env, it)
		case KindRole:
			verb := "add"
			if it.Status == Differs {
				verb = "modify"
			}
			err = runCmd(ctx, env, pveum, "role", verb, RoleName, "--privs", strings.Join(RolePrivs, ","))
		case KindUser:
			err = runCmd(ctx, env, pveum, "user", "add", UserID, "--comment", "PBP proofing")
		case KindToken:
			var s string
			s, err = applyToken(ctx, env, it, a.RotateToken)
			if s != "" {
				tokenSecret, readded = s, true
			}
		case KindACL:
			err = runCmd(ctx, env, pveum, "acl", "modify", it.Path, "--users", UserID, "--tokens", TokenID, "--roles", RoleName)
		default:
			err = fmt.Errorf("unknown item kind %q", it.Kind)
		}
		if err != nil {
			return tokenSecret, fmt.Errorf("%s: %w", it.Name, err)
		}
	}
	return tokenSecret, nil
}

// runCmd runs a command whose output is not needed; a failure carries its
// stderr.
func runCmd(ctx context.Context, env Env, name string, args ...string) error {
	_, stderr, exit, err := env.Run(ctx, name, args, nil)
	return cmdErr(name, args, stderr, exit, err)
}

func cmdErr(name string, args []string, stderr string, exit int, err error) error {
	cmd := name + " " + strings.Join(args, " ")
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if exit != 0 {
		return fmt.Errorf("%s: exit %d: %s", cmd, exit, strings.TrimSpace(stderr))
	}
	return nil
}

// applyToken adds the token, first removing an existing one when rotating
// or when it differs (privsep 0). It returns the new secret.
func applyToken(ctx context.Context, env Env, it Item, rotate bool) (string, error) {
	if it.Status == Differs || rotate {
		exists, err := tokenExists(ctx, env)
		if err != nil {
			return "", err
		}
		if exists {
			if err := runCmd(ctx, env, pveum, "user", "token", "remove", UserID, TokenName); err != nil {
				return "", err
			}
		}
	}
	args := []string{"user", "token", "add", UserID, TokenName, "--privsep", "1", "--comment", "PBP proof token", "--output-format", "json"}
	stdout, stderr, exit, err := env.Run(ctx, pveum, args, nil)
	if err := cmdErr(pveum, args, stderr, exit, err); err != nil {
		return "", err
	}
	// stdout holds the secret: it never goes into an error.
	var out struct {
		Value string `json:"value"`
	}
	if json.Unmarshal([]byte(stdout), &out) != nil || out.Value == "" {
		return "", fmt.Errorf("pveum user token add %s %s: no token value in its output; remove the token with `pveum user token remove %s %s` and run setup again",
			UserID, TokenName, UserID, TokenName)
	}
	return out.Value, nil
}

func tokenExists(ctx context.Context, env Env) (bool, error) {
	var toks []struct {
		TokenID string `json:"tokenid"`
	}
	exit, stderr, err := runPveum(ctx, env, &toks, "user", "token", "list", UserID)
	switch {
	case exit != 0 && strings.Contains(strings.ToLower(stderr), "no such user"):
		return false, nil
	case err != nil:
		return false, err
	}
	for _, t := range toks {
		if t.TokenID == TokenName {
			return true, nil
		}
	}
	return false, nil
}

// applyDir creates a directory, or fixes the mode and owner of an existing
// one in place (no backup for directories). Only for directories whose
// parent is root's (/etc/pbp); pbp's .ssh goes through secureSSHDir.
func applyDir(env Env, ids ids, it Item) error {
	p := env.path(it.Path)
	if it.Status == Create {
		if err := os.Mkdir(p, it.Mode.Perm()); err != nil {
			return err
		}
	} else {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory; setup does not remove it, move it away by hand", it.Path)
		}
	}
	if err := os.Chmod(p, it.Mode); err != nil {
		return err
	}
	return env.Chown(p, ids.uid[it.Owner], ids.gid[it.Group])
}

// applyFile writes a file through a temp file in the same directory, after
// a backup when it replaces one. Only for files in directories pbp cannot
// write to; authorized_keys goes through applyAuthorizedKeys.
func applyFile(env Env, ids ids, it Item) error {
	p := env.path(it.Path)
	if it.Status == Differs {
		if _, err := backup(env, p); err != nil {
			return err
		}
	}
	tmp, err := writeTemp(env, ids, it)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeTemp writes it.Content to a dotted temp file next to it.Path, with
// the item's mode and owner, and returns its path.
func writeTemp(env Env, ids ids, it Item) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(env.path(it.Path)), tmpPattern)
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	err = func() error {
		_, err := f.Write(it.Content)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp, it.Mode)
		}
		if err == nil {
			err = env.Chown(tmp, ids.uid[it.Owner], ids.gid[it.Group])
		}
		return err
	}()
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// applySudoers installs /etc/sudoers.d/pbp: the dotted temp file is checked
// with `visudo -cf`, renamed into place, then the whole configuration is
// checked with `visudo -c`. If that fails, the old file is restored (or the
// new one removed) so sudo keeps working.
func applySudoers(ctx context.Context, env Env, ids ids, it Item) error {
	p := env.path(it.Path)
	var bak string
	if it.Status == Differs {
		var err error
		if bak, err = backup(env, p); err != nil {
			return err
		}
	}
	tmp, err := writeTemp(env, ids, it)
	if err != nil {
		return err
	}
	if err := runCmd(ctx, env, visudo, "-cf", tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := runCmd(ctx, env, visudo, "-c"); err != nil {
		if rerr := restore(env, bak, p); rerr != nil {
			return fmt.Errorf("%w; restoring %s also failed: %v", err, it.Path, rerr)
		}
		return err
	}
	return nil
}

// restore puts the backup bak back at p, or removes p when there was no
// backup. The backup is copied (it stays) into a dotted temp file restore
// creates itself, which is then renamed over p.
func restore(env Env, bak, p string) error {
	if bak == "" {
		return os.Remove(p)
	}
	f, err := os.CreateTemp(filepath.Dir(p), tmpPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := copyInto(env, bak, f); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// backup copies the regular file at p to a new p.pbp-backup-<YYYYMMDDTHHMMSS>,
// keeping its mode and owner, and returns the backup's path. The backup is
// always created with O_EXCL, so an existing path there (even a symlink,
// e.g. pre-placed in pbp's home) is never written through; a same-second
// second backup is an error. Anything but a regular file at p is refused.
func backup(env Env, p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", notRegular(p, fi)
	}
	bak := p + backupSuffix + env.Now().Format(backupTime)
	f, err := createExcl(bak)
	if err != nil {
		return "", err
	}
	if err := copyInto(env, p, f); err != nil {
		_ = os.Remove(bak)
		return "", err
	}
	return bak, nil
}

func notRegular(p string, fi fs.FileInfo) error {
	return fmt.Errorf("%s is not a regular file (%s); setup does not replace it, move it away by hand", p, fi.Mode().Type())
}

// createExcl creates a new file, failing if anything (a symlink too) is
// already at p.
func createExcl(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path setup manages
}

// copyInto copies the regular file src (never followed if it is a symlink)
// into dst, a file the caller just created, gives dst src's mode and owner,
// and closes it.
func copyInto(env Env, src string, dst *os.File) (err error) {
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}()
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- a path setup manages
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return notRegular(src, fi)
	}
	if _, err := io.Copy(dst, in); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := dst.Chmod(fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return env.Chown(dst.Name(), int(st.Uid), int(st.Gid))
	}
	return nil
}

// applyTemplates installs the notification templates as node.InstallTemplates
// does (pmxcfs has no modes or owners). With Replace, each template that
// differs is first copied to its backup name; PVE looks templates up by
// name, so the backups are inert.
func applyTemplates(env Env, it Item) error {
	dir := filepath.Join(env.Root, node.DefaultTemplateDir)
	if it.Status == Differs {
		for n, want := range report.Templates() {
			p := filepath.Join(dir, n)
			have, err := os.ReadFile(p) // #nosec G304 -- the template dir
			if errors.Is(err, fs.ErrNotExist) || err == nil && string(have) == string(want) {
				continue
			}
			if err != nil {
				return err
			}
			f, err := createExcl(p + backupSuffix + env.Now().Format(backupTime))
			if err != nil {
				return err
			}
			_, err = f.Write(have)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
	return node.InstallTemplates(dir)
}
