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
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

// DefaultStateDir holds the state store, the run lock and the daemon start
// time. PBP_STATE_DIR overrides it.
const DefaultStateDir = "/var/lib/pbp"

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

// Serve calls runFn with trigger "schedule" at each time cfg.Schedule names
// (container time zone), holding the run lock for the whole call. A tick
// that finds the lock taken is skipped. When ctx ends, Serve stops
// scheduling and waits for an in-flight run: the run's own context is
// cancelled only after shutdownGrace, so it can finish or tear down.
func Serve(ctx context.Context, cfg config.Config, runFn func(ctx context.Context, trigger string)) error {
	sched, err := cron.ParseStandard(cfg.Schedule)
	if err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}
	log := slog.Default()

	runCtx, cancelRuns := context.WithCancel(context.Background())
	defer cancelRuns()
	var wg sync.WaitGroup

	log.Info("daemon started", "schedule", cfg.Schedule, "next", sched.Next(time.Now()))
	for {
		next := sched.Next(time.Now())
		if next.IsZero() {
			return errors.New("schedule never fires")
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return shutdown(log, &wg, cancelRuns)
		case <-timer.C:
			if ctx.Err() != nil {
				return shutdown(log, &wg, cancelRuns)
			}
		}

		release, err := AcquireLock(LockPath())
		if errors.Is(err, ErrBusy) {
			log.Warn("previous run still active, skipping", "scheduled", next)
			continue
		}
		if err != nil {
			log.Error("scheduled run not started", "err", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer release()
			defer func() {
				if p := recover(); p != nil {
					log.Error("panic in scheduled run", "panic", p)
				}
			}()
			runFn(runCtx, "schedule")
		}()
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
