package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog routes the default logger into a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func TestStateDir(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", "")
	if got := StateDir(); got != "/var/lib/pbp/state" {
		t.Errorf("StateDir = %q", got)
	}
	t.Setenv("PBP_STATE_DIR", "/tmp/x")
	if got := StateDir(); got != "/tmp/x" || LockPath() != "/tmp/x/run.lock" {
		t.Errorf("StateDir = %q, LockPath = %q", got, LockPath())
	}
}

func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "run.lock")
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second AcquireLock err = %v, want ErrBusy", err)
	}
	release()
	release2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}

func TestServeSkipsWhileRunActive(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	buf := captureLog(t)
	var active, maxActive, calls atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	runFn := func(ctx context.Context, _ <-chan struct{}, trigger string) {
		if trigger != "schedule" {
			t.Errorf("trigger = %q", trigger)
		}
		calls.Add(1)
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		// Stay active until a later tick has been skipped.
		eventually(t, func() bool { return strings.Contains(buf.String(), "previous run still active, skipping") }, "a skipped tick")
		active.Add(-1)
		stop() // shut down once the run ends, so no second run starts
	}

	if err := serve(ctx, config.Config{Schedule: "* * * * *"}, runFn, ticking()); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Error("Serve returned while a run was active")
	}
	if maxActive.Load() != 1 || calls.Load() != 1 {
		t.Errorf("max concurrent = %d, calls = %d; want 1, 1", maxActive.Load(), calls.Load())
	}
}

// fakeClock is a wall clock the test sets; the loop wakes every few
// milliseconds to look at it.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration // added at each read, when set
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.t
	f.t = f.t.Add(f.step)
	return t
}
func (f *fakeClock) set(t time.Time) { f.mu.Lock(); f.t = t; f.mu.Unlock() }
func (f *fakeClock) clock() clock {
	return clock{now: f.now, wake: 2 * time.Millisecond, retryEvery: 5 * time.Millisecond, retryFor: time.Hour}
}

// ticking is a clock that reaches the next minute within a few reads, for
// tests that need ticks without waiting for real ones (code review 7.8).
func ticking() clock {
	return (&fakeClock{t: at(1, 59, 50), step: 10 * time.Second}).clock()
}

func at(hh, mm, ss int) time.Time { return time.Date(2026, 10, 22, hh, mm, ss, 0, time.Local) }
func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A run starts when the wall clock reaches its time: a clock that steps
// back after the timer would have fired does not start it early, and the
// same time never runs twice (code review 6.8).
func TestServeFollowsTheWallClock(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	fc := &fakeClock{t: at(1, 59, 0)}
	var calls atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{Schedule: "0 2 * * *"}, func(context.Context, <-chan struct{}, string) { calls.Add(1) }, fc.clock())
	}()
	time.Sleep(50 * time.Millisecond) // many wakes, all before 02:00
	if calls.Load() != 0 {
		t.Fatal("ran before 02:00")
	}
	fc.set(at(2, 0, 1))
	eventually(t, func() bool { return calls.Load() == 1 }, "the 02:00 run")
	fc.set(at(1, 59, 30)) // the clock steps back over 02:00
	time.Sleep(20 * time.Millisecond)
	fc.set(at(2, 0, 30))
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("02:00 ran %d times", n)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A tick that finds the lock held by another pbp command (a pbp check,
// say) waits for it instead of losing the night's run (code review 6.7).
func TestServeRetriesWhenLockHeldElsewhere(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	buf := captureLog(t)
	release, err := AcquireLock(LockPath())
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClock{t: at(1, 59, 59)}
	var calls atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{Schedule: "0 2 * * *"}, func(context.Context, <-chan struct{}, string) { calls.Add(1) }, fc.clock())
	}()
	eventually(t, func() bool { return strings.Contains(buf.String(), "daemon started") }, "the start")
	fc.set(at(2, 0, 1))
	eventually(t, func() bool { return strings.Contains(buf.String(), "retrying") }, "the retry")
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("ran while another process held the lock")
	}
	release()
	eventually(t, func() bool { return calls.Load() == 1 }, "the run after the lock was released")
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "run lock held by another pbp command; retrying") {
		t.Errorf("retry not logged: %s", buf)
	}
}

