// Package daemon runs PBP's scheduled runs and owns the single-run lock.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

// DefaultStateDir holds the state store, the run lock and the daemon start
// time. PBP_STATE_DIR overrides it.
const DefaultStateDir = "/var/lib/pbp/state"

// shutdownGrace is how long an in-flight run may keep going after shutdown
// is requested before its context is cancelled.
var shutdownGrace = 10 * time.Minute

// StateDir returns PBP_STATE_DIR or DefaultStateDir.
func StateDir() string {
	if d := os.Getenv("PBP_STATE_DIR"); d != "" {
		return d
	}
	return DefaultStateDir
}

// LockPath is the run lock in the state directory.
func LockPath() string { return filepath.Join(StateDir(), "run.lock") }

// clock is what Serve needs of time; a seam for tests.
type clock struct {
	now func() time.Time
	// wake caps each wait, so a wall-clock step (NTP, a manual fix) or a
	// suspend is noticed within it (code review 6.8).
	wake time.Duration
	// retryEvery and retryFor: a tick that finds the lock held by another
	// pbp command (not this daemon's own run) retries this often, for this
	// long, instead of losing the run (code review 6.7).
	retryEvery, retryFor time.Duration
}

var realClock = clock{now: time.Now, wake: time.Minute, retryEvery: time.Minute, retryFor: 15 * time.Minute}

// Serve calls runFn with trigger "schedule" at each time cfg.Schedule names
// (container time zone), holding the run lock for the whole call. A tick
// that finds this daemon's previous run still active is skipped; one that
// finds the lock held by another pbp command (pbp check, pbp run) retries
// for a while. Ticks follow the wall clock: a run starts once the clock
// reaches its time, never early and never twice for the same time, even if
// the clock steps. When ctx ends, Serve stops scheduling and waits for an
// in-flight run: its stop channel closes at once, so it starts no further
// proofs, and its own context is cancelled only after shutdownGrace, so
// proofs in flight can finish or tear down.
func Serve(ctx context.Context, cfg config.Config, runFn func(ctx context.Context, stop <-chan struct{}, trigger string)) error {
	return serve(ctx, cfg, runFn, realClock)
}

func serve(ctx context.Context, cfg config.Config, runFn func(ctx context.Context, stop <-chan struct{}, trigger string), c clock) error {
	sched, err := cron.ParseStandard(cfg.Schedule)
	if err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}
	log := slog.Default()

	runCtx, cancelRuns := context.WithCancel(context.Background())
	defer cancelRuns()
	var wg sync.WaitGroup
	var own atomic.Bool // this daemon's run is active

	next := sched.Next(c.now())
	log.Info("daemon started", "schedule", cfg.Schedule, "next", next)
	for {
		if next.IsZero() {
			return errors.New("schedule never fires")
		}
		// Wait on the wall clock, in bounded steps.
		for now := c.now(); now.Before(next); now = c.now() {
			if !sleep(ctx, min(next.Sub(now), c.wake)) {
				return shutdown(log, &wg, cancelRuns)
			}
		}
		if ctx.Err() != nil {
			return shutdown(log, &wg, cancelRuns)
		}
		fired := next

		release, err := AcquireLock(LockPath())
		if errors.Is(err, ErrBusy) && !own.Load() {
			release, err = retryLock(ctx, log, c, fired)
		}
		switch {
		case errors.Is(err, ErrBusy):
			log.Warn("previous run still active, skipping", "scheduled", fired)
		case ctx.Err() != nil && err != nil:
			return shutdown(log, &wg, cancelRuns)
		case err != nil:
			log.Error("scheduled run not started", "err", err)
		default:
			own.Store(true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer release()
				defer own.Store(false)
				defer func() {
					if p := recover(); p != nil {
						log.Error("panic in scheduled run", "panic", p)
					}
				}()
				runFn(runCtx, ctx.Done(), "schedule")
			}()
		}
		// Never the same time again, even if the clock stepped back.
		next = sched.Next(later(c.now(), fired))
	}
}

// retryLock waits for a lock another pbp command holds, every
// c.retryEvery for up to c.retryFor; ErrBusy if it stays held.
func retryLock(ctx context.Context, log *slog.Logger, c clock, scheduled time.Time) (func(), error) {
	log.Warn("run lock held by another pbp command; retrying", "scheduled", scheduled, "for", c.retryFor)
	deadline := c.now().Add(c.retryFor)
	for c.now().Before(deadline) {
		if !sleep(ctx, c.retryEvery) {
			return nil, ctx.Err()
		}
		release, err := AcquireLock(LockPath())
		if !errors.Is(err, ErrBusy) {
			return release, err
		}
	}
	return nil, ErrBusy
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// shutdown waits for an in-flight run, cancelling it after shutdownGrace.
func shutdown(log *slog.Logger, wg *sync.WaitGroup, cancelRuns context.CancelFunc) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Info("daemon stopped")
		return nil
	default:
	}
	log.Info("shutdown requested; waiting for the active run", "grace", shutdownGrace)
	grace := time.NewTimer(shutdownGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
		log.Warn("grace period over; cancelling the active run")
		cancelRuns()
		<-done
	}
	log.Info("daemon stopped")
	return nil
}
