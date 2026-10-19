package probe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultCommandTimeout bounds one command when Checks.CommandTimeout is 0.
const DefaultCommandTimeout = time.Minute

// timedOutDetail is the CheckResult detail of a check whose command ran out
// of time.
const timedOutDetail = "timed out"

var (
	// errCommandTimeout: one command ran past the per-command timeout while
	// the probe itself still had time. That check fails.
	errCommandTimeout = errors.New("command timed out")
	// errProbeEnded: the probe's own context ended (its deadline, or a
	// shutdown). PBP could not finish judging the guest.
	errProbeEnded = errors.New("probe ended")
	// errSettleTimeout: the guest's boot did not settle within the wait
	// budget while the probe itself still had time.
	errSettleTimeout = errors.New("boot did not settle")
)

// DefaultSettleTimeout bounds the wait for boot to settle when the probe
// context has no deadline and Input.SettleTimeout is 0.
const DefaultSettleTimeout = 5 * time.Minute

// maxSettleReserve caps the time kept back from the boot-settle wait for
// the checks that follow it.
const maxSettleReserve = 30 * time.Second

// settleBudget is how long to wait for boot to settle: the probe's
// remaining time minus a reserve for the checks after it (a quarter of the
// remaining time, at most maxSettleReserve), or fallback without a deadline.
func settleBudget(ctx context.Context, fallback time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		if fallback <= 0 {
			return DefaultSettleTimeout
		}
		return fallback
	}
	rem := time.Until(dl)
	return rem - min(rem/4, maxSettleReserve)
}

// guard wraps the probe's executor. It bounds every command with the
// per-command timeout and classifies what goes wrong:
//
//   - a command that runs out of its own time returns errCommandTimeout;
//   - a command cut short because the probe's context ended returns
//     errProbeEnded, and the probe is marked ended;
//   - any other error (except ErrExecBlocked) is a transport error, the
//     first of which is remembered.
//
// Steps that tolerate an Exec error, such as dependency tracing, stay
// conservative; Run then turns a resulting FAILED into ERROR, since an
// unfinished probe or a broken transport says nothing about the backup.
type guard struct {
	ex      Executor
	timeout time.Duration

	mu        sync.Mutex
	transport error
	ended     error // the probe context's error, once a command saw it end
}

func newGuard(ex Executor, timeout time.Duration) *guard {
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	return &guard{ex: ex, timeout: timeout}
}

func (g *guard) Exec(ctx context.Context, argv []string) (ExecResult, error) {
	res, err := g.run(ctx, argv)
	if err != nil {
		g.note(ctx, err)
	}
	return res, err
}

// settle runs argv bounded by budget instead of the per-command timeout.
// It is for waiting on the guest's boot to settle, which legitimately takes
// longer than an ordinary check. Running out of budget while the probe
// still has time returns errSettleTimeout and is not recorded.
func (g *guard) settle(ctx context.Context, budget time.Duration, argv []string) (ExecResult, error) {
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res, err := g.ex.Exec(sctx, argv)
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		err = fmt.Errorf("%w: %w", errProbeEnded, ctx.Err())
	case sctx.Err() != nil:
		return res, fmt.Errorf("%w after %s: %w", errSettleTimeout, budget, err)
	}
	g.note(ctx, err)
	return res, err
}

// note records an Exec error: the probe ending, or the first transport
// error. Command timeouts and a blocked agent are the step's own business.
func (g *guard) note(ctx context.Context, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case errors.Is(err, errProbeEnded):
		if g.ended == nil {
			g.ended = ctx.Err()
		}
	case errors.Is(err, errCommandTimeout), errors.Is(err, ErrExecBlocked):
	default:
		if g.transport == nil {
			g.transport = err
		}
	}
}

// run executes argv under the per-command timeout without recording
// anything, and classifies a context-caused failure.
func (g *guard) run(ctx context.Context, argv []string) (ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	res, err := g.ex.Exec(cctx, argv)
	if err == nil {
		return res, nil
	}
	switch {
	case ctx.Err() != nil:
		return res, fmt.Errorf("%w: %w", errProbeEnded, ctx.Err())
	case cctx.Err() != nil:
		return res, fmt.Errorf("%w after %s: %w", errCommandTimeout, g.timeout, err)
	}
	return res, err
}

func (g *guard) transportErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.transport
}

// endedErr is the probe context's error if a command saw the probe end.
func (g *guard) endedErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ended
}

// transportReason is the ERROR reason for a transport error.
func transportReason(err error) string { return "probe transport error: " + err.Error() }

// endedReason is the ERROR reason when the probe's context ended.
func endedReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe timed out"
	}
	return "probe cancelled"
}