// If the lock stays held for the whole retry window, the run is skipped.
func TestServeGivesUpWhenLockStaysHeld(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	buf := captureLog(t)
	release, err := AcquireLock(LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fc := &fakeClock{t: at(1, 59, 59)}
	c := fc.clock()
	c.retryFor = 20 * time.Minute
	var calls atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{Schedule: "0 2 * * *"}, func(context.Context, <-chan struct{}, string) { calls.Add(1) }, c)
	}()
	eventually(t, func() bool { return strings.Contains(buf.String(), "daemon started") }, "the start")
	fc.set(at(2, 0, 1))
	eventually(t, func() bool { return strings.Contains(buf.String(), "retrying") }, "the retry")
	fc.set(at(2, 30, 0)) // past the retry window
	eventually(t, func() bool {
		return strings.Contains(buf.String(), "run lock still held by another pbp command; skipping")
	}, "the skip")
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Errorf("ran %d times while another process held the lock", calls.Load())
	}
}

func TestServeHoldsLockDuringRun(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	captureLog(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var lockErr error
	runFn := func(context.Context, <-chan struct{}, string) {
		defer stop()
		release, err := AcquireLock(LockPath())
		if err == nil {
			release()
		}
		lockErr = err
	}
	if err := serve(ctx, config.Config{Schedule: "* * * * *"}, runFn, ticking()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(lockErr, ErrBusy) {
		t.Errorf("lock during run: %v, want ErrBusy", lockErr)
	}
	release, err := AcquireLock(LockPath())
	if err != nil {
		t.Fatalf("lock not released after run: %v", err)
	}
	release()
}

func TestServeShutdownGrace(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	captureLog(t)
	old := shutdownGrace
	shutdownGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = old })

	started := make(chan struct{})
	var runCancelledAfter time.Duration
	var stopAt time.Time
	var mu sync.Mutex
	runFn := func(ctx context.Context, _ <-chan struct{}, _ string) {
		close(started)
		<-ctx.Done()
		mu.Lock()
		runCancelledAfter = time.Since(stopAt)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // teardown
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- serve(ctx, config.Config{Schedule: "* * * * *"}, runFn, ticking()) }()
	<-started
	mu.Lock()
	stopAt = time.Now()
	mu.Unlock()
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if runCancelledAfter < shutdownGrace {
		t.Errorf("run cancelled %v after shutdown, want >= %v", runCancelledAfter, shutdownGrace)
	}
	if since := time.Since(stopAt); since < shutdownGrace+100*time.Millisecond {
		t.Errorf("Serve returned %v after shutdown, before the run's teardown finished", since)
	}
}

func TestServeBadSchedule(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	if err := Serve(context.Background(), config.Config{Schedule: "nope"}, nil); err == nil {
		t.Error("want error")
	}
}

// On shutdown the run is told to stop admitting at once (stop closes), while
// its own context stays live for the grace period, so in-flight proofs can
// finish or tear down (code review I3).
func TestServeSignalsStopBeforeCancelling(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	captureLog(t)
	old := shutdownGrace
	shutdownGrace = 2 * time.Second
	defer func() { shutdownGrace = old }()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan string, 1)
	runFn := func(runCtx context.Context, stop <-chan struct{}, _ string) {
		close(started)
		select {
		case <-stop:
		case <-time.After(5 * time.Second):
			result <- "stop never closed"
			return
		}
		if runCtx.Err() != nil {
			result <- "run context cancelled together with stop"
			return
		}
		<-runCtx.Done() // cancelled only when the grace runs out
		result <- "ok"
	}
	go func() {
		<-started
		cancel()
	}()
	if err := serve(ctx, config.Config{Schedule: "* * * * *"}, runFn, ticking()); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != "ok" {
		t.Error(got)
	}
}
