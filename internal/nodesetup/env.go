package nodesetup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
)

// Env is every outside effect, so tests run unprivileged on a temp root.
type Env struct {
	// Root is "/" in production; every filesystem path is joined under it.
	Root string
	// Run runs a command by absolute path, with stdin, and returns its output
	// and exit code; err is for failures to start it.
	Run   func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr string, exit int, err error)
	Chown func(path string, uid, gid int) error
	// Lookup resolves a user name (also "root") to its uid, primary gid, home
	// and login shell. Owner checks compare stat uid/gid with it, so a test
	// can map both root and pbp to its own uid/gid. An unknown user is an
	// error wrapping ErrNoSuchUser; any other error means the lookup failed.
	Lookup func(user string) (uid, gid int, home, shell string, err error)
	Now    func() time.Time
}

// ErrNoSuchUser is wrapped by Env.Lookup when the user does not exist.
var ErrNoSuchUser = errors.New("no such user")

// NewEnv is the production Env: the real root, commands run by absolute
// path with the helper's minimal environment and their stdout and stderr
// captured (stdout is never put into an error: `pveum user token add`
// prints the secret there), os.Lchown, /etc/passwd lookups and time.Now.
func NewEnv() Env {
	return Env{
		Root:   "/",
		Run:    node.Capture,
		Chown:  os.Lchown,
		Lookup: passwdLookup("/etc/passwd"),
		Now:    time.Now,
	}
}

// passwdLookup reads user entries from a passwd(5) file. os/user has no
// login shell, so the file is read directly.
func passwdLookup(path string) func(user string) (int, int, string, string, error) {
	return func(user string) (int, int, string, string, error) {
		b, err := os.ReadFile(path) // #nosec G304 -- the system passwd file
		if err != nil {
			return 0, 0, "", "", fmt.Errorf("look up user %s: %w", user, err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			f := strings.Split(sc.Text(), ":")
			if len(f) != 7 || f[0] != user || user == "" {
				continue
			}
			uid, uerr := strconv.Atoi(f[2])
			gid, gerr := strconv.Atoi(f[3])
			if uerr != nil || gerr != nil {
				continue
			}
			return uid, gid, f[5], f[6], nil
		}
		if err := sc.Err(); err != nil {
			return 0, 0, "", "", fmt.Errorf("look up user %s: read %s: %w", user, path, err)
		}
		return 0, 0, "", "", fmt.Errorf("user %s: %w", user, ErrNoSuchUser)
	}
}

// path returns p (an absolute node path) under Root.
func (e Env) path(p string) string {
	return filepath.Join(e.Root, p)
}
