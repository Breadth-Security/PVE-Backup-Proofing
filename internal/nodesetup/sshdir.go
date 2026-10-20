package nodesetup

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// pbp owns its home, /var/lib/pbp, so a process running as pbp can replace
// /var/lib/pbp/.ssh (with a symlink to /etc/pve/priv or /root/.ssh, say)
// at any moment, including between the plan and the y/N prompt's answer.
// Nothing in .ssh is therefore ever reached by path: the home is opened
// with O_NOFOLLOW (its parent /var/lib is root's), .ssh is opened relative
// to it with O_NOFOLLOW and, for setup, made root's before anything is
// written; every later step (temp file, backup, rename, unlink) is relative
// to that directory fd. Once .ssh is root's, pbp can no longer change what
// is in it; at most it can rename it away from sshd's view, which the
// self-test's second plan then reports.

const (
	sshDirName = ".ssh"
	akName     = "authorized_keys"
	sshDirMode = 0o755
)

// maxManagedFile caps how much of a file setup and uninstall read or back
// up. pbp can write its own .ssh before setup secures it, so a huge or
// sparse authorized_keys planted there must not make root read it into
// memory or copy it in full. Every file setup writes is a few hundred bytes.
const maxManagedFile = 64 << 10

// errTooLarge marks a file over maxManagedFile.
var errTooLarge = errors.New("larger than 64 KiB, which setup never writes")

func tooLarge(path string) error {
	return fmt.Errorf("%s is %w; move it away by hand", path, errTooLarge)
}

// readCapped reads all of f, refusing more than maxManagedFile bytes.
func readCapped(f *os.File, path string) ([]byte, error) {
	if fi, err := f.Stat(); err == nil && fi.Size() > maxManagedFile {
		return nil, tooLarge(path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxManagedFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxManagedFile {
		return nil, tooLarge(path)
	}
	return b, nil
}

// sshDirAllowed reports whether name may sit in pbp's .ssh: only
// authorized_keys, setup's own backups of it, and temp files an interrupted
// setup left (secureSSHDir removes those). sshd reads other key files
// (authorized_keys2, or whatever AuthorizedKeysFile names) too.
func sshDirAllowed(name string) bool {
	return name == akName || strings.HasPrefix(name, akName+backupSuffix) || isTempName(name)
}

// isTempName reports whether name is one of setup's temp names.
func isTempName(name string) bool {
	prefix := strings.TrimSuffix(tmpPattern, "*")
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.ParseUint(rest, 10, 64)
	return err == nil
}

// openHome opens pbp's home (under Root) as a directory, never through a
// symlink.
func openHome(env Env) (int, error) {
	p := env.path(PbpHome)
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: PbpHome, Err: err}
	}
	return fd, nil
}

// openSSHDirAt opens .ssh in the home dir fd, never through a symlink.
// ENOENT is returned as is (wrapped in a PathError); anything that is not a
// directory gives a "not a directory" error naming SSHDir.
func openSSHDirAt(home int) (int, error) {
	fd, err := unix.Openat(home, sshDirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return -1, fmt.Errorf("%s is not a directory (a symlink or other file); setup and uninstall do not touch it, move it away by hand", SSHDir)
	case err != nil:
		return -1, &fs.PathError{Op: "open", Path: SSHDir, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "fstat", Path: SSHDir, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%s is not a directory; setup and uninstall do not touch it, move it away by hand", SSHDir)
	}
	return fd, nil
}

// secureSSHDir opens pbp's .ssh for setup's writes: it creates it if absent,
// opens it without following a symlink, makes it root:root 0755 through
// the fd, and then (when pbp can no longer add anything) checks it holds
// nothing but authorized_keys and its backups. The caller closes the fd.
func secureSSHDir(env Env, ids ids) (int, error) {
	home, err := openHome(env)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(home) }()
	// Created 0700 and opened to 0755 only once it is root's.
	if err := unix.Mkdirat(home, sshDirName, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, &fs.PathError{Op: "mkdir", Path: SSHDir, Err: err}
	}
	fd, err := openSSHDirAt(home)
	if err != nil {
		return -1, err
	}
	err = env.Fchown(fd, ids.uid["root"], ids.gid["root"])
	if err == nil {
		err = unix.Fchmod(fd, sshDirMode)
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "chown/chmod", Path: SSHDir, Err: err}
	}
	// .ssh is root's now, so nothing new can appear in it: remove the temp
	// files an interrupted setup left (unlinkat removes a symlink itself,
	// never its target).
	if err := removeTempFiles(fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if extra, err := sshDirExtra(fd); err != nil || len(extra) > 0 {
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		return -1, fmt.Errorf("%s holds %s; sshd may read keys from it: setup allows only %s there, move the rest away by hand",
			SSHDir, strings.Join(extra, ", "), akName)
	}
	return fd, nil
}

// dirNames lists the entries of the directory fd. dir stays open.
func dirNames(dir int) ([]string, error) {
	dup, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: SSHDir, Err: err}
	}
	f := os.NewFile(uintptr(dup), SSHDir)
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// removeTempFiles unlinks setup's leftover temp names in the directory fd.
func removeTempFiles(dir int) error {
	names, err := dirNames(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !isTempName(n) {
			continue
		}
		if err := unix.Unlinkat(dir, n, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &fs.PathError{Op: "remove", Path: fdPath(n), Err: err}
		}
	}
	return nil
}

// sshDirExtra lists the entries of the directory fd that sshDirAllowed
// rejects. dir stays open.
func sshDirExtra(dir int) ([]string, error) {
	names, err := dirNames(dir)
	if err != nil {
		return nil, err
	}
	var extra []string
	for _, n := range names {
		if !sshDirAllowed(n) {
			extra = append(extra, n)
		}
	}
	return extra, nil
}

