package probe

import (
	"context"
	"errors"
	"sync"
)

// guard wraps the probe's executor and remembers the first transport error
// (an Exec that could not run at all, other than ErrExecBlocked). Steps that
// tolerate an Exec error, such as dependency tracing, stay conservative; the
// remembered error then turns a resulting FAILED into ERROR, since a broken
// transport says nothing about the backup.
type guard struct {
	ex Executor

	mu        sync.Mutex
	transport error
}

func (g *guard) Exec(ctx context.Context, argv []string) (ExecResult, error) {
	res, err := g.ex.Exec(ctx, argv)
	if err != nil && !errors.Is(err, ErrExecBlocked) {
		g.mu.Lock()
		if g.transport == nil {
			g.transport = err
		}
		g.mu.Unlock()
	}
	return res, err
}

func (g *guard) transportErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.transport
}

// transportReason is the ERROR reason for a transport error.
func transportReason(err error) string { return "probe transport error: " + err.Error() }
