package nodesetup

import (
	"context"
	"path/filepath"
	"time"
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
	// can map both root and pbp to its own uid/gid.
	Lookup func(user string) (uid, gid int, home, shell string, err error)
	Now    func() time.Time
}

// path returns p (an absolute node path) under Root.
func (e Env) path(p string) string {
	return filepath.Join(e.Root, p)
}