// fdPath names a file in .ssh for errors.
func fdPath(name string) string { return filepath.Join(SSHDir, name) }

// openRegularAt opens name in dir read-only, never through a symlink, and
// refuses anything but a regular file.
func openRegularAt(dir int, name string) (*os.File, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%s is not a regular file (symlink); setup and uninstall do not touch it, move it away by hand", fdPath(name))
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: fdPath(name), Err: err}
	}
	f := os.NewFile(uintptr(fd), fdPath(name))
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, notRegular(fdPath(name), fi)
	}
	return f, nil
}

// createExclAt creates name in dir with O_EXCL and O_NOFOLLOW: anything
// already there, a symlink too, is an error.
func createExclAt(dir int, name string) (*os.File, error) {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: fdPath(name), Err: err}
	}
	return os.NewFile(uintptr(fd), fdPath(name)), nil
}

// backupAt copies the regular file name in dir to a new
// name.pbp-backup-<ts> in dir, with its mode and owner.
func backupAt(env Env, dir int, name string) error {
	in, err := openRegularAt(dir, name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > maxManagedFile {
		return tooLarge(fdPath(name))
	}
	bak := name + backupSuffix + env.Now().Format(backupTime)
	out, err := createExclAt(dir, bak)
	if err != nil {
		return err
	}
	err = func() error {
		if _, err := io.Copy(out, io.LimitReader(in, maxManagedFile)); err != nil {
			return err
		}
		if err := out.Sync(); err != nil {
			return err
		}
		if err := out.Chmod(fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)); err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstat(int(in.Fd()), &st); err != nil {
			return err
		}
		return env.Fchown(int(out.Fd()), int(st.Uid), int(st.Gid))
	}()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = unix.Unlinkat(dir, bak, 0)
		return err
	}
	return nil
}

// tempName returns a fresh dotted temp name matching tmpPattern.
func tempName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strings.Replace(tmpPattern, "*", strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10), 1)
}

// writeFileAt writes content to name in dir through a dotted temp file in
// dir with mode and the owner uid:gid, renamed into place.
func writeFileAt(env Env, dir int, name string, content []byte, mode os.FileMode, uid, gid int) error {
	var f *os.File
	var tmp string
	for range 100 {
		tmp = tempName()
		var err error
		if f, err = createExclAt(dir, tmp); err == nil {
			break
		} else if !errors.Is(err, unix.EEXIST) {
			return err
		}
		f = nil
	}
	if f == nil {
		return fmt.Errorf("cannot create a temp file in %s", SSHDir)
	}
	err := func() error {
		if _, err := f.Write(content); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Chmod(mode); err != nil {
			return err
		}
		return env.Fchown(int(f.Fd()), uid, gid)
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if rerr := unix.Renameat(dir, tmp, dir, name); rerr != nil {
			err = &fs.PathError{Op: "rename", Path: fdPath(name), Err: rerr}
		}
	}
	if err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
	}
	return err
}

// applyAuthorizedKeys writes authorized_keys into the secured .ssh dir fd.
// The plan was made by path before the y/N prompt, and pbp owns its home:
// it can hide .ssh while planning and put it back before this runs. So the
// file is read again through the fd, and whatever is there now decides:
// setup's own line needs no write, a file the plan did not expect (a
// planned Create that finds one) is refused, and a planned Differs (only
// reached with --replace) is backed up first.
func applyAuthorizedKeys(env Env, ids ids, dir int, it Item) error {
	f, err := openRegularAt(dir, akName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		have, err := readCapped(f, fdPath(akName))
		_ = f.Close()
		if err != nil {
			return err
		}
		if string(have) == string(it.Content) {
			return nil
		}
		if it.Status != Differs {
			return fmt.Errorf("%s appeared or changed after the plan was made; nothing was overwritten, run pbp node setup again", AuthorizedKeys)
		}
		if err := backupAt(env, dir, akName); err != nil {
			return err
		}
	}
	return writeFileAt(env, dir, akName, it.Content, it.Mode, ids.uid[it.Owner], ids.gid[it.Group])
}

// removeAuthorizedKeys removes authorized_keys from pbp's .ssh, through a
// dir fd opened without following a symlink, after a backup unless it is
// exactly what setup writes. A missing .ssh or file is fine.
func removeAuthorizedKeys(env Env, it Item, setups setupsFunc) error {
	home, err := openHome(env)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(home) }()
	dir, err := openSSHDirAt(home)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	f, err := openRegularAt(dir, akName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := readCapped(f, fdPath(akName))
	_ = f.Close()
	// An oversized file is not setup's (and pbp may have planted it to
	// fill the disk): remove it without a backup.
	big := errors.Is(err, errTooLarge)
	if err != nil && !big {
		return err
	}
	if !big && (it.Backup || !setups(env, b)) {
		if err := backupAt(env, dir, akName); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(dir, akName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "remove", Path: AuthorizedKeys, Err: err}
	}
	return nil
}

// removeSSHDir removes pbp's .ssh if it is an empty directory (never
// through a symlink), and leaves it if not.
func removeSSHDir(env Env) error {
	home, err := openHome(env)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(home) }()
	err = unix.Unlinkat(home, sshDirName, unix.AT_REMOVEDIR)
	switch {
	case err == nil, errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		return nil
	case errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("%s is not a directory; uninstall does not touch it, move it away by hand", SSHDir)
	}
	return &fs.PathError{Op: "remove", Path: SSHDir, Err: err}
}
