// Package hostsetup is the host side of `pbp setup`: it asks the questions,
// writes the config and key, and prints the lines to run on the node.
package hostsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

// Layout says where setup writes (spec §4.1). Docker (Deb false) writes under
// Dir; the .deb layout (Deb true) writes under /etc/pbp and ignores Dir.
type Layout struct {
	Dir string
	Deb bool
}

// dockerID is the distroless "nonroot" uid and gid the container runs as.
const dockerID = 65532

// Seams so tests run unprivileged: ownership changes, the deb group lookup
// and the deb root (always /etc/pbp in production).
var (
	chown       = os.Lchown
	lookupGroup = func(name string) (int, error) {
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, fmt.Errorf("look up group %s (is the pbp package installed?): %w", name, err)
		}
		return strconv.Atoi(g.Gid)
	}
	debRoot = "/etc/pbp"
)

const debConfigDir = "/etc/pbp"

func (l Layout) root() string {
	if l.Deb {
		return debRoot
	}
	return l.Dir
}

// ConfigPath is config.yaml on disk.
func (l Layout) ConfigPath() string { return filepath.Join(l.root(), "config.yaml") }

// SecretsDir holds the SSH key pair and the token secret.
func (l Layout) SecretsDir() string { return filepath.Join(l.root(), "secrets") }

// KeyPath is the SSH private key on disk.
func (l Layout) KeyPath() string { return filepath.Join(l.SecretsDir(), "pve_ssh_key") }

// PubKeyPath is the SSH public key on disk.
func (l Layout) PubKeyPath() string { return l.KeyPath() + ".pub" }

// TokenPath is the API token secret on disk.
func (l Layout) TokenPath() string { return filepath.Join(l.SecretsDir(), "pve_token") }

// KnownHostsPath is the node's host key file on disk.
func (l Layout) KnownHostsPath() string { return filepath.Join(l.root(), "known_hosts") }

// LogsDir is the bind-mounted log directory; "" for the deb layout (the
// package owns /var/log/pbp).
func (l Layout) LogsDir() string {
	if l.Deb {
		return ""
	}
	return filepath.Join(l.Dir, "logs")
}

// EnvPath is the compose .env holding TZ; "" for the deb layout.
func (l Layout) EnvPath() string {
	if l.Deb {
		return ""
	}
	return filepath.Join(l.Dir, ".env")
}

// InConfigKeyPath is target.ssh.key_file: where the daemon sees the key.
func (l Layout) InConfigKeyPath() string {
	if l.Deb {
		return debConfigDir + "/secrets/pve_ssh_key"
	}
	return "/run/secrets/pve_ssh_key"
}

// InConfigTokenPath is target.api.token_secret_file.
func (l Layout) InConfigTokenPath() string {
	if l.Deb {
		return debConfigDir + "/secrets/pve_token"
	}
	return "/run/secrets/pve_token"
}

// InConfigKnownHostsPath is target.ssh.known_hosts_file (the same for both).
func (l Layout) InConfigKnownHostsPath() string { return debConfigDir + "/known_hosts" }

// CheckPaths refuses a config.yaml whose key, token or known_hosts path is
// not where this layout keeps that file: setup writes and checks only the
// layout's files, so a config naming others (a 0.2 .deb host may use other
// names) would be set up against files the daemon never reads.
func (l Layout) CheckPaths(cfg config.Config) error {
	type field struct{ name, have, want, disk string }
	var bad []string
	for _, f := range []field{
		{"target.ssh.key_file", cfg.Target.SSH.KeyFile, l.InConfigKeyPath(), l.KeyPath()},
		{"target.api.token_secret_file", cfg.Target.API.TokenSecretFile, l.InConfigTokenPath(), l.TokenPath()},
		{"target.ssh.known_hosts_file", cfg.Target.SSH.KnownHostsFile, l.InConfigKnownHostsPath(), l.KnownHostsPath()},
	} {
		if f.have == f.want {
			continue
		}
		where := ""
		if f.disk != f.want {
			where = " (on this host " + f.disk + ")"
		}
		bad = append(bad, fmt.Sprintf("%s is %q, expected %s%s", f.name, f.have, f.want, where))
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%s names files where pbp setup does not keep them: %s. "+
		"Move those files to the expected paths and update config.yaml to match (sudoedit %s), then run pbp setup again",
		l.ConfigPath(), strings.Join(bad, "; "), l.ConfigPath())
}

// HaveToken reports whether a non-empty token secret file exists.
func (l Layout) HaveToken() bool {
	fi, err := os.Stat(l.TokenPath())
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// owner is the uid and gid every file of the layout gets.
func (l Layout) owner() (uid, gid int, err error) {
	if !l.Deb {
		return dockerID, dockerID, nil
	}
	gid, err = lookupGroup("pbp")
	return 0, gid, err
}

// WriteSecretFile writes a file under SecretsDir (creating it first) with the
// layout's secret mode and owner: 0400 65532 (Docker) or 0640 root:pbp (deb).
func (l Layout) WriteSecretFile(path string, data []byte) error {
	if err := l.ensureDir(l.SecretsDir(), l.pick(0o700, 0o750)); err != nil {
		return err
	}
	return l.writeFile(path, data, l.pick(0o400, 0o640))
}

// WritePlainFile writes a non-secret file (config.yaml, known_hosts, .env)
// with the layout's mode and owner: 0444 65532 (Docker) or 0640 root:pbp (deb).
func (l Layout) WritePlainFile(path string, data []byte) error {
	return l.writeFile(path, data, l.pick(0o444, 0o640))
}

func (l Layout) pick(docker, deb os.FileMode) os.FileMode {
	if l.Deb {
		return deb
	}
	return docker
}

// ensureDir creates dir if missing and sets its mode and owner.
func (l Layout) ensureDir(dir string, mode os.FileMode) error {
	uid, gid, err := l.owner()
	if err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, mode); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return err
	}
	if err := chown(dir, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	return nil
}

// writeFile replaces path atomically: temp file in the same directory, fsync,
// chmod, chown, rename. The temp file is removed on any error.
func (l Layout) writeFile(path string, data []byte, mode os.FileMode) (err error) {
	uid, gid, err := l.owner()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = chown(tmp, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

// TestHooks replaces the ownership change, the pbp group lookup and the deb
// root, so callers' tests run unprivileged. It returns the restore function.
// Production code never calls it.
func TestHooks(chownFn func(string, int, int) error, group func(string) (int, error), root string) (restore func()) {
	oldChown, oldGroup, oldRoot := chown, lookupGroup, debRoot
	chown, lookupGroup, debRoot = chownFn, group, root
	return func() { chown, lookupGroup, debRoot = oldChown, oldGroup, oldRoot }
}
