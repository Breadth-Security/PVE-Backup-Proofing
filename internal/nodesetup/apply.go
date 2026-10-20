package nodesetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	// RotateToken removes an existing API token and adds it again with a
	// new secret.
	RotateToken bool
}

// Apply brings the node to the state p describes, item by item in plan
// order (authorized_keys last, so the host cannot log in before the rest is
// in place). Correct items are skipped. It refuses, changing nothing, when a
// check-only item is not correct or an item differs and o.Replace is false.
// It returns the API token's secret when it created the token, else "".
// The secret never appears in an argv, a log or an error. ACL paths come
// from p, so nodeName and a only add RotateToken (a.RotateToken or
// o.RotateToken rotates).
func Apply(ctx context.Context, env Env, p Plan, a nodeproto.SetupArgs, nodeName string, o ApplyOpts) (tokenSecret string, err error) {
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
		return "", fmt.Errorf("setup does not change %s; fix it by hand and run setup again", strings.Join(checkOnly, ", "))
	}
	if len(differs) > 0 {
		return "", fmt.Errorf("these differ from what setup writes: %s; rerun with --replace to back them up and replace them", strings.Join(differs, ", "))
	}
	ids, err := lookupIDs(env)
	if err != nil {
		return "", err
	}
	rotate := o.RotateToken || a.RotateToken
	for _, it := range p.Items {
		if it.Status == Correct {
			continue
		}
		var err error
		switch it.Kind {
		case KindDir:
			err = applyDir(env, ids, it)
		case KindFile:
			if it.Path == SudoersPath {
				err = applySudoers(ctx, env, ids, it)
			} else {
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
			tokenSecret, err = applyToken(ctx, env, it, rotate)
		case KindACL:
			err = runCmd(ctx, env, pveum, "acl", "modify", it.Path, "--users", UserID, "--tokens", TokenID, "--roles", RoleName)
		default:
			err = fmt.Errorf("unknown item kind %q", it.Kind)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", it.Name, err)
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
// one in place (no backup for directories).
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
// a backup when it replaces one.
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
// backup. The backup is copied (it stays) through a dotted temp file.
func restore(env Env, bak, p string) error {
	if bak == "" {
		return os.Remove(p)
	}
	f, err := os.CreateTemp(filepath.Dir(p), tmpPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Close()
	if err := copyFile(env, bak, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// backup copies the file at p to p.pbp-backup-<YYYYMMDDTHHMMSS>, keeping its
// mode and owner, and returns the backup's path. A symlink is backed up as
// a symlink; anything else that is not a regular file is refused.
func backup(env Env, p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	bak := p + backupSuffix + env.Now().Format(backupTime)
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		return bak, os.Symlink(target, bak)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file (%s); setup does not replace it, move it away by hand", p, fi.Mode().Type())
	}
	return bak, copyFile(env, p, bak)
}

// copyFile copies src to dst (created, or truncated when it exists empty
// from CreateTemp) with src's mode and owner.
func copyFile(env Env, src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src) // #nosec G304 -- a path setup manages
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if _, err := os.Lstat(dst); err == nil {
		flags = os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(dst, flags, 0o600) // #nosec G304 -- see above
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(dst, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && err == nil {
		err = env.Chown(dst, int(st.Uid), int(st.Gid))
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
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
			bak := p + backupSuffix + env.Now().Format(backupTime)
			if err := os.WriteFile(bak, have, 0o644); err != nil { // #nosec G306 -- templates are not secret
				return err
			}
		}
	}
	return node.InstallTemplates(dir)
}
